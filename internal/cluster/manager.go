package cluster

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/danecwalker/buidl/internal/config"
	"github.com/danecwalker/buidl/internal/inventory"
	"github.com/danecwalker/buidl/internal/remote"
)

// Logger is the output surface this package needs.
type Logger interface {
	Info(format string, args ...any)
	Detail(format string, args ...any)
	Warn(format string, args ...any)
	Success(format string, args ...any)
	Step(name string)
	// StepDetail annotates the current step with a short outcome note, shown in
	// the closing summary.
	StepDetail(format string, args ...any)
}

// Manager installs and inspects a cluster.
type Manager struct {
	infra    *config.Infra
	provider inventory.Provider
	distro   Distro
	log      Logger

	// clients caches one SSH connection per host for the lifetime of a command.
	// Cluster bootstrap makes many small calls per machine, and re-handshaking
	// each time roughly doubles the wall clock.
	mu      sync.Mutex
	clients map[string]*remote.Client
}

// New builds a Manager for the configured infrastructure.
func New(infra *config.Infra, log Logger) (*Manager, error) {
	if infra == nil {
		return nil, fmt.Errorf("no `infra` block in buidl.yaml\n\n" +
			"hint: add one to let buidl install Kubernetes on your servers:\n" +
			"  infra:\n    servers:\n      - host: 10.0.0.1\n        role: control-plane")
	}

	distro, err := DistroFor(infra.Kubernetes.Distribution)
	if err != nil {
		return nil, err
	}

	return &Manager{
		infra:    infra,
		provider: infra.InventoryProvider(),
		distro:   distro,
		log:      log,
		clients:  map[string]*remote.Client{},
	}, nil
}

// Distro reports the distribution driver in use.
func (m *Manager) Distro() Distro { return m.distro }

// Close releases every cached SSH connection.
func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.clients {
		_ = c.Close()
	}
	m.clients = map[string]*remote.Client{}
	return nil
}

// connect returns a cached or new SSH client for a machine.
func (m *Manager) connect(ctx context.Context, server inventory.Server) (*remote.Client, error) {
	m.mu.Lock()
	if c, ok := m.clients[server.Host]; ok {
		m.mu.Unlock()
		return c, nil
	}
	m.mu.Unlock()

	cfg := remote.Config{
		Host:              server.Host,
		Port:              orDefaultInt(server.Port, m.infra.SSH.Port),
		User:              orDefaultString(server.User, m.infra.SSH.User),
		KeyPath:           m.infra.SSH.KeyPath,
		KnownHosts:        m.infra.SSH.KnownHosts,
		AcceptNewHostKeys: m.infra.SSH.AcceptNewHostKeys,
		Sudo:              m.infra.SSH.Sudo,
	}

	client, err := remote.Dial(ctx, cfg)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	// Another goroutine may have connected first; keep one and close the extra.
	if existing, ok := m.clients[server.Host]; ok {
		m.mu.Unlock()
		_ = client.Close()
		return existing, nil
	}
	m.clients[server.Host] = client
	m.mu.Unlock()

	return client, nil
}

// NodeOutcome records what happened to one machine during convergence.
type NodeOutcome struct {
	Host     string
	Role     inventory.Role
	Action   Action
	Duration time.Duration
	// Err is the failure, when the change did not succeed.
	Err error
}

// OK reports whether the change succeeded.
func (o NodeOutcome) OK() bool { return o.Err == nil }

// Result is the account of a convergence run.
type Result struct {
	Outcomes []NodeOutcome
	Duration time.Duration
}

// Succeeded returns the machines that were changed successfully.
func (r *Result) Succeeded() []NodeOutcome {
	var out []NodeOutcome
	for _, o := range r.Outcomes {
		if o.OK() {
			out = append(out, o)
		}
	}
	return out
}

// Failed returns the machines whose change failed.
func (r *Result) Failed() []NodeOutcome {
	var out []NodeOutcome
	for _, o := range r.Outcomes {
		if !o.OK() {
			out = append(out, o)
		}
	}
	return out
}

