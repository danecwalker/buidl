package cluster

import (
	"strings"
	"testing"

	"github.com/danecwalker/buidl/internal/config"
	"github.com/danecwalker/buidl/internal/inventory"
)

func TestDistroForSelectsDriver(t *testing.T) {
	k, err := DistroFor(config.DistributionK3s)
	if err != nil {
		t.Fatalf("DistroFor(k3s): %v", err)
	}
	if k.Name() != "k3s" {
		t.Errorf("Name = %q", k.Name())
	}

	r, err := DistroFor(config.DistributionRKE2)
	if err != nil {
		t.Fatalf("DistroFor(rke2): %v", err)
	}
	if r.Name() != "rke2" {
		t.Errorf("Name = %q", r.Name())
	}

	if _, err := DistroFor("openshift"); err == nil {
		t.Error("expected an error for an unsupported distribution")
	}
}

// TestDistroPathsAreDistinct guards against copy-paste errors between the two
// drivers, where an RKE2 node would be handed k3s paths and fail obscurely.
func TestDistroPathsAreDistinct(t *testing.T) {
	k, _ := DistroFor(config.DistributionK3s)
	r, _ := DistroFor(config.DistributionRKE2)

	pairs := []struct {
		name   string
		k3s    string
		rke2   string
		expect [2]string
	}{
		{"config", k.ConfigPath(), r.ConfigPath(), [2]string{"/etc/rancher/k3s/config.yaml", "/etc/rancher/rke2/config.yaml"}},
		{"token", k.TokenPath(), r.TokenPath(), [2]string{"/var/lib/rancher/k3s/server/token", "/var/lib/rancher/rke2/server/token"}},
		{"kubeconfig", k.KubeconfigPath(), r.KubeconfigPath(), [2]string{"/etc/rancher/k3s/k3s.yaml", "/etc/rancher/rke2/rke2.yaml"}},
	}

	for _, p := range pairs {
		if p.k3s != p.expect[0] {
			t.Errorf("k3s %s path = %q, want %q", p.name, p.k3s, p.expect[0])
		}
		if p.rke2 != p.expect[1] {
			t.Errorf("rke2 %s path = %q, want %q", p.name, p.rke2, p.expect[1])
		}
		if p.k3s == p.rke2 {
			t.Errorf("%s path is identical across distributions", p.name)
		}
	}
}

func TestServiceNamesDifferByRole(t *testing.T) {
	k, _ := DistroFor(config.DistributionK3s)
	if got := k.ServiceName(inventory.RoleControlPlane); got != "k3s" {
		t.Errorf("k3s control-plane service = %q", got)
	}
	// Restarting the wrong unit would silently do nothing on an agent.
	if got := k.ServiceName(inventory.RoleWorker); got != "k3s-agent" {
		t.Errorf("k3s worker service = %q", got)
	}

	r, _ := DistroFor(config.DistributionRKE2)
	if got := r.ServiceName(inventory.RoleControlPlane); got != "rke2-server" {
		t.Errorf("rke2 control-plane service = %q", got)
	}
	if got := r.ServiceName(inventory.RoleWorker); got != "rke2-agent" {
		t.Errorf("rke2 worker service = %q", got)
	}
}

func TestInstallCommandsSelectMode(t *testing.T) {
	k, _ := DistroFor(config.DistributionK3s)

	server := k.InstallCommand(inventory.RoleControlPlane, "v1.34.1+k3s1")
	if !strings.Contains(server, "INSTALL_K3S_EXEC=server") {
		t.Errorf("k3s control-plane install should run in server mode: %s", server)
	}
	if !strings.Contains(server, "INSTALL_K3S_VERSION=v1.34.1+k3s1") {
		t.Errorf("version should be pinned: %s", server)
	}

	agent := k.InstallCommand(inventory.RoleWorker, "")
	if !strings.Contains(agent, "INSTALL_K3S_EXEC=agent") {
		t.Errorf("k3s worker install should run in agent mode: %s", agent)
	}
	// An empty version must not emit an empty pin, which the installer rejects.
	if strings.Contains(agent, "INSTALL_K3S_VERSION=") {
		t.Errorf("unpinned install should omit the version variable: %s", agent)
	}

	r, _ := DistroFor(config.DistributionRKE2)
	rkeServer := r.InstallCommand(inventory.RoleControlPlane, "v1.34.1+rke2r1")
	if !strings.Contains(rkeServer, "INSTALL_RKE2_TYPE=server") {
		t.Errorf("rke2 install type: %s", rkeServer)
	}
}

