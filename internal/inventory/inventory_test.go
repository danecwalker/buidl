package inventory

import (
	"context"
	"strings"
	"testing"
)

func resolve(t *testing.T, servers ...Server) *Inventory {
	t.Helper()
	inv, err := Static{Servers: servers}.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return inv
}

func TestSingleServerBecomesControlPlane(t *testing.T) {
	// The smallest useful config is one host with no role. It must produce a
	// working single-node cluster rather than an inventory with no control plane.
	inv := resolve(t, Server{Host: "10.0.0.1"})

	if got := inv.Servers[0].Role; got != RoleControlPlane {
		t.Errorf("role = %q, want control-plane", got)
	}
	if inv.HighlyAvailable() {
		t.Error("a single server is not highly available")
	}
}

func TestRolelessServersDefaultToWorkerAfterTheFirst(t *testing.T) {
	inv := resolve(t,
		Server{Host: "10.0.0.1"},
		Server{Host: "10.0.0.2"},
		Server{Host: "10.0.0.3"},
	)

	if inv.Servers[0].Role != RoleControlPlane {
		t.Error("the first server should become the control plane")
	}
	for _, s := range inv.Servers[1:] {
		if s.Role != RoleWorker {
			t.Errorf("%s role = %q, want worker", s.Host, s.Role)
		}
	}
}

func TestExplicitControlPlanePreventsPromotion(t *testing.T) {
	// When the user named a control plane, the first entry must not be promoted
	// as well — that would create an unintended second etcd member.
	inv := resolve(t,
		Server{Host: "10.0.1.1"},
		Server{Host: "10.0.0.1", Role: RoleControlPlane},
	)

	if inv.Servers[0].Role != RoleWorker {
		t.Errorf("first server role = %q, want worker", inv.Servers[0].Role)
	}
	if len(inv.ControlPlanes()) != 1 {
		t.Errorf("control planes = %d, want 1", len(inv.ControlPlanes()))
	}
}

func TestBootstrapIsFirstControlPlaneInOrder(t *testing.T) {
	// Bootstrap order is significant: this machine creates the cluster and every
	// other node registers against it, so it must be deterministic.
	inv := resolve(t,
		Server{Host: "10.0.1.1", Role: RoleWorker},
		Server{Host: "10.0.0.2", Role: RoleControlPlane},
		Server{Host: "10.0.0.1", Role: RoleControlPlane},
	)

	bootstrap, err := inv.Bootstrap()
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if bootstrap.Host != "10.0.0.2" {
		t.Errorf("bootstrap = %s, want the first control plane in inventory order", bootstrap.Host)
	}
}

func TestInternalAddressPrefersPrivateIP(t *testing.T) {
	// On most clouds the public address is NAT'd or metered, so cluster traffic
	// must use the private network when one is declared.
	withPrivate := Server{Host: "203.0.113.5", PrivateIP: "10.0.0.1"}
	if got := withPrivate.InternalAddress(); got != "10.0.0.1" {
		t.Errorf("InternalAddress = %q, want the private IP", got)
	}

	withoutPrivate := Server{Host: "203.0.113.5"}
	if got := withoutPrivate.InternalAddress(); got != "203.0.113.5" {
		t.Errorf("InternalAddress = %q, want the host", got)
	}
}

func TestQuorumWarnings(t *testing.T) {
	tests := []struct {
		controlPlanes int
		wantWarning   bool
		contains      string
	}{
		{1, false, ""},
		// Two members tolerate zero failures while doubling the failure surface.
		{2, true, "cannot form an etcd quorum"},
		{3, false, ""},
		{4, true, "even number"},
		{5, false, ""},
	}

	for _, tt := range tests {
		servers := make([]Server, 0, tt.controlPlanes)
		for i := 0; i < tt.controlPlanes; i++ {
			servers = append(servers, Server{
				Host: "10.0.0." + string(rune('1'+i)),
				Role: RoleControlPlane,
			})
		}
		inv := &Inventory{Servers: servers}

		warning := inv.QuorumWarning()
		if tt.wantWarning && warning == "" {
			t.Errorf("%d control planes: expected a warning", tt.controlPlanes)
		}
		if !tt.wantWarning && warning != "" {
			t.Errorf("%d control planes: unexpected warning %q", tt.controlPlanes, warning)
		}
		if tt.contains != "" && !strings.Contains(warning, tt.contains) {
			t.Errorf("%d control planes: warning %q should contain %q", tt.controlPlanes, warning, tt.contains)
		}
	}
}

