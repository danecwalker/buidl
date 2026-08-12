package cli

import (
	"strings"
	"testing"

	"github.com/danewalker/buidl/internal/cluster"
	"github.com/danewalker/buidl/internal/config"
	"github.com/danewalker/buidl/internal/inventory"
	"github.com/danewalker/buidl/internal/ui"
)

// clusterPlanWithAddons builds a plan whose servers are all up to date, so the
// only thing left to report is the addons.
func clusterPlanWithAddons(t *testing.T, addons ...cluster.AddonPlan) *cluster.Plan {
	t.Helper()

	distro, err := cluster.DistroFor(config.DistributionK3s)
	if err != nil {
		t.Fatalf("DistroFor: %v", err)
	}
	server := inventory.Server{Host: "10.0.0.1", Role: inventory.RoleControlPlane}
	return &cluster.Plan{
		Distro:              distro,
		Kubernetes:          config.ClusterKubernetes{Version: "v1.34.1+k3s1"},
		Inventory:           &inventory.Inventory{Servers: []inventory.Server{server}},
		Bootstrap:           server,
		RegistrationAddress: "https://10.0.0.1:6443",
		Nodes: []cluster.NodePlan{{
			Server: server,
			Role:   inventory.RoleControlPlane,
			Action: cluster.ActionUpToDate,
			Facts:  cluster.Facts{Reachable: true, OS: "Linux", HasSystemd: true, Installed: true, ServiceActive: true},
			Reason: "already joined",
		}},
		Addons: addons,
	}
}

// TestRenderClusterPlanReportsPendingAddons is the guard against a plan that
// claims a cluster matches its configuration while a configured addon was never
// installed. The deploy that follows then performs a cluster-wide CRD install
// taking minutes, unannounced.
func TestRenderClusterPlanReportsPendingAddons(t *testing.T) {
	app, out := newTestApp(t, ui.ModePlain)

	app.renderClusterPlan(clusterPlanWithAddons(t,
		cluster.AddonPlan{Addon: cluster.Addon{Name: "cert-manager"}},
		cluster.AddonPlan{Addon: cluster.Addon{Name: "buildkit"}, Installed: true},
	), false)

	got := out.String()
	if strings.Contains(got, "no changes; the cluster matches your configuration") {
		t.Errorf("output claims the cluster matches the config with an addon pending:\n%s", got)
	}
	if !strings.Contains(got, "cert-manager") || !strings.Contains(got, "to install") {
		t.Errorf("output does not say cert-manager would be installed:\n%s", got)
	}
	// The header listed names alone, which reads as a description of the cluster
	// rather than of the configuration.
	if !strings.Contains(got, "cert-manager (pending)") {
		t.Errorf("header does not mark cert-manager as pending:\n%s", got)
	}
	if !strings.Contains(got, "buildkit (installed)") {
		t.Errorf("header does not mark buildkit as installed:\n%s", got)
	}
}

func TestRenderClusterPlanReportsNoChangesWhenAddonsAreInstalled(t *testing.T) {
	app, out := newTestApp(t, ui.ModePlain)

	app.renderClusterPlan(clusterPlanWithAddons(t,
		cluster.AddonPlan{Addon: cluster.Addon{Name: "cert-manager"}, Installed: true},
	), false)

	got := out.String()
	// The converse failure matters as much: a plan that always reported work would
	// make --detailed-exitcode useless as a gate.
	if !strings.Contains(got, "no changes; the cluster matches your configuration") {
		t.Errorf("a fully converged cluster should report no changes:\n%s", got)
	}
	if strings.Contains(got, "to install") {
		t.Errorf("nothing is pending, but the plan lists an install:\n%s", got)
	}
}

func TestAddonPlanSummary(t *testing.T) {
	tests := []struct {
		name   string
		addons []cluster.AddonPlan
		want   string
	}{
		{
			name: "none configured",
			want: "none",
		},
		{
			name: "mixed states",
			addons: []cluster.AddonPlan{
				{Addon: cluster.Addon{Name: "cert-manager"}},
				{Addon: cluster.Addon{Name: "metrics-server"}, Installed: true},
			},
			want: "cert-manager (pending), metrics-server (installed)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := &cluster.Plan{Addons: tt.addons}
			if got := addonPlanSummary(plan); got != tt.want {
				t.Errorf("addonPlanSummary = %q, want %q", got, tt.want)
			}
		})
	}
}
