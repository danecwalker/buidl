// Package inventory models the fleet of machines a cluster is built from.
//
// buidl does not provision infrastructure. Creating VMs, networks, firewalls and
// load balancers is the job of OpenTofu, Terraform, Ansible or a cloud console —
// tools that already do it well and own the state. buidl's job starts once the
// machines exist: turn them into a working Kubernetes cluster and keep deploying
// to it.
//
// The Provider interface is the seam between those two worlds. Today only Static
// is wired up (servers listed in buidl.yaml), but a provider that reads
// `tofu output -json`, parses an Ansible inventory, or shells out to an arbitrary
// script implements the same three-method interface and needs no changes
// elsewhere.
package inventory

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// Role describes what a machine does in the cluster.
type Role string

const (
	// RoleControlPlane runs the API server and an etcd member. In k3s and RKE2
	// terms, a "server" node.
	RoleControlPlane Role = "control-plane"
	// RoleWorker runs application workloads only. An "agent" node.
	RoleWorker Role = "worker"
)

// Valid reports whether r is a recognized role.
func (r Role) Valid() bool {
	return r == RoleControlPlane || r == RoleWorker
}

// Server is one machine in the fleet.
type Server struct {
	// Host is the address buidl connects to over SSH. May be a hostname or IP.
	Host string `yaml:"host"`

	// Role defaults to worker when omitted, except that the first server in an
	// otherwise role-less list is promoted to control-plane so a single-server
	// config works with no ceremony.
	Role Role `yaml:"role"`

	// Name overrides the Kubernetes node name. Defaults to the machine's
	// hostname, which is what the kubelet would choose anyway.
	Name string `yaml:"name"`

	// User and Port override the fleet-wide SSH defaults for this machine.
	User string `yaml:"user"`
	Port int    `yaml:"port"`

	// PrivateIP is the address other cluster members should use to reach this
	// machine. On most clouds the public address is NAT'd or metered, so cluster
	// traffic must use the private network. When empty, Host is used.
	PrivateIP string `yaml:"privateIP"`

	// Labels are applied to the Kubernetes node, so workloads can target machine
	// classes (for example a GPU pool) via deploy.kubernetes.nodeSelector.
	Labels map[string]string `yaml:"labels"`

	// Taints keep general workloads off specialized machines. Format:
	// "key=value:Effect", e.g. "gpu=true:NoSchedule".
	Taints []string `yaml:"taints"`
}

// Address returns the address to connect to over SSH.
func (s Server) Address() string {
	return s.Host
}

// InternalAddress returns the address other cluster members should use.
func (s Server) InternalAddress() string {
	if s.PrivateIP != "" {
		return s.PrivateIP
	}
	return s.Host
}

// IsControlPlane reports whether this machine runs the control plane.
func (s Server) IsControlPlane() bool {
	return s.Role == RoleControlPlane
}

// Inventory is a resolved, validated fleet.
type Inventory struct {
	Servers []Server
	// Source names where the inventory came from, for display.
	Source string
}

// Provider resolves an inventory from somewhere.
//
// Implementations must be read-only with respect to infrastructure: a provider
// discovers machines, it never creates them.
type Provider interface {
	// Name identifies the provider in output.
	Name() string
	// Resolve returns the fleet.
	Resolve(ctx context.Context) (*Inventory, error)
}

// Static is a Provider backed by a list written directly in buidl.yaml.
//
// This is the right choice for genuinely bare metal, and the simplest way to
// consume another tool's output: paste the addresses it produced.
type Static struct {
	Servers []Server
}

// Name implements Provider.
func (s Static) Name() string { return "static" }

// Resolve implements Provider.
func (s Static) Resolve(context.Context) (*Inventory, error) {
	if len(s.Servers) == 0 {
		return nil, fmt.Errorf("no servers listed under infra.servers")
	}
	servers := make([]Server, len(s.Servers))
	copy(servers, s.Servers)

	inv := &Inventory{Servers: servers, Source: "buidl.yaml"}
	Normalize(inv)
	if err := Validate(inv); err != nil {
		return nil, err
	}
	return inv, nil
}

// Normalize fills in defaults that depend on the fleet as a whole.
func Normalize(inv *Inventory) {
	if len(inv.Servers) == 0 {
		return
	}

	// A single-server config, or one where nobody declared a role, still needs a
	// control plane. Promoting the first entry means `infra.servers: [{host: x}]`
	// produces a working single-node cluster.
	hasControlPlane := false
	for _, s := range inv.Servers {
		if s.Role == RoleControlPlane {
			hasControlPlane = true
			break
		}
	}
	for i := range inv.Servers {
		if inv.Servers[i].Role == "" {
			if !hasControlPlane && i == 0 {
				inv.Servers[i].Role = RoleControlPlane
				hasControlPlane = true
			} else {
				inv.Servers[i].Role = RoleWorker
			}
		}
	}
}