func TestValidationRejectsProblems(t *testing.T) {
	tests := []struct {
		name    string
		servers []Server
		wantErr string
	}{
		{
			name:    "no servers",
			servers: nil,
			wantErr: "empty",
		},
		{
			name:    "missing host",
			servers: []Server{{Role: RoleControlPlane}},
			wantErr: "host is required",
		},
		{
			name: "duplicate host",
			servers: []Server{
				{Host: "10.0.0.1", Role: RoleControlPlane},
				{Host: "10.0.0.1", Role: RoleWorker},
			},
			wantErr: "more than once",
		},
		{
			name:    "host is a url",
			servers: []Server{{Host: "https://10.0.0.1", Role: RoleControlPlane}},
			wantErr: "bare hostname",
		},
		{
			name:    "invalid role",
			servers: []Server{{Host: "10.0.0.1", Role: "master"}},
			wantErr: "role must be",
		},
		{
			name:    "no control plane",
			servers: []Server{{Host: "10.0.0.1", Role: RoleWorker}},
			wantErr: "at least one server must have role: control-plane",
		},
		{
			name: "duplicate node name",
			servers: []Server{
				{Host: "10.0.0.1", Role: RoleControlPlane, Name: "node"},
				{Host: "10.0.0.2", Role: RoleWorker, Name: "node"},
			},
			wantErr: "used more than once",
		},
		{
			name:    "bad port",
			servers: []Server{{Host: "10.0.0.1", Role: RoleControlPlane, Port: 99999}},
			wantErr: "port must be",
		},
		{
			name:    "taint without effect",
			servers: []Server{{Host: "10.0.0.1", Role: RoleControlPlane, Taints: []string{"gpu=true"}}},
			wantErr: "key=value:Effect",
		},
		{
			name:    "taint with bad effect",
			servers: []Server{{Host: "10.0.0.1", Role: RoleControlPlane, Taints: []string{"gpu=true:Nope"}}},
			wantErr: "effect must be",
		},
		{
			name:    "taint without key",
			servers: []Server{{Host: "10.0.0.1", Role: RoleControlPlane, Taints: []string{"novalue:NoSchedule"}}},
			wantErr: "expected key=value",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inv := &Inventory{Servers: tt.servers}
			err := Validate(inv)
			if err == nil {
				t.Fatalf("expected an error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidationAcceptsGoodTaints(t *testing.T) {
	for _, taint := range []string{
		"gpu=true:NoSchedule",
		"dedicated=db:NoExecute",
		"spot=yes:PreferNoSchedule",
	} {
		inv := &Inventory{Servers: []Server{
			{Host: "10.0.0.1", Role: RoleControlPlane, Taints: []string{taint}},
		}}
		if err := Validate(inv); err != nil {
			t.Errorf("taint %q should be valid: %v", taint, err)
		}
	}
}

func TestValidationReportsEveryProblemAtOnce(t *testing.T) {
	inv := &Inventory{Servers: []Server{
		{Host: "", Role: "bogus"},
		{Host: "10.0.0.1", Role: RoleWorker, Taints: []string{"bad"}},
	}}

	err := Validate(inv)
	if err == nil {
		t.Fatal("expected errors")
	}
	msg := err.Error()
	// Fixing one problem per run is a poor experience for a fleet config.
	for _, want := range []string{"host is required", "role must be", "key=value:Effect"} {
		if !strings.Contains(msg, want) {
			t.Errorf("aggregated error should mention %q, got:\n%s", want, msg)
		}
	}
}

func TestStaticProviderRejectsEmpty(t *testing.T) {
	_, err := Static{}.Resolve(context.Background())
	if err == nil {
		t.Fatal("expected an error for an empty server list")
	}
	if !strings.Contains(err.Error(), "infra.servers") {
		t.Errorf("error should point at the config key, got: %v", err)
	}
}

func TestStaticProviderDoesNotMutateInput(t *testing.T) {
	// Resolve normalizes roles; the caller's slice (which came from config) must
	// not be rewritten underneath it.
	original := []Server{{Host: "10.0.0.1"}}
	if _, err := (Static{Servers: original}).Resolve(context.Background()); err != nil {
		t.Fatal(err)
	}
	if original[0].Role != "" {
		t.Errorf("input was mutated: role became %q", original[0].Role)
	}
}

func TestSummary(t *testing.T) {
	tests := []struct {
		servers  []Server
		contains string
	}{
		{[]Server{{Host: "a", Role: RoleControlPlane}}, "single-node"},
		{[]Server{{Host: "a", Role: RoleControlPlane}, {Host: "b", Role: RoleWorker}}, "multi-node"},
		{[]Server{{Host: "a", Role: RoleControlPlane}, {Host: "b", Role: RoleControlPlane}}, "HA"},
	}
	for _, tt := range tests {
		inv := &Inventory{Servers: tt.servers}
		if got := inv.Summary(); !strings.Contains(got, tt.contains) {
			t.Errorf("Summary = %q, want it to contain %q", got, tt.contains)
		}
	}
}

func TestControlPlanesAndWorkersPartition(t *testing.T) {
	inv := resolve(t,
		Server{Host: "cp1", Role: RoleControlPlane},
		Server{Host: "w1", Role: RoleWorker},
		Server{Host: "cp2", Role: RoleControlPlane},
		Server{Host: "w2", Role: RoleWorker},
	)

	if len(inv.ControlPlanes()) != 2 {
		t.Errorf("control planes = %d, want 2", len(inv.ControlPlanes()))
	}
	if len(inv.Workers()) != 2 {
		t.Errorf("workers = %d, want 2", len(inv.Workers()))
	}
	// The two sets must cover every server exactly once.
	if len(inv.ControlPlanes())+len(inv.Workers()) != len(inv.Servers) {
		t.Error("control planes and workers should partition the fleet")
	}
}