// record appends an outcome, timing it from start.
func (r *Result) record(node NodePlan, start time.Time, err error) {
	r.Outcomes = append(r.Outcomes, NodeOutcome{
		Host:     node.Server.Host,
		Role:     node.Role,
		Action:   node.Action,
		Duration: time.Since(start),
		Err:      err,
	})
}

// Converge brings the cluster to the planned state.
//
// Ordering is dictated by etcd, not convenience:
//
//  1. The bootstrap control plane must exist and be serving before anything can
//     join it.
//  2. Additional control planes are changed one at a time. Adding or restarting
//     two etcd members concurrently can leave the cluster without a quorum.
//  3. Workers are changed concurrently; they hold no cluster state, so there is
//     nothing to serialize.
func (m *Manager) Converge(ctx context.Context, plan *Plan) (*Result, error) {
	started := time.Now()
	result := &Result{}

	if !plan.HasChanges() {
		m.log.Success("cluster is already up to date (%s)", plan.Inventory.Summary())
		return result, nil
	}

	// Bootstrap and reconfiguration of the bootstrap node.
	for _, node := range plan.Nodes {
		if node.Server.Host != plan.Bootstrap.Host {
			continue
		}
		switch node.Action {
		case ActionBootstrap:
			m.log.Step(fmt.Sprintf("Initializing cluster on %s", node.Server.Host))
			start := time.Now()

			if err := m.installNode(ctx, node, plan); err != nil {
				result.record(node, start, err)
				return result, err
			}
			if err := m.waitForAPI(ctx, node.Server, 5*time.Minute); err != nil {
				result.record(node, start, err)
				return result, err
			}
			// The distribution may have replaced our token during bootstrap; read
			// back the authoritative one before any node joins with it.
			token, err := m.readToken(ctx, node.Server)
			if err != nil {
				err = fmt.Errorf("reading the join token after bootstrap: %w", err)
				result.record(node, start, err)
				return result, err
			}
			if token != "" && token != plan.Token {
				m.log.Detail("using the token generated during bootstrap")
				plan.Token = token
				// Re-render every pending node config against the real token.
				if err := rerenderPending(plan); err != nil {
					result.record(node, start, err)
					return result, err
				}
			}

			result.record(node, start, nil)
			m.log.Success("cluster initialized on %s", node.Server.Host)

		case ActionReconfigure, ActionUpgrade:
			m.log.Step(fmt.Sprintf("%s %s", verbFor(node.Action), node.Server.Host))
			start := time.Now()

			if err := m.changeNode(ctx, node, plan); err != nil {
				result.record(node, start, err)
				return result, err
			}
			if err := m.waitForAPI(ctx, node.Server, 5*time.Minute); err != nil {
				result.record(node, start, err)
				return result, err
			}

			result.record(node, start, nil)
			m.log.Success("%s %s", node.Server.Host, pastTenseFor(node.Action))
		}
	}

	// Additional control planes, strictly one at a time.
	for _, node := range plan.Nodes {
		if node.Server.Host == plan.Bootstrap.Host || node.Role != inventory.RoleControlPlane {
			continue
		}
		switch node.Action {
		case ActionJoinControlPlane:
			m.log.Step(fmt.Sprintf("Joining control plane %s", node.Server.Host))
			start := time.Now()

			if err := m.installNode(ctx, node, plan); err != nil {
				result.record(node, start, err)
				return result, err
			}
			// Waiting for each member to serve before touching the next keeps
			// etcd's quorum well-defined throughout.
			if err := m.waitForAPI(ctx, node.Server, 5*time.Minute); err != nil {
				result.record(node, start, err)
				return result, err
			}

			result.record(node, start, nil)
			m.log.Success("%s joined the control plane", node.Server.Host)

		case ActionReconfigure, ActionUpgrade:
			m.log.Step(fmt.Sprintf("%s %s", verbFor(node.Action), node.Server.Host))
			start := time.Now()

			if err := m.changeNode(ctx, node, plan); err != nil {
				result.record(node, start, err)
				return result, err
			}
			if err := m.waitForAPI(ctx, node.Server, 5*time.Minute); err != nil {
				result.record(node, start, err)
				return result, err
			}

			result.record(node, start, nil)
			m.log.Success("%s %s", node.Server.Host, pastTenseFor(node.Action))
		}
	}

	// Workers, concurrently.
	var workers []NodePlan
	for _, node := range plan.Nodes {
		if node.Role == inventory.RoleControlPlane {
			continue
		}
		switch node.Action {
		case ActionJoinWorker, ActionReconfigure, ActionUpgrade:
			workers = append(workers, node)
		}
	}

	if len(workers) > 0 {
		m.log.Step(fmt.Sprintf("Changing %d worker(s)", len(workers)))
		err := m.joinWorkers(ctx, workers, plan, result)
		result.Duration = time.Since(started)
		if err != nil {
			return result, err
		}
	}

	result.Duration = time.Since(started)
	return result, nil
}

