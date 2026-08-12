package cluster

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/danewalker/buidl/internal/config"
	"github.com/danewalker/buidl/internal/inventory"
	"github.com/danewalker/buidl/internal/remote"
)

// Action is what needs to happen to one machine.
type Action string

const (
	// ActionBootstrap installs the first control plane and creates the cluster.
	ActionBootstrap Action = "bootstrap"
	// ActionJoinControlPlane adds a control-plane member to an existing cluster.
	ActionJoinControlPlane Action = "join-control-plane"
	// ActionJoinWorker adds a worker.
	ActionJoinWorker Action = "join-worker"
	// ActionReconfigure rewrites the node config and restarts the service on a
	// machine that is already a member.
	ActionReconfigure Action = "reconfigure"
	// ActionUpgrade re-runs the installer on a member whose installed version no
	// longer matches the configured one.
	//
	// Detecting this matters: the version is not part of the node config file, so
	// without an explicit check a changed `kubernetes.version` would compare equal
	// and be silently reported as up to date.
	ActionUpgrade Action = "upgrade"
	// ActionUpToDate means the machine is a healthy member with matching config.
	ActionUpToDate Action = "up-to-date"
	// ActionSkipped means the machine could not be planned for: unreachable, not
	// Linux, or without systemd. Reporting this distinctly matters — calling an
	// unreachable server "up-to-date" would claim the cluster matches the
	// configuration when in truth nothing is known about it.
	ActionSkipped Action = "skipped"
)

// Facts describe a machine's current state, gathered before any change.
type Facts struct {
	// Reachable reports whether SSH succeeded. Unreachable machines are reported
	// together rather than failing the run on the first one, so a typo in the
	// fifth address does not hide a firewall problem on the second.
	Reachable bool
	// Error explains an unreachable machine.
	Error error

	OS       string
	Arch     string
	Hostname string
	// HasSystemd reports whether systemd is the init system. Both distributions
	// require it.
	HasSystemd bool

	// Installed reports whether the distribution's config file is present.
	Installed bool
	// ServiceActive reports whether the node's systemd unit is running.
	ServiceActive bool
	// CurrentVersion is the installed distribution version, when detectable.
	CurrentVersion string

	// Firewall is the active host firewall, if any. Stock cloud images commonly
	// ship one allowing only SSH, which blocks the API server and ingress while
	// leaving SSH — and therefore the whole install — working perfectly.
	Firewall FirewallKind
}

// NodePlan is the planned change for one machine.
type NodePlan struct {
	Server inventory.Server
	Role   inventory.Role
	Action Action
	Facts  Facts
	// Reason explains the action in one line.
	Reason string
	// Config is the node config file that will be written.
	Config string
}

// Plan is the full set of changes converging the cluster would make.
type Plan struct {
	Distro     Distro
	Kubernetes config.ClusterKubernetes
	Inventory  *inventory.Inventory

	// Bootstrap is the machine that creates the cluster.
	Bootstrap inventory.Server
	// BootstrapExists reports whether a cluster is already running, in which case
	// the bootstrap node is joined rather than initialized.
	BootstrapExists bool

	// RegistrationAddress is the URL joining nodes use.
	RegistrationAddress string
	// TLSSANs are the names the API server certificate must cover.
	TLSSANs []string
	// Token is the cluster join secret: read from the bootstrap node when a
	// cluster already exists, supplied by config, or generated.
	Token string

	Nodes    []NodePlan
	Warnings []string
}

// Changes returns the nodes that need work.
func (p *Plan) Changes() []NodePlan {
	var out []NodePlan
	for _, n := range p.Nodes {
		if n.Action != ActionUpToDate && n.Action != ActionSkipped {
			out = append(out, n)
		}
	}
	return out
}

// HasChanges reports whether anything would change.
func (p *Plan) HasChanges() bool {
	return len(p.Changes()) > 0
}

