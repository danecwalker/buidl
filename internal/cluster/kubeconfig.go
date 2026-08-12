package cluster

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/danewalker/buidl/internal/inventory"
	"github.com/danewalker/buidl/internal/remote"
)

// Kubeconfig fetches the admin kubeconfig from a control-plane node and rewrites
// it to be usable from here.
//
// The file the distribution writes on-node points at 127.0.0.1, which works only
// on that machine. The rewrite substitutes an address reachable from the caller:
// the configured control-plane endpoint if there is one, otherwise the node's own
// address.
func (m *Manager) Kubeconfig(ctx context.Context, contextName string) (*clientcmdapi.Config, error) {
	inv, err := m.provider.Resolve(ctx)
	if err != nil {
		return nil, err
	}
	bootstrap, err := inv.Bootstrap()
	if err != nil {
		return nil, err
	}

	client, err := m.connect(ctx, bootstrap)
	if err != nil {
		return nil, err
	}

	raw, err := client.ReadFile(ctx, m.distro.KubeconfigPath())
	if err != nil {
		return nil, fmt.Errorf("reading the kubeconfig from %s: %w\n\nhint: has the cluster been created? run `buidl deploy`, which installs it if needed", bootstrap.Host, err)
	}

	cfg, err := clientcmd.Load([]byte(raw))
	if err != nil {
		return nil, fmt.Errorf("parsing the kubeconfig from %s: %w", bootstrap.Host, err)
	}

	// Point at an address that resolves from this machine.
	endpoint := m.infra.Kubernetes.ControlPlaneEndpoint
	if endpoint == "" {
		endpoint = bootstrap.Host
	}
	server := fmt.Sprintf("https://%s:6443", endpoint)
	for _, clusterEntry := range cfg.Clusters {
		clusterEntry.Server = server
	}

	if contextName != "" {
		renameContext(cfg, contextName)
	}

	return cfg, nil
}

// renameContext renames the single context, cluster and user to a stable name.
//
// Both distributions name everything "default", which collides the moment you
// manage a second cluster. Naming it after the app and environment makes
// `kubectl config get-contexts` readable and lets deploy.kubernetes.context act
// as a real guardrail.
func renameContext(cfg *clientcmdapi.Config, name string) {
	newClusters := map[string]*clientcmdapi.Cluster{}
	for _, v := range cfg.Clusters {
		newClusters[name] = v
	}
	cfg.Clusters = newClusters

	newAuth := map[string]*clientcmdapi.AuthInfo{}
	for _, v := range cfg.AuthInfos {
		newAuth[name] = v
	}
	cfg.AuthInfos = newAuth

	newContexts := map[string]*clientcmdapi.Context{}
	for _, v := range cfg.Contexts {
		v.Cluster = name
		v.AuthInfo = name
		newContexts[name] = v
	}
	cfg.Contexts = newContexts
	cfg.CurrentContext = name
}

// MergeKubeconfig merges a fetched config into the local kubeconfig file.
//
// Merging rather than overwriting is essential: clobbering ~/.kube/config would
// destroy every other cluster the user has access to.
func MergeKubeconfig(fetched *clientcmdapi.Config, path string, setCurrent bool) (string, error) {
	if path == "" {
		// Honor KUBECONFIG's first entry, which is where kubectl would write.
		if env := os.Getenv("KUBECONFIG"); env != "" {
			path = filepath.SplitList(env)[0]
		} else {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", fmt.Errorf("locating the home directory: %w", err)
			}
			path = filepath.Join(home, ".kube", "config")
		}
	}

	existing, err := clientcmd.LoadFromFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("reading %s: %w", path, err)
		}
		existing = clientcmdapi.NewConfig()
	}

	for name, v := range fetched.Clusters {
		existing.Clusters[name] = v
	}
	for name, v := range fetched.AuthInfos {
		existing.AuthInfos[name] = v
	}
	for name, v := range fetched.Contexts {
		existing.Contexts[name] = v
	}
	if setCurrent && fetched.CurrentContext != "" {
		existing.CurrentContext = fetched.CurrentContext
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	if err := clientcmd.WriteToFile(*existing, path); err != nil {
		return "", fmt.Errorf("writing %s: %w", path, err)
	}
	// A kubeconfig holds cluster-admin credentials.
	if err := os.Chmod(path, 0o600); err != nil {
		return "", fmt.Errorf("securing %s: %w", path, err)
	}

	return path, nil
}