func TestRKE2NeedsExplicitStartAndK3sDoesNot(t *testing.T) {
	k, _ := DistroFor(config.DistributionK3s)
	// The k3s installer enables and starts the unit itself.
	if got := k.StartCommand(inventory.RoleControlPlane); got != "" {
		t.Errorf("k3s should not need a start command, got %q", got)
	}

	r, _ := DistroFor(config.DistributionRKE2)
	// RKE2 deliberately leaves the unit stopped after install.
	start := r.StartCommand(inventory.RoleControlPlane)
	if !strings.Contains(start, "systemctl enable --now rke2-server") {
		t.Errorf("rke2 must be started explicitly, got %q", start)
	}
	if !strings.Contains(r.StartCommand(inventory.RoleWorker), "rke2-agent") {
		t.Errorf("rke2 worker start should target the agent unit, got %q", r.StartCommand(inventory.RoleWorker))
	}
}

func TestKubectlCommandsAreUsable(t *testing.T) {
	k, _ := DistroFor(config.DistributionK3s)
	if got := k.KubectlCommand(); got != "k3s kubectl" {
		t.Errorf("k3s kubectl = %q", got)
	}

	r, _ := DistroFor(config.DistributionRKE2)
	rkeKubectl := r.KubectlCommand()
	// RKE2's kubectl is not on PATH and needs KUBECONFIG set.
	if !strings.Contains(rkeKubectl, "/var/lib/rancher/rke2/bin/kubectl") {
		t.Errorf("rke2 kubectl should use the bundled binary: %q", rkeKubectl)
	}
	if !strings.Contains(rkeKubectl, "KUBECONFIG=") {
		t.Errorf("rke2 kubectl needs KUBECONFIG: %q", rkeKubectl)
	}
}

func TestUninstallCommandsDifferByRoleForK3s(t *testing.T) {
	k, _ := DistroFor(config.DistributionK3s)
	// k3s ships separate scripts; running the server one on an agent fails.
	if got := k.UninstallCommand(inventory.RoleControlPlane); got != "/usr/local/bin/k3s-uninstall.sh" {
		t.Errorf("k3s control-plane uninstall = %q", got)
	}
	if got := k.UninstallCommand(inventory.RoleWorker); got != "/usr/local/bin/k3s-agent-uninstall.sh" {
		t.Errorf("k3s worker uninstall = %q", got)
	}

	r, _ := DistroFor(config.DistributionRKE2)
	// RKE2 ships one script for both roles.
	if r.UninstallCommand(inventory.RoleControlPlane) != r.UninstallCommand(inventory.RoleWorker) {
		t.Error("rke2 uses a single uninstall script for both roles")
	}
}

func TestAddonsRespectConfiguration(t *testing.T) {
	mgr := &Manager{
		infra: &config.Infra{
			Kubernetes: config.ClusterKubernetes{Distribution: config.DistributionK3s},
			Addons: config.Addons{
				BuildKit:         true,
				CertManager:      true,
				CertManagerEmail: "ops@acme.com",
				MetricsServer:    true,
			},
		},
	}

	names := mgr.AddonNames()
	for _, want := range []string{"cert-manager", "metrics-server", "buildkit"} {
		if !containsString(names, want) {
			t.Errorf("addons %v should include %q", names, want)
		}
	}
	// cert-manager must come first: a deploy's Ingress references its CRDs.
	if names[0] != "cert-manager" {
		t.Errorf("cert-manager should install first, got order %v", names)
	}

	none := &Manager{infra: &config.Infra{}}
	if got := none.AddonSummary(); got != "none" {
		t.Errorf("AddonSummary with no addons = %q", got)
	}
}