// pastTenseFor renders an action as a completed verb.
func pastTenseFor(action Action) string {
	if action == ActionUpgrade {
		return "upgraded"
	}
	return "reconfigured"
}

// joinWorkers installs workers concurrently and aggregates failures.
//
// One bad machine should not hide the outcome for the rest, so every result is
// collected before reporting.
func (m *Manager) joinWorkers(ctx context.Context, workers []NodePlan, plan *Plan, result *Result) error {
	type outcome struct {
		node     NodePlan
		start    time.Time
		duration time.Duration
		err      error
	}

	results := make(chan outcome, len(workers))
	var wg sync.WaitGroup

	for _, node := range workers {
		wg.Add(1)
		go func(n NodePlan) {
			defer wg.Done()
			start := time.Now()
			var err error
			if n.Action == ActionJoinWorker {
				err = m.installNode(ctx, n, plan)
			} else {
				err = m.changeNode(ctx, n, plan)
			}
			results <- outcome{node: n, start: start, duration: time.Since(start), err: err}
		}(node)
	}

	wg.Wait()
	close(results)

	// Collect every result before reporting, so one bad machine does not hide the
	// outcome for the rest.
	collected := make([]outcome, 0, len(workers))
	for r := range results {
		collected = append(collected, r)
	}
	// Concurrent completion order is arbitrary; sort so the report is stable.
	sort.Slice(collected, func(i, j int) bool {
		return collected[i].node.Server.Host < collected[j].node.Server.Host
	})

	var failures []string
	for _, r := range collected {
		result.record(r.node, r.start, r.err)
		if r.err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", r.node.Server.Host, r.err))
			m.log.Warn("%s failed after %s", r.node.Server.Host, r.duration.Round(time.Second))
			continue
		}
		verb := "joined"
		if r.node.Action != ActionJoinWorker {
			verb = pastTenseFor(r.node.Action)
		}
		m.log.Success("%s %s (%s)", r.node.Server.Host, verb, r.duration.Round(time.Second))
	}

	if len(failures) > 0 {
		return fmt.Errorf("%d of %d worker(s) failed:\n  - %s",
			len(failures), len(workers), strings.Join(failures, "\n  - "))
	}
	return nil
}

// installNode writes the config and runs the installer on one machine.
func (m *Manager) installNode(ctx context.Context, node NodePlan, plan *Plan) error {
	client, err := m.connect(ctx, node.Server)
	if err != nil {
		return err
	}

	// The config file contains the join token, so it must never be world-readable.
	if err := client.WriteFile(ctx, m.distro.ConfigPath(), node.Config, "0600"); err != nil {
		return fmt.Errorf("writing %s: %w", m.distro.ConfigPath(), err)
	}
	m.log.Detail("%s: wrote %s", node.Server.Host, m.distro.ConfigPath())

	install := m.distro.InstallCommand(node.Role, plan.Kubernetes.Version)
	m.log.Detail("%s: installing %s", node.Server.Host, m.distro.Name())
	if _, err := client.Sudo(ctx, install); err != nil {
		return fmt.Errorf("installing %s on %s: %w", m.distro.Name(), node.Server.Host, err)
	}

	// RKE2 does not start its unit from the installer; k3s does.
	if start := m.distro.StartCommand(node.Role); start != "" {
		if _, err := client.Sudo(ctx, start); err != nil {
			return fmt.Errorf("starting %s on %s: %w", m.distro.Name(), node.Server.Host, err)
		}
	}

	return nil
}