// ContextExists reports whether a context is present in the local kubeconfig.
//
// Callers use this to decide whether credentials still need fetching. A cluster
// being up to date says nothing about whether *this* machine can reach it: a CI
// runner starts with no kubeconfig at all, and a run that failed after installing
// leaves the cluster healthy but the credentials unfetched.
// The answer must come from the same merged view the deploy client builds from.
// The client uses NewDefaultClientConfigLoadingRules, which merges every entry in
// KUBECONFIG; reading only the first file makes a context living in a later entry
// invisible, so buidl re-fetches credentials on every run and never adopts the
// context it already has.
func ContextExists(name string) bool {
	if name == "" {
		return false
	}

	cfg, err := localKubeconfig()
	if err != nil {
		return false
	}
	_, ok := cfg.Contexts[name]
	return ok
}

// CurrentContext reports the context the local kubeconfig selects, or "" when
// there is none.
//
// Used where buidl must name the cluster it is about to address without an
// explicit context configured — a prompt that cannot say which cluster it means
// is worse than no prompt.
func CurrentContext() string {
	cfg, err := localKubeconfig()
	if err != nil {
		return ""
	}
	return cfg.CurrentContext
}

// localKubeconfig loads the merged kubeconfig exactly as the deploy client does.
func localKubeconfig() (*clientcmdapi.Config, error) {
	return clientcmd.NewDefaultClientConfigLoadingRules().Load()
}

// NodeStatus is one node as the cluster reports it.
type NodeStatus struct {
	Name    string
	Status  string
	Roles   string
	Version string
	// Host is the inventory address, when it can be matched to a node.
	Host string
	Role inventory.Role
	// ServiceActive reports the systemd unit state, which explains a node that
	// the cluster cannot see at all.
	ServiceActive bool
	// Reachable reports whether SSH succeeded.
	Reachable bool
	// Message explains an unreachable or unhealthy node.
	Message string
}

// Status describes the cluster as it currently is.
type Status struct {
	Distribution string
	Endpoint     string
	// Reachable reports whether the API server answered.
	Reachable bool
	Summary   string
	Nodes     []NodeStatus
	// Warnings covers topology problems such as an unsafe control-plane count.
	Warnings []string
}

// Status inspects the cluster.
func (m *Manager) Status(ctx context.Context) (*Status, error) {
	inv, err := m.provider.Resolve(ctx)
	if err != nil {
		return nil, err
	}

	st := &Status{
		Distribution: m.distro.Name(),
		Endpoint:     m.infra.Kubernetes.ControlPlaneEndpoint,
		Summary:      inv.Summary(),
	}
	if w := inv.QuorumWarning(); w != "" {
		st.Warnings = append(st.Warnings, w)
	}

	facts := m.gatherFacts(ctx, inv)

	// Build a row per inventory machine, so a server that never joined still
	// appears. Reporting only what the API knows would silently omit exactly the
	// machines the user needs to hear about.
	byName := map[string]NodeStatus{}
	bootstrap, err := inv.Bootstrap()
	if err == nil {
		if nodes, err := m.queryNodes(ctx, bootstrap); err == nil {
			st.Reachable = true
			for _, n := range nodes {
				byName[n.Name] = n
			}
		}
	}

	for _, server := range inv.Servers {
		f := facts[server.Host]
		row := NodeStatus{
			Host:          server.Host,
			Role:          server.Role,
			Reachable:     f.Reachable,
			ServiceActive: f.ServiceActive,
			Name:          server.Name,
			Status:        "unknown",
		}
		if row.Name == "" {
			row.Name = f.Hostname
		}
		if !f.Reachable {
			row.Status = "unreachable"
			if f.Error != nil {
				row.Message = firstLine(f.Error.Error())
			}
		} else if reported, ok := byName[row.Name]; ok {
			row.Status = reported.Status
			row.Roles = reported.Roles
			row.Version = reported.Version
		} else if !f.Installed {
			row.Status = "not installed"
		} else if !f.ServiceActive {
			row.Status = "service stopped"
		} else {
			row.Status = "not registered"
		}
		st.Nodes = append(st.Nodes, row)
	}

	return st, nil
}