// Validate checks a normalized inventory for problems that would make cluster
// bootstrap fail or produce an unsafe cluster.
func Validate(inv *Inventory) error {
	if len(inv.Servers) == 0 {
		return fmt.Errorf("inventory is empty")
	}

	var problems []string
	seenHost := map[string]bool{}
	seenName := map[string]bool{}
	controlPlanes := 0

	for i, s := range inv.Servers {
		where := fmt.Sprintf("infra.servers[%d]", i)

		if strings.TrimSpace(s.Host) == "" {
			problems = append(problems, where+": host is required")
		} else {
			// A duplicated host would be installed twice and join itself.
			if seenHost[s.Host] {
				problems = append(problems, fmt.Sprintf("%s: host %s appears more than once", where, s.Host))
			}
			seenHost[s.Host] = true

			if strings.Contains(s.Host, "://") {
				problems = append(problems, fmt.Sprintf("%s: host must be a bare hostname or IP, not a URL (got %q)", where, s.Host))
			}
		}

		if !s.Role.Valid() {
			problems = append(problems, fmt.Sprintf("%s: role must be %q or %q (got %q)", where, RoleControlPlane, RoleWorker, s.Role))
		}
		if s.Role == RoleControlPlane {
			controlPlanes++
		}

		if s.Name != "" {
			if seenName[s.Name] {
				problems = append(problems, fmt.Sprintf("%s: node name %q is used more than once", where, s.Name))
			}
			seenName[s.Name] = true
		}

		if s.Port < 0 || s.Port > 65535 {
			problems = append(problems, fmt.Sprintf("%s: port must be between 1 and 65535 (got %d)", where, s.Port))
		}

		for _, taint := range s.Taints {
			if err := validateTaint(taint); err != nil {
				problems = append(problems, fmt.Sprintf("%s: taint %q: %v", where, taint, err))
			}
		}
	}

	if controlPlanes == 0 {
		problems = append(problems, "at least one server must have role: control-plane")
	}

	if len(problems) > 0 {
		return fmt.Errorf("invalid inventory:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return nil
}

// validateTaint checks the "key=value:Effect" form Kubernetes expects.
func validateTaint(taint string) error {
	kv, effect, found := strings.Cut(taint, ":")
	if !found {
		return fmt.Errorf("expected key=value:Effect")
	}
	switch effect {
	case "NoSchedule", "PreferNoSchedule", "NoExecute":
	default:
		return fmt.Errorf("effect must be NoSchedule, PreferNoSchedule or NoExecute (got %q)", effect)
	}
	if key, _, found := strings.Cut(kv, "="); !found || key == "" {
		return fmt.Errorf("expected key=value before the effect")
	}
	return nil
}

// ControlPlanes returns the control-plane servers in inventory order.
//
// Order matters: the first is bootstrapped and becomes the cluster's initial
// etcd member, and the rest join it.
func (inv *Inventory) ControlPlanes() []Server {
	var out []Server
	for _, s := range inv.Servers {
		if s.IsControlPlane() {
			out = append(out, s)
		}
	}
	return out
}

// Workers returns the worker servers.
func (inv *Inventory) Workers() []Server {
	var out []Server
	for _, s := range inv.Servers {
		if !s.IsControlPlane() {
			out = append(out, s)
		}
	}
	return out
}

// Bootstrap returns the server that initializes the cluster.
func (inv *Inventory) Bootstrap() (Server, error) {
	cps := inv.ControlPlanes()
	if len(cps) == 0 {
		return Server{}, fmt.Errorf("no control-plane server in inventory")
	}
	return cps[0], nil
}

// HighlyAvailable reports whether the control plane has more than one member.
func (inv *Inventory) HighlyAvailable() bool {
	return len(inv.ControlPlanes()) > 1
}

// QuorumWarning returns a warning when the control-plane count cannot form a
// healthy etcd quorum, or "" when the topology is sound.
//
// etcd needs a strict majority to accept writes. With two members, losing either
// one loses quorum, so a 2-node control plane is *less* available than a single
// node: it doubles the failure surface while still tolerating zero failures.
func (inv *Inventory) QuorumWarning() string {
	n := len(inv.ControlPlanes())
	switch {
	case n == 2:
		return "2 control-plane servers cannot form an etcd quorum: losing either one makes the cluster read-only. Use 1 or 3."
	case n > 2 && n%2 == 0:
		return fmt.Sprintf("%d control-plane servers is an even number; etcd tolerates the same failures as %d. Use an odd count.", n, n-1)
	}
	return ""
}

// Summary renders a one-line description of the topology.
func (inv *Inventory) Summary() string {
	cp := len(inv.ControlPlanes())
	w := len(inv.Workers())
	kind := "single-node"
	switch {
	case cp > 1:
		kind = "HA"
	case w > 0:
		kind = "multi-node"
	}
	return fmt.Sprintf("%s: %d control-plane, %d worker", kind, cp, w)
}

// Hosts returns every host address, sorted, for display.
func (inv *Inventory) Hosts() []string {
	out := make([]string, 0, len(inv.Servers))
	for _, s := range inv.Servers {
		out = append(out, s.Host)
	}
	sort.Strings(out)
	return out
}