// changeNode applies a reconfigure or an upgrade to an existing member.
//
// An upgrade re-runs the installer with the new pinned version, which is the
// distributions' documented in-place upgrade path; a reconfigure only rewrites
// the config file and restarts the unit.
func (m *Manager) changeNode(ctx context.Context, node NodePlan, plan *Plan) error {
	client, err := m.connect(ctx, node.Server)
	if err != nil {
		return err
	}

	if err := client.WriteFile(ctx, m.distro.ConfigPath(), node.Config, "0600"); err != nil {
		return err
	}

	if node.Action == ActionUpgrade {
		install := m.distro.InstallCommand(node.Role, plan.Kubernetes.Version)
		if _, err := client.Sudo(ctx, install); err != nil {
			return fmt.Errorf("upgrading %s on %s: %w", m.distro.Name(), node.Server.Host, err)
		}
		// The installer replaces the binary and restarts the unit itself on k3s;
		// RKE2 needs the explicit restart below.
	}

	service := m.distro.ServiceName(node.Role)
	if _, err := client.Sudo(ctx, "systemctl restart "+remote.Quote(service)); err != nil {
		return fmt.Errorf("restarting %s on %s: %w", service, node.Server.Host, err)
	}
	return nil
}

// verbFor renders an action as a progress verb.
func verbFor(action Action) string {
	if action == ActionUpgrade {
		return "Upgrading"
	}
	return "Reconfiguring"
}

// waitForAPI blocks until a control-plane node serves a ready API.
//
// This gate is what makes joins reliable: registering against an API server that
// is still starting fails with a connection error that looks like a
// misconfiguration.
func (m *Manager) waitForAPI(ctx context.Context, server inventory.Server, timeout time.Duration) error {
	client, err := m.connect(ctx, server)
	if err != nil {
		return err
	}

	deadline := time.Now().Add(timeout)
	probe := m.distro.KubectlCommand() + " get --raw /readyz"

	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	var lastErr string
	for {
		res, err := client.TrySudo(ctx, probe)
		if err == nil && res.ExitCode == 0 && strings.Contains(res.Stdout, "ok") {
			return nil
		}
		if res != nil && strings.TrimSpace(res.Stderr) != "" {
			lastErr = firstLine(res.Stderr)
		}

		if time.Now().After(deadline) {
			msg := fmt.Sprintf("the API server on %s did not become ready within %s", server.Host, timeout)
			if lastErr != "" {
				msg += "\n\nlast error: " + lastErr
			}
			service := m.distro.ServiceName(inventory.RoleControlPlane)
			msg += fmt.Sprintf("\n\nhint: check the service on the host:\n  ssh %s 'journalctl -u %s -n 50 --no-pager'", server.Host, service)
			return fmt.Errorf("%s", msg)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// rerenderPending re-renders node configs after the token changed.
func rerenderPending(plan *Plan) error {
	for i := range plan.Nodes {
		node := &plan.Nodes[i]
		if node.Action == ActionUpToDate || node.Action == ActionBootstrap {
			continue
		}
		rendered, err := renderNodeConfig(node.Server, node.Role, false, plan)
		if err != nil {
			return err
		}
		node.Config = rendered
	}
	return nil
}

// generateToken produces a cluster join secret.
//
// Generating it locally lets every node's config be written before bootstrap
// finishes, rather than requiring a second pass to distribute a token the
// distribution chose.
func generateToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating a cluster token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

func orDefaultInt(v, fallback int) int {
	if v == 0 {
		return fallback
	}
	return v
}

func orDefaultString(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
