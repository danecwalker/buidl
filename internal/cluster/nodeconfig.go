package cluster

import (
	"fmt"
	"sort"

	"gopkg.in/yaml.v3"

	"github.com/danewalker/buidl/internal/config"
	"github.com/danewalker/buidl/internal/inventory"
)

// nodeConfig is the distribution config file written to each machine.
//
// Driving everything through a config file rather than command-line flags matters
// for idempotency: re-running the installer picks up the same file, so a repeated
// `cluster up` converges instead of producing a differently-flagged unit. It also
// keeps the join token out of the process list and shell history.
//
// k3s and RKE2 accept the same keys for everything buidl sets, so one struct
// serves both.
type nodeConfig struct {
	// Token is the shared cluster secret. Present on every node.
	Token string `yaml:"token,omitempty"`

	// Server is the address a joining node registers with. Absent on the
	// bootstrap node, which has nothing to join.
	Server string `yaml:"server,omitempty"`

	// ClusterInit starts a new cluster with embedded etcd. Only on bootstrap.
	ClusterInit bool `yaml:"cluster-init,omitempty"`

	// TLSSAN lists additional names the API server certificate must cover.
	// Without the endpoint here, a kubeconfig pointing at a load balancer would
	// fail certificate validation.
	TLSSAN []string `yaml:"tls-san,omitempty"`

	NodeName  string   `yaml:"node-name,omitempty"`
	NodeLabel []string `yaml:"node-label,omitempty"`
	NodeTaint []string `yaml:"node-taint,omitempty"`

	// NodeIP pins the address this node advertises to the cluster. Essential on
	// cloud servers with both a public and a private interface: without it the
	// node may advertise its public address and route all intra-cluster traffic
	// over the internet.
	NodeIP string `yaml:"node-ip,omitempty"`

	ClusterCIDR string `yaml:"cluster-cidr,omitempty"`
	ServiceCIDR string `yaml:"service-cidr,omitempty"`

	Disable []string `yaml:"disable,omitempty"`

	// WriteKubeconfigMode makes the admin kubeconfig readable so buidl can fetch
	// it over SSH without a second privilege escalation.
	WriteKubeconfigMode string `yaml:"write-kubeconfig-mode,omitempty"`

	// extra carries user-supplied passthrough keys, merged at render time.
	extra map[string]string `yaml:"-"`
}

// renderNodeConfig builds the config file for one machine.
func renderNodeConfig(
	server inventory.Server,
	role inventory.Role,
	isBootstrap bool,
	plan *Plan,
) (string, error) {
	k8s := plan.Kubernetes

	nc := nodeConfig{
		Token:     plan.Token,
		NodeName:  server.Name,
		NodeIP:    server.PrivateIP,
		NodeLabel: labelArgs(server.Labels),
		NodeTaint: server.Taints,
	}

	if role == inventory.RoleControlPlane {
		// Server-only settings. Agents reject cluster-level keys.
		nc.ClusterCIDR = k8s.ClusterCIDR
		nc.ServiceCIDR = k8s.ServiceCIDR
		nc.Disable = k8s.Disable
		nc.TLSSAN = plan.TLSSANs
		// 0644 would expose cluster-admin to every local user; 0640 keeps it to
		// root and its group, which is enough for buidl to read over sudo.
		nc.WriteKubeconfigMode = "0640"

		if isBootstrap {
			// Always initialize with embedded etcd, even for a single node. The
			// alternative (sqlite) cannot later accept additional control planes
			// without a migration, so this keeps a one-node cluster growable.
			nc.ClusterInit = true
		} else {
			nc.Server = plan.RegistrationAddress
		}
	} else {
		nc.Server = plan.RegistrationAddress
	}

	// Render, then merge passthrough keys so users can set anything buidl does
	// not model without waiting for a schema change.
	doc := map[string]any{}
	encoded, err := yaml.Marshal(nc)
	if err != nil {
		return "", fmt.Errorf("rendering node config: %w", err)
	}
	if err := yaml.Unmarshal(encoded, &doc); err != nil {
		return "", fmt.Errorf("rendering node config: %w", err)
	}

	for k, v := range k8s.ExtraArgs {
		doc[k] = v
	}

	out, err := yaml.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("rendering node config: %w", err)
	}

	header := fmt.Sprintf("# Managed by buidl. Edits are overwritten on the next `buidl cluster up`.\n# node: %s (%s)\n", server.Host, role)
	return header + string(out), nil
}

// labelArgs renders node labels as key=value pairs in a stable order.
func labelArgs(labels map[string]string) []string {
	if len(labels) == 0 {
		return nil
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	// Deterministic order keeps the rendered config stable, so an unchanged
	// inventory does not rewrite files and restart services.
	sort.Strings(keys)

	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+labels[k])
	}
	return out
}

// registrationAddress determines the URL joining nodes register with.
//
// A configured endpoint always wins: with more than one control plane, nodes must
// register through a stable address so losing the bootstrap machine does not
// break future joins. Validation requires the endpoint in that case, so the
// fallback here only ever applies to a single-control-plane cluster.
func registrationAddress(k8s config.ClusterKubernetes, bootstrap inventory.Server) string {
	if k8s.ControlPlaneEndpoint != "" {
		return fmt.Sprintf("https://%s:6443", k8s.ControlPlaneEndpoint)
	}
	return fmt.Sprintf("https://%s:6443", bootstrap.InternalAddress())
}

// tlsSANs collects every name the API server certificate must cover.
//
// Missing a name here produces a confusing failure much later: kubectl reports a
// certificate error against an address that otherwise works, so it is cheaper to
// be generous.
func tlsSANs(k8s config.ClusterKubernetes, inv *inventory.Inventory) []string {
	seen := map[string]bool{}
	var out []string

	add := func(name string) {
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		out = append(out, name)
	}

	add(k8s.ControlPlaneEndpoint)
	for _, san := range k8s.TLSSANs {
		add(san)
	}
	// Every control-plane address, public and private: buidl may connect via one
	// while cluster members use the other.
	for _, s := range inv.ControlPlanes() {
		add(s.Host)
		add(s.PrivateIP)
	}
	return out
}