// Skipped returns the nodes that could not be planned for.
func (p *Plan) Skipped() []NodePlan {
	var out []NodePlan
	for _, n := range p.Nodes {
		if n.Action == ActionSkipped {
			out = append(out, n)
		}
	}
	return out
}

// Actionable reports whether any machine was successfully inspected. When false,
// the plan says nothing about the cluster's real state.
//
// An empty fleet is actionable: there is nothing to be uncertain about. Validation
// rejects a config with no servers before this is reached, so the case only arises
// in tests, but returning false here would turn "nothing configured" into
// "cluster state unknown".
func (p *Plan) Actionable() bool {
	if len(p.Nodes) == 0 {
		return true
	}
	return len(p.Skipped()) < len(p.Nodes)
}

// Unreachable returns the machines that could not be contacted.
func (p *Plan) Unreachable() []NodePlan {
	var out []NodePlan
	for _, n := range p.Nodes {
		if !n.Facts.Reachable {
			out = append(out, n)
		}
	}
	return out
}

// gatherFacts probes every machine concurrently.
//
// Concurrency matters here: a ten-machine fleet with a 15-second SSH timeout
// would otherwise take two and a half minutes just to discover that one address
// is wrong.
func (m *Manager) gatherFacts(ctx context.Context, inv *inventory.Inventory) map[string]Facts {
	facts := make(map[string]Facts, len(inv.Servers))
	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, server := range inv.Servers {
		wg.Add(1)
		go func(s inventory.Server) {
			defer wg.Done()
			f := m.probe(ctx, s)
			mu.Lock()
			facts[s.Host] = f
			mu.Unlock()
		}(server)
	}

	wg.Wait()
	return facts
}

// probe gathers facts from one machine.
func (m *Manager) probe(ctx context.Context, server inventory.Server) Facts {
	client, err := m.connect(ctx, server)
	if err != nil {
		return Facts{Reachable: false, Error: err}
	}
	// Not closed here: connect returns a cached client owned by the Manager, and
	// closing it would leave a dead socket in the cache for every later step.

	f := Facts{Reachable: true}

	// One round trip for the cheap facts; each is on its own line.
	if res, err := client.Run(ctx, "uname -s; uname -m; hostname"); err == nil {
		lines := strings.Split(strings.TrimSpace(res.Stdout), "\n")
		if len(lines) > 0 {
			f.OS = strings.TrimSpace(lines[0])
		}
		if len(lines) > 1 {
			f.Arch = strings.TrimSpace(lines[1])
		}
		if len(lines) > 2 {
			f.Hostname = strings.TrimSpace(lines[2])
		}
	}

	if res, err := client.Try(ctx, "command -v systemctl"); err == nil {
		f.HasSystemd = res.ExitCode == 0
	}

	// The config file's presence is a better installed-signal than the binary:
	// the binary can exist from a failed or partial run.
	if installed, err := client.Exists(ctx, m.distro.ConfigPath()); err == nil {
		f.Installed = installed
	}

	service := m.distro.ServiceName(server.Role)
	if res, err := client.Try(ctx, "systemctl is-active --quiet "+remote.Quote(service)); err == nil {
		f.ServiceActive = res.ExitCode == 0
	}

	f.Firewall = detectFirewall(ctx, client)

	if f.Installed {
		if res, err := client.Try(ctx, m.distro.Name()+" --version"); err == nil && res.ExitCode == 0 {
			f.CurrentVersion = firstLine(res.Stdout)
		}
	}

	return f
}

