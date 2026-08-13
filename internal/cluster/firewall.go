package cluster

import (
	"context"
	"fmt"
	"strings"

	"github.com/danecwalker/buidl/internal/config"
	"github.com/danecwalker/buidl/internal/inventory"
	"github.com/danecwalker/buidl/internal/remote"
)

// FirewallKind identifies the host firewall in use on a machine.
type FirewallKind string

const (
	FirewallNone FirewallKind = ""
	FirewallUFW  FirewallKind = "ufw"
	// FirewallFirewalld covers RHEL-family images.
	FirewallFirewalld FirewallKind = "firewalld"
	// FirewallUnknown means packets are being filtered but by something buidl
	// does not recognize. Worth saying, since the symptom is identical.
	FirewallUnknown FirewallKind = "a host firewall"
)

// buidl detects host firewalls but never changes them.
//
// The reasoning, which is worth stating because the opposite is tempting: buidl
// installs Kubernetes, so it knows exactly which ports the cluster needs, and it
// would be easy to open them. Two things argue against it.
//
// First, blast radius. Installing a service is additive; rewriting firewall
// policy is not. A rule applied slightly wrong — a bad range, a mistyped CIDR,
// an implementation whose syntax differs from what was assumed — can sever SSH
// and leave a machine unreachable, with buidl itself as the thing that did it.
// A *printed* command that is slightly wrong costs someone thirty seconds.
//
// Second, surface area. Doing this properly means owning ufw, firewalld,
// nftables, iptables and whatever a given image ships, across distributions,
// forever. Detection degrades gracefully; application does not.
//
// Kamal takes the same position, treating open ports as a server prerequisite.
// So buidl detects, explains precisely, and leaves the change to the operator.

// requiredPorts describes the openings this cluster needs.
//
// The set is derived from buidl's own configuration rather than fixed, which is
// why the advice lives here: etcd ports only matter for a multi-member control
// plane, and the ranges to allow come from clusterCIDR and serviceCIDR. Any
// external copy of this list silently goes stale the moment one of those changes.
func requiredPorts(inv *inventory.Inventory, role inventory.Role) []string {
	ports := []string{
		"8472/udp   flannel vxlan overlay",
		"10250/tcp  kubelet",
	}

	if role == inventory.RoleControlPlane {
		ports = append(ports, "6443/tcp   kubernetes api")
		// etcd peers only exist with more than one control plane; opening them on
		// a single node would be exposure with no purpose.
		if inv.HighlyAvailable() {
			ports = append(ports, "2379:2380/tcp  etcd peers")
		}
	}

	// Both supported distributions ship an ingress controller, and ACME http-01
	// needs port 80 reachable from arbitrary addresses.
	ports = append(ports,
		"80/tcp     ingress and acme http-01 challenge",
		"443/tcp    ingress tls",
	)

	return ports
}

// detectFirewall reports which host firewall is active, if any.
//
// Only an *active* firewall is reported: an installed-but-inactive ufw blocks
// nothing, and warning about it would be noise.
func detectFirewall(ctx context.Context, client *remote.Client) FirewallKind {
	if res, err := client.TrySudo(ctx, "ufw status 2>/dev/null | head -1"); err == nil {
		if strings.Contains(res.Stdout, "Status: active") {
			return FirewallUFW
		}
	}

	if res, err := client.TrySudo(ctx, "systemctl is-active firewalld 2>/dev/null"); err == nil {
		if strings.TrimSpace(res.Stdout) == "active" {
			return FirewallFirewalld
		}
	}

	// A default-DROP input policy filters traffic regardless of which tool set it.
	// Recognising this is what keeps the diagnosis useful on images that use
	// neither ufw nor firewalld.
	if res, err := client.TrySudo(ctx, "iptables -L INPUT -n 2>/dev/null | head -1"); err == nil {
		if strings.Contains(res.Stdout, "policy DROP") {
			return FirewallUnknown
		}
	}

	return FirewallNone
}

