package cluster

import (
	"testing"

	"github.com/danecwalker/buidl/internal/inventory"
)

// TestVersionDrifted covers the check that keeps a changed `kubernetes.version`
// from being a silent no-op.
//
// The version is not part of the node config file, so without this check an
// already-joined node would compare equal and be reported as up to date while
// still running the old version.
func TestVersionDrifted(t *testing.T) {
	tests := []struct {
		name      string
		installed string
		want      string
		drifted   bool
	}{
		{
			name:      "matching pinned version",
			installed: "k3s version v1.34.1+k3s1 (0fa1b2c3)",
			want:      "v1.34.1+k3s1",
			drifted:   false,
		},
		{
			name:      "older installed version",
			installed: "k3s version v1.33.0+k3s1 (deadbeef)",
			want:      "v1.34.1+k3s1",
			drifted:   true,
		},
		{
			name:      "unpinned config never drifts",
			installed: "k3s version v1.33.0+k3s1 (deadbeef)",
			want:      "",
			drifted:   false,
		},
		{
			name:      "undetectable installed version is not drift",
			installed: "",
			want:      "v1.34.1+k3s1",
			drifted:   false,
		},
		{
			name:      "rke2 version string",
			installed: "rke2 version v1.34.1+rke2r1 (abc)",
			want:      "v1.34.1+rke2r1",
			drifted:   false,
		},
		{
			name:      "patch difference is drift",
			installed: "k3s version v1.34.0+k3s1 (abc)",
			want:      "v1.34.1+k3s1",
			drifted:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := versionDrifted(tt.installed, tt.want); got != tt.drifted {
				t.Errorf("versionDrifted(%q, %q) = %v, want %v", tt.installed, tt.want, got, tt.drifted)
			}
		})
	}
}

// TestSkippedNodesAreNotCountedAsChanges verifies the accounting that keeps a
// plan honest about what it does and does not know.
func TestPlanAccounting(t *testing.T) {
	reachable := Facts{Reachable: true, OS: "Linux", HasSystemd: true}
	plan := &Plan{Nodes: []NodePlan{
		{Server: inventory.Server{Host: "a"}, Action: ActionBootstrap, Facts: reachable},
		{Server: inventory.Server{Host: "b"}, Action: ActionJoinWorker, Facts: reachable},
		{Server: inventory.Server{Host: "c"}, Action: ActionUpToDate, Facts: reachable},
		{Server: inventory.Server{Host: "d"}, Action: ActionSkipped, Facts: Facts{Reachable: false}},
	}}

	if n := len(plan.Changes()); n != 2 {
		t.Errorf("Changes = %d, want 2 (skipped and up-to-date excluded)", n)
	}
	if !plan.HasChanges() {
		t.Error("HasChanges should be true")
	}
	if n := len(plan.Skipped()); n != 1 {
		t.Errorf("Skipped = %d, want 1", n)
	}
	if n := len(plan.Unreachable()); n != 1 {
		t.Errorf("Unreachable = %d, want 1", n)
	}
	// At least one machine was inspected, so the plan means something.
	if !plan.Actionable() {
		t.Error("Actionable should be true when some servers were inspected")
	}
}

// TestPlanIsNotActionableWhenNothingWasInspected is the guard against the most
// dangerous possible output: "no changes" on a fleet nobody could reach, which
// reads as a healthy cluster.
func TestPlanIsNotActionableWhenNothingWasInspected(t *testing.T) {
	plan := &Plan{Nodes: []NodePlan{
		{Server: inventory.Server{Host: "a"}, Action: ActionSkipped, Facts: Facts{Reachable: false}},
		{Server: inventory.Server{Host: "b"}, Action: ActionSkipped, Facts: Facts{Reachable: false}},
	}}

	if plan.HasChanges() {
		t.Error("skipped nodes must not register as changes")
	}
	if plan.Actionable() {
		t.Error("a plan where every server was skipped must not be actionable")
	}
	if n := len(plan.Unreachable()); n != 2 {
		t.Errorf("Unreachable = %d, want 2", n)
	}
}

func TestEmptyPlanIsActionable(t *testing.T) {
	// No nodes means no servers configured, which validation already rejects;
	// Actionable must not divide by zero or report a false negative.
	plan := &Plan{}
	if !plan.Actionable() {
		t.Error("an empty plan should not be reported as unknown")
	}
}

// TestPendingAddonsAreReportedAsChanges guards the case that made `plan` claim a
// cluster matched its configuration while a configured addon had never been
// installed — after which `deploy` performed a multi-minute cluster-wide CRD
// install unannounced, and `plan --detailed-exitcode` exited 0.
func TestPendingAddons(t *testing.T) {
	tests := []struct {
		name    string
		addons  []AddonPlan
		pending []string
	}{
		{
			name:    "no addons configured",
			addons:  nil,
			pending: nil,
		},
		{
			name: "every addon already installed",
			addons: []AddonPlan{
				{Addon: Addon{Name: "cert-manager"}, Installed: true},
				{Addon: Addon{Name: "buildkit"}, Installed: true},
			},
			pending: nil,
		},
		{
			name: "one addon missing on an otherwise up-to-date cluster",
			addons: []AddonPlan{
				{Addon: Addon{Name: "cert-manager"}},
				{Addon: Addon{Name: "buildkit"}, Installed: true},
			},
			pending: []string{"cert-manager"},
		},
		{
			// A cluster that does not exist yet has none of them.
			name: "fresh cluster",
			addons: []AddonPlan{
				{Addon: Addon{Name: "cert-manager"}},
				{Addon: Addon{Name: "metrics-server"}},
			},
			pending: []string{"cert-manager", "metrics-server"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Nodes are deliberately up to date: the whole point is that server-level
			// convergence says nothing about addons.
			plan := &Plan{
				Nodes:  []NodePlan{{Server: inventory.Server{Host: "a"}, Action: ActionUpToDate}},
				Addons: tt.addons,
			}

			got := plan.PendingAddons()
			if len(got) != len(tt.pending) {
				t.Fatalf("PendingAddons = %d entries, want %d", len(got), len(tt.pending))
			}
			for i, want := range tt.pending {
				if got[i].Addon.Name != want {
					t.Errorf("PendingAddons[%d] = %q, want %q", i, got[i].Addon.Name, want)
				}
			}
			// Server-level accounting must stay unchanged, or deploy would prompt to
			// install Kubernetes on zero servers.
			if plan.HasChanges() {
				t.Error("a pending addon must not register as a server change")
			}
		})
	}
}

func TestVerbForAction(t *testing.T) {
	if got := verbFor(ActionUpgrade); got != "Upgrading" {
		t.Errorf("verbFor(upgrade) = %q", got)
	}
	if got := verbFor(ActionReconfigure); got != "Reconfiguring" {
		t.Errorf("verbFor(reconfigure) = %q", got)
	}
}

func TestFirstLine(t *testing.T) {
	if got := firstLine("first\nsecond\nthird"); got != "first" {
		t.Errorf("firstLine = %q", got)
	}
	if got := firstLine("only"); got != "only" {
		t.Errorf("firstLine = %q", got)
	}
	if got := firstLine("  padded  \nnext"); got != "padded" {
		t.Errorf("firstLine = %q", got)
	}
}