// Plan computes what convergence would do, without changing anything.
func (m *Manager) Plan(ctx context.Context) (*Plan, error) {
	inv, err := m.provider.Resolve(ctx)
	if err != nil {
		return nil, err
	}

	bootstrap, err := inv.Bootstrap()
	if err != nil {
		return nil, err
	}

	plan := &Plan{
		Distro:              m.distro,
		Kubernetes:          m.infra.Kubernetes,
		Inventory:           inv,
		Bootstrap:           bootstrap,
		RegistrationAddress: registrationAddress(m.infra.Kubernetes, bootstrap),
		TLSSANs:             tlsSANs(m.infra.Kubernetes, inv),
	}

	if w := inv.QuorumWarning(); w != "" {
		plan.Warnings = append(plan.Warnings, w)
	}

	m.log.Step(fmt.Sprintf("Inspecting %d server(s)", len(inv.Servers)))
	facts := m.gatherFacts(ctx, inv)

	// Establish the token before rendering node configs, since every config
	// embeds it.
	plan.BootstrapExists = facts[bootstrap.Host].Installed && facts[bootstrap.Host].ServiceActive
	token, err := m.resolveToken(ctx, plan)
	if err != nil {
		return nil, err
	}
	plan.Token = token

	for _, server := range inv.Servers {
		f := facts[server.Host]
		isBootstrap := server.Host == bootstrap.Host

		node := NodePlan{Server: server, Role: server.Role, Facts: f}

		switch {
		case !f.Reachable:
			node.Action = ActionSkipped
			// The reason a machine could not be reached — bad credentials, a
			// closed port, an unknown host key — is the entire content of this
			// row. Reporting a bare "unreachable" leaves the user with nothing to
			// act on.
			node.Reason = "unreachable"
			if f.Error != nil {
				node.Reason = firstLine(f.Error.Error())
			}

		case f.OS != "" && f.OS != "Linux":
			node.Action = ActionSkipped
			node.Reason = fmt.Sprintf("unsupported OS %q; Kubernetes nodes must run Linux", f.OS)
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("%s runs %s and will be skipped", server.Host, f.OS))

		case !f.HasSystemd:
			node.Action = ActionSkipped
			node.Reason = "systemd not found; both k3s and RKE2 require it"
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("%s has no systemd and will be skipped", server.Host))

		case f.Installed && f.ServiceActive:
			// Already a member. Rewrite config only if it would differ.
			//
			// The bootstrap machine keeps cluster-init and no `server` entry for the
			// life of the cluster — that is what it is. Re-rendering it as a joiner
			// once the cluster exists would differ from what is on disk on every
			// run, so the plan would never reach a steady state and each deploy
			// would needlessly restart the API server.
			rendered, err := renderNodeConfig(server, server.Role, isBootstrap, plan)
			if err != nil {
				return nil, err
			}
			changed, err := m.configDiffers(ctx, server, rendered)
			if err != nil {
				return nil, err
			}
			switch {
			case versionDrifted(f.CurrentVersion, m.infra.Kubernetes.Version):
				node.Action = ActionUpgrade
				node.Reason = fmt.Sprintf("installed %s, configured %s",
					orUnknownVersion(f.CurrentVersion), m.infra.Kubernetes.Version)
			case changed:
				node.Action = ActionReconfigure
				node.Reason = "node configuration changed"
			default:
				node.Action = ActionUpToDate
				node.Reason = fmt.Sprintf("already joined (%s)", orUnknownVersion(f.CurrentVersion))
			}
			node.Config = rendered

		case isBootstrap && !plan.BootstrapExists:
			node.Action = ActionBootstrap
			node.Reason = "will initialize the cluster"

		case server.Role == inventory.RoleControlPlane:
			node.Action = ActionJoinControlPlane
			node.Reason = "will join as a control-plane member"

		default:
			node.Action = ActionJoinWorker
			node.Reason = "will join as a worker"
		}

		// Render the config for any node that will be written to.
		if node.Config == "" && node.Action != ActionUpToDate {
			rendered, err := renderNodeConfig(server, server.Role, node.Action == ActionBootstrap, plan)
			if err != nil {
				return nil, err
			}
			node.Config = rendered
		}

		plan.Nodes = append(plan.Nodes, node)
	}

	// An active host firewall is reported per machine. It does not block the
	// install — which is exactly why it needs saying: everything succeeds and the
	// cluster is then unreachable.
	for _, node := range plan.Nodes {
		if warning := firewallWarning(m.infra, inv, node); warning != "" {
			plan.Warnings = append(plan.Warnings, warning)
		}
	}

	if unreachable := plan.Unreachable(); len(unreachable) > 0 {
		hosts := make([]string, 0, len(unreachable))
		for _, n := range unreachable {
			hosts = append(hosts, n.Server.Host)
		}
		plan.Warnings = append(plan.Warnings,
			fmt.Sprintf("%d server(s) unreachable: %s", len(hosts), strings.Join(hosts, ", ")))
	}

	return plan, nil
}