// firewallWarning explains an active host firewall and how to open the cluster's
// ports.
//
// This is the entire value of detection. A firewall permitting only SSH lets the
// whole install succeed — buidl connects, installs, starts the service, reports
// success — while the API server and ingress are silently unreachable. The
// symptom is a bare connection timeout with nothing pointing at the cause.
func firewallWarning(infra *config.Infra, inv *inventory.Inventory, node NodePlan) string {
	kind := node.Facts.Firewall
	if kind == FirewallNone {
		return ""
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s is active and will block this cluster.\n\n", node.Server.Host, kind)
	fmt.Fprintf(&b, "The install will still succeed, but the API server and ingress will be\n")
	fmt.Fprintf(&b, "unreachable. These ports need to be open:\n\n")

	for _, port := range requiredPorts(inv, node.Role) {
		fmt.Fprintf(&b, "  %s\n", port)
	}

	// Forwarded pod and service traffic is the part people miss: opening the
	// ports above is not enough, because a default-deny policy also drops
	// traffic between pods, which fails as DNS timeouts and probes that never
	// pass rather than as anything resembling a firewall problem.
	fmt.Fprintf(&b, "\nplus forwarded traffic for the pod and service networks:\n\n")
	for _, cidr := range config.SplitCIDRs(infra.Kubernetes.ClusterCIDR) {
		fmt.Fprintf(&b, "  %s  pod network\n", cidr)
	}
	for _, cidr := range config.SplitCIDRs(infra.Kubernetes.ServiceCIDR) {
		fmt.Fprintf(&b, "  %s  service network\n", cidr)
	}

	if example := firewallExample(kind, infra, inv, node.Role); example != "" {
		fmt.Fprintf(&b, "\nFor %s:\n\n%s", kind, example)
	}

	fmt.Fprintf(&b, "\nbuidl does not change firewall rules: a mistaken rule can sever SSH and\n")
	fmt.Fprintf(&b, "lock you out of the machine. Apply them yourself, or in your provisioning.")

	return b.String()
}

// firewallExample renders copy-pasteable commands for a recognized firewall.
func firewallExample(kind FirewallKind, infra *config.Infra, inv *inventory.Inventory, role inventory.Role) string {
	var b strings.Builder

	switch kind {
	case FirewallUFW:
		for _, port := range requiredPorts(inv, role) {
			fmt.Fprintf(&b, "  ufw allow %s\n", strings.Fields(port)[0])
		}
		// "to any" is what covers forwarded traffic, not just traffic addressed
		// to the host itself.
		for _, cidr := range firewallNetworks(infra) {
			fmt.Fprintf(&b, "  ufw allow from %s to any\n", cidr)
		}

	case FirewallFirewalld:
		for _, port := range requiredPorts(inv, role) {
			// firewalld writes ranges with a dash, not a colon.
			fmt.Fprintf(&b, "  firewall-cmd --permanent --add-port=%s\n",
				strings.Replace(strings.Fields(port)[0], ":", "-", 1))
		}
		for _, cidr := range firewallNetworks(infra) {
			fmt.Fprintf(&b, "  firewall-cmd --permanent --zone=trusted --add-source=%s\n", cidr)
		}
		fmt.Fprintf(&b, "  firewall-cmd --reload\n")

	default:
		// An unrecognized filter gets the port list above but no invented syntax.
		return ""
	}

	return b.String()
}

// firewallNetworks is each pod and service CIDR on its own line. A comma
// list is what k3s accepts; it is not what ufw or firewalld accept.
func firewallNetworks(infra *config.Infra) []string {
	if infra == nil {
		return nil
	}
	var out []string
	out = append(out, config.SplitCIDRs(infra.Kubernetes.ClusterCIDR)...)
	out = append(out, config.SplitCIDRs(infra.Kubernetes.ServiceCIDR)...)
	return out
}
