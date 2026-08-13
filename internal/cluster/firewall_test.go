package cluster

import (
	"strings"
	"testing"

	"github.com/danecwalker/buidl/internal/config"
	"github.com/danecwalker/buidl/internal/inventory"
)

func TestFirewallExampleSplitsDualStackCIDRs(t *testing.T) {
	infra := &config.Infra{
		Kubernetes: config.ClusterKubernetes{
			ClusterCIDR: config.DefaultClusterCIDR,
			ServiceCIDR: config.DefaultServiceCIDR,
		},
	}
	inv := &inventory.Inventory{Servers: []inventory.Server{{Host: "10.0.0.1", Role: inventory.RoleControlPlane}}}
	inventory.Normalize(inv)

	got := firewallExample(FirewallUFW, infra, inv, inventory.RoleControlPlane)
	for _, cidr := range []string{"10.42.0.0/16", "fd00:42::/56", "10.43.0.0/16", "fd00:43::/112"} {
		want := "ufw allow from " + cidr + " to any"
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "from 10.42.0.0/16,fd00") {
		t.Error("must not pass a comma-separated list to ufw")
	}
}