// configDiffers reports whether the rendered config differs from what is on disk.
//
// Comparing before writing avoids restarting a healthy node's service on every
// convergence, which would otherwise cause a brief API server outage per run.
func (m *Manager) configDiffers(ctx context.Context, server inventory.Server, rendered string) (bool, error) {
	client, err := m.connect(ctx, server)
	if err != nil {
		// Unreachable machines are handled by the caller.
		return false, nil
	}

	current, err := client.ReadFile(ctx, m.distro.ConfigPath())
	if err != nil {
		// Unreadable means we should write it.
		return true, nil
	}
	return normalizeConfig(current) != normalizeConfig(rendered), nil
}

// normalizeConfig prepares a node config for comparison.
//
// Comments and blank lines are dropped so a header change alone does not look
// like a configuration change.
//
// The token is dropped too, and that is the important part. A distribution
// rewrites the join token it was given into a fuller derived form — k3s turns a
// plain secret into "K10<ca-hash>::server:<secret>" — so the value buidl reads
// back never equals the value it wrote. Comparing them would mark the bootstrap
// node as needing reconfiguration on every single run, restarting the API server
// each time and never reaching a steady state. Token rotation is a deliberate,
// separate operation, not something to infer from a diff.
func normalizeConfig(s string) string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, "token:") {
			continue
		}
		out = append(out, trimmed)
	}
	return strings.Join(out, "\n")
}

// resolveToken determines the cluster join token.
func (m *Manager) resolveToken(ctx context.Context, plan *Plan) (string, error) {
	// An existing cluster's token is authoritative: a different one would make
	// joins fail with an opaque TLS error.
	if plan.BootstrapExists {
		token, err := m.readToken(ctx, plan.Bootstrap)
		if err != nil {
			return "", fmt.Errorf("reading the join token from %s: %w", plan.Bootstrap.Host, err)
		}
		if token != "" {
			return token, nil
		}
	}

	if m.infra.Kubernetes.Token != "" {
		return m.infra.Kubernetes.Token, nil
	}

	// Fresh cluster: generate a token rather than letting the distribution pick
	// one, so every node's config can be written before bootstrap completes.
	return generateToken()
}

// readToken fetches the join token from a running control-plane node.
func (m *Manager) readToken(ctx context.Context, server inventory.Server) (string, error) {
	client, err := m.connect(ctx, server)
	if err != nil {
		return "", err
	}

	token, err := client.ReadFile(ctx, m.distro.TokenPath())
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(token), nil
}

func firstLine(s string) string {
	if i := strings.Index(s, "\n"); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

// versionDrifted reports whether an installed version differs from the pinned
// one.
//
// An unpinned config (empty want) never drifts: the user asked for "current
// stable", so reinstalling on every run would be churn, not convergence. An
// undetectable installed version is likewise treated as no drift, since acting on
// a failed probe would restart healthy nodes.
func versionDrifted(installed, want string) bool {
	if want == "" || installed == "" {
		return false
	}
	// `k3s --version` prints "k3s version v1.34.1+k3s1 (hash)", so a substring
	// check is the right comparison against a pinned "v1.34.1+k3s1".
	return !strings.Contains(installed, want)
}

func orUnknownVersion(v string) string {
	if v == "" {
		return "version unknown"
	}
	return v
}