func TestIngressClassMatchesDistribution(t *testing.T) {
	// The ACME http01 solver must name the controller that actually exists, or
	// certificate challenges never reach the cluster.
	k3sMgr := &Manager{infra: &config.Infra{
		Kubernetes: config.ClusterKubernetes{Distribution: config.DistributionK3s},
	}}
	if got := k3sMgr.defaultIngressClass(); got != "traefik" {
		t.Errorf("k3s ingress class = %q, want traefik", got)
	}

	rkeMgr := &Manager{infra: &config.Infra{
		Kubernetes: config.ClusterKubernetes{Distribution: config.DistributionRKE2},
	}}
	if got := rkeMgr.defaultIngressClass(); got != "nginx" {
		t.Errorf("rke2 ingress class = %q, want nginx", got)
	}

	// Disabling traefik means the user runs their own controller.
	disabled := &Manager{infra: &config.Infra{
		Kubernetes: config.ClusterKubernetes{
			Distribution: config.DistributionK3s,
			Disable:      []string{"traefik"},
		},
	}}
	if got := disabled.defaultIngressClass(); got == "traefik" {
		t.Error("ingress class should not be traefik when traefik is disabled")
	}
}

func TestCertManagerAddonCreatesIssuer(t *testing.T) {
	addon := certManagerAddon("ops@acme.com", "traefik")

	if !strings.Contains(addon.Manifest, "letsencrypt-prod") {
		t.Error("the ClusterIssuer must be named letsencrypt-prod to match proxy.ssl")
	}
	if !strings.Contains(addon.Manifest, "ops@acme.com") {
		t.Error("the ACME contact address should be included")
	}
	if !strings.Contains(addon.Manifest, "class: traefik") {
		t.Error("the http01 solver should name the ingress class")
	}
	// The webhook must be ready before a ClusterIssuer is accepted.
	joined := strings.Join(addon.Commands, "\n")
	if !strings.Contains(joined, "rollout status deployment/cert-manager-webhook") {
		t.Error("cert-manager install should wait for the webhook")
	}
	// Commands must be distribution-agnostic.
	if !strings.Contains(joined, kubectlPlaceholder) {
		t.Error("addon commands should use the kubectl placeholder, not a hardcoded kubectl")
	}
	if strings.Contains(joined, "\nkubectl ") || strings.HasPrefix(joined, "kubectl ") {
		t.Error("addon commands must not hardcode `kubectl`; k3s and rke2 differ")
	}
}

func TestBuildKitAddonIsRootless(t *testing.T) {
	addon := buildKitAddon()

	// A privileged buildkitd is effectively root on the node, and a build runs
	// the least trustworthy code in the system.
	if strings.Contains(addon.Manifest, "privileged: true") {
		t.Error("the buildkit addon must not run privileged")
	}
	if !strings.Contains(addon.Manifest, "rootless") {
		t.Error("expected the rootless buildkit image")
	}
	if !strings.Contains(addon.Manifest, "runAsUser: 1000") {
		t.Error("buildkitd should run as an unprivileged user")
	}
	// It must be reachable from outside the pod for buidl to drive it.
	if !strings.Contains(addon.Manifest, "tcp://0.0.0.0:1234") {
		t.Error("buildkitd should listen on TCP for remote builds")
	}
	if !strings.Contains(addon.Manifest, "kind: Namespace") {
		t.Error("the manifest should create its own namespace")
	}
}

func TestBuildKitAddressMatchesManifest(t *testing.T) {
	addr := BuildKitAddress()
	// A mismatch here would print setup instructions that do not work.
	if !strings.Contains(addr, "buildkitd") {
		t.Errorf("BuildKitAddress = %q, want the service name", addr)
	}
	if !strings.Contains(addr, config.DefaultBuildKitNS) {
		t.Errorf("BuildKitAddress = %q, want the addon namespace", addr)
	}
	if !strings.Contains(addr, ":1234") {
		t.Errorf("BuildKitAddress = %q, want the service port", addr)
	}
}

func TestNewRequiresInfra(t *testing.T) {
	_, err := New(nil, nil)
	if err == nil {
		t.Fatal("expected an error with no infra block")
	}
	// The error should show what to add, since this is the first thing a user
	// hits when trying cluster commands.
	if !strings.Contains(err.Error(), "infra:") {
		t.Errorf("error should include an example config, got: %v", err)
	}
}

func TestGenerateTokenIsRandomAndLongEnough(t *testing.T) {
	seen := map[string]bool{}
	for range 50 {
		token, err := generateToken()
		if err != nil {
			t.Fatalf("generateToken: %v", err)
		}
		// A short or repeated token would be a cluster-admin-equivalent weakness.
		if len(token) < 64 {
			t.Errorf("token %q is too short", token)
		}
		if seen[token] {
			t.Fatalf("generateToken returned a duplicate: %q", token)
		}
		seen[token] = true
	}
}