// queryNodes reads node state via kubectl on a control-plane machine.
//
// Going through the node avoids requiring a local kubeconfig, so `cluster status`
// works immediately after bootstrap and from any machine with SSH access.
func (m *Manager) queryNodes(ctx context.Context, server inventory.Server) ([]NodeStatus, error) {
	client, err := m.connect(ctx, server)
	if err != nil {
		return nil, err
	}

	// Custom columns keep parsing trivial and stable across kubectl versions.
	//
	// The whole argument must be quoted for the remote shell: the JSONPath filter
	// contains parentheses, which a shell reads as subshell syntax, and the
	// backslash-escaped dots would be eaten too. Unquoted, this fails with a shell
	// syntax error that surfaces as "could not query the API server".
	columns := `NAME:.metadata.name,STATUS:.status.conditions[?(@.type=="Ready")].status,` +
		`ROLES:.metadata.labels.node-role\.kubernetes\.io/control-plane,VERSION:.status.nodeInfo.kubeletVersion`
	cmd := m.distro.KubectlCommand() + " get nodes --no-headers -o " +
		remote.Quote("custom-columns="+columns)

	res, err := client.TrySudo(ctx, cmd)
	if err != nil || res.ExitCode != 0 {
		return nil, fmt.Errorf("querying nodes on %s", server.Host)
	}

	var out []NodeStatus
	for _, line := range strings.Split(strings.TrimSpace(res.Stdout), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		node := NodeStatus{Name: fields[0]}
		if fields[1] == "True" {
			node.Status = "Ready"
		} else {
			node.Status = "NotReady"
		}
		if len(fields) > 2 && fields[2] == "true" {
			node.Roles = "control-plane"
		} else {
			node.Roles = "worker"
		}
		if len(fields) > 3 {
			node.Version = fields[3]
		}
		out = append(out, node)
	}
	return out, nil
}

// Reset uninstalls the distribution from every machine.
//
// Workers are removed first and the bootstrap control plane last: tearing down
// the API server first would leave agents unable to deregister cleanly.
func (m *Manager) Reset(ctx context.Context) error {
	inv, err := m.provider.Resolve(ctx)
	if err != nil {
		return err
	}
	bootstrap, err := inv.Bootstrap()
	if err != nil {
		return err
	}

	ordered := append([]inventory.Server{}, inv.Workers()...)
	for _, s := range inv.ControlPlanes() {
		if s.Host != bootstrap.Host {
			ordered = append(ordered, s)
		}
	}
	ordered = append(ordered, bootstrap)

	var failures []string
	for _, server := range ordered {
		m.log.Step(fmt.Sprintf("Removing %s from %s", m.distro.Name(), server.Host))

		client, err := m.connect(ctx, server)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", server.Host, err))
			continue
		}

		script := m.distro.UninstallCommand(server.Role)
		exists, err := client.Exists(ctx, script)
		if err != nil || !exists {
			m.log.Detail("%s: no uninstall script; nothing to remove", server.Host)
			continue
		}
		if _, err := client.Sudo(ctx, script); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", server.Host, err))
			continue
		}
		m.log.Success("%s cleaned", server.Host)
	}

	if len(failures) > 0 {
		return fmt.Errorf("reset failed on %d server(s):\n  - %s", len(failures), strings.Join(failures, "\n  - "))
	}
	return nil
}
