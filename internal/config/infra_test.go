package config

import (
	"strings"
	"testing"

	"github.com/danecwalker/buidl/internal/inventory"
)

const infraBase = `
app: api
image: ghcr.io/acme/api
infra:
  servers:
    - {host: 10.0.0.1, role: control-plane}
    - {host: 10.0.1.1, role: worker}
`

func TestInfraDefaults(t *testing.T) {
	res, err := Load(LoadOptions{Path: write(t, infraBase), Strict: true})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	in := res.Config.Infra
	if in == nil {
		t.Fatal("Infra should be populated")
	}

	if in.Provider != "static" {
		t.Errorf("provider = %q, want static", in.Provider)
	}
	if in.Kubernetes.Distribution != DistributionK3s {
		t.Errorf("distribution = %q, want k3s", in.Kubernetes.Distribution)
	}
	if in.SSH.User != DefaultSSHUser {
		t.Errorf("ssh user = %q, want %s", in.SSH.User, DefaultSSHUser)
	}
	if in.SSH.Port != DefaultSSHPort {
		t.Errorf("ssh port = %d", in.SSH.Port)
	}
	// Verifying host keys must be the default: bootstrap installs root software
	// and copies back a cluster-admin credential.
	if in.SSH.AcceptNewHostKeys {
		t.Error("acceptNewHostKeys must default to false")
	}
	if in.Kubernetes.ClusterCIDR != DefaultClusterCIDR {
		t.Errorf("clusterCIDR = %q", in.Kubernetes.ClusterCIDR)
	}
}

func TestInfraAbsentIsValid(t *testing.T) {
	// Against a managed cluster, buidl only deploys and never touches servers.
	res, err := Load(LoadOptions{Path: write(t, "app: api\nimage: ghcr.io/acme/api\n"), Strict: true})
	if err != nil {
		t.Fatalf("a config without infra must be valid: %v", err)
	}
	if res.Config.Infra != nil {
		t.Error("Infra should stay nil when not configured")
	}
}

func TestNonRootUserEnablesSudo(t *testing.T) {
	res, err := Load(LoadOptions{Path: write(t, `
app: api
image: ghcr.io/acme/api
infra:
  ssh:
    user: ubuntu
  servers:
    - {host: 10.0.0.1, role: control-plane}
`), Strict: true})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Installing Kubernetes needs root; a non-root user without sudo could never
	// succeed, so inferring it beats failing later on the server.
	if !res.Config.Infra.SSH.Sudo {
		t.Error("sudo should be enabled automatically for a non-root user")
	}
}

func TestSingleServerRoleInferredThroughConfig(t *testing.T) {
	res, err := Load(LoadOptions{Path: write(t, `
app: api
image: ghcr.io/acme/api
infra:
  servers:
    - {host: 10.0.0.1}
`), Strict: true})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := res.Config.Infra.Servers[0].Role; got != inventory.RoleControlPlane {
		t.Errorf("role = %q, want control-plane", got)
	}
}

func TestInfraValidation(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name: "unsupported provider",
			yaml: `
app: api
image: ghcr.io/acme/api
infra:
  provider: tofu
  servers:
    - {host: 10.0.0.1, role: control-plane}
`,
			wantErr: "not supported yet",
		},
		{
			name: "unknown distribution",
			yaml: `
app: api
image: ghcr.io/acme/api
infra:
  kubernetes:
    distribution: openshift
  servers:
    - {host: 10.0.0.1, role: control-plane}
`,
			wantErr: "must be \"k3s\" or \"rke2\"",
		},
		{
			name: "HA without endpoint",
			yaml: `
app: api
image: ghcr.io/acme/api
infra:
  servers:
    - {host: 10.0.0.1, role: control-plane}
    - {host: 10.0.0.2, role: control-plane}
    - {host: 10.0.0.3, role: control-plane}
`,
			wantErr: "controlPlaneEndpoint` is required",
		},
		{
			name: "no control plane",
			yaml: `
app: api
image: ghcr.io/acme/api
infra:
  servers:
    - {host: 10.0.0.1, role: worker}
    - {host: 10.0.0.2, role: worker}
`,
			wantErr: "at least one server must have role: control-plane",
		},
		{
			name: "cert-manager without email",
			yaml: `
app: api
image: ghcr.io/acme/api
infra:
  addons:
    certManager: true
  servers:
    - {host: 10.0.0.1, role: control-plane}
`,
			wantErr: "certManagerEmail` is required",
		},
		{
			name: "ssl without cert-manager",
			yaml: `
app: api
image: ghcr.io/acme/api
proxy:
  host: api.acme.com
  ssl: true
infra:
  servers:
    - {host: 10.0.0.1, role: control-plane}
`,
			wantErr: "infra.addons.certManager` is not",
		},
		{
			name: "autoscale on rke2 without metrics-server",
			yaml: `
app: api
image: ghcr.io/acme/api
deploy:
  autoscale: {min: 1, max: 5, cpuPercent: 70}
infra:
  kubernetes:
    distribution: rke2
  servers:
    - {host: 10.0.0.1, role: control-plane}
`,
			wantErr: "needs metrics-server",
		},
		{
			name: "autoscale when metrics-server is disabled",
			yaml: `
app: api
image: ghcr.io/acme/api
deploy:
  autoscale: {min: 1, max: 5, cpuPercent: 70}
infra:
  kubernetes:
    disable: [metrics-server]
  servers:
    - {host: 10.0.0.1, role: control-plane}
`,
			wantErr: "needs metrics-server",
		},
		{
			name: "invalid ssh port",
			yaml: `
app: api
image: ghcr.io/acme/api
infra:
  ssh:
    port: 70000
  servers:
    - {host: 10.0.0.1, role: control-plane}
`,
			wantErr: "infra.ssh.port",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(LoadOptions{Path: write(t, tt.yaml), Strict: true})
			if err == nil {
				t.Fatalf("expected an error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestAutoscaleAcceptedOnK3sWithBundledMetrics(t *testing.T) {
	// k3s bundles metrics-server, so requiring the addon would be wrong.
	_, err := Load(LoadOptions{Path: write(t, `
app: api
image: ghcr.io/acme/api
deploy:
  autoscale: {min: 1, max: 5, cpuPercent: 70}
infra:
  kubernetes:
    distribution: k3s
  servers:
    - {host: 10.0.0.1, role: control-plane}
`), Strict: true})
	if err != nil {
		t.Errorf("autoscale on k3s should be valid without the addon: %v", err)
	}
}

func TestFullInfraConfigIsValid(t *testing.T) {
	_, err := Load(LoadOptions{Path: write(t, `
app: api
image: ghcr.io/acme/api
deploy:
  autoscale: {min: 3, max: 12, cpuPercent: 70}
proxy:
  host: api.acme.com
  ssl: true
infra:
  provider: static
  ssh:
    user: root
    port: 22
    keyPath: ~/.ssh/id_ed25519
    acceptNewHostKeys: false
  kubernetes:
    distribution: rke2
    version: v1.34.1+rke2r1
    controlPlaneEndpoint: k8s.acme.com
    tlsSANs: [api.internal]
    clusterCIDR: 10.42.0.0/16
    serviceCIDR: 10.43.0.0/16
    disable: [rke2-ingress-nginx]
    extraArgs:
      kubelet-arg: max-pods=200
  addons:
    buildkit: true
    certManager: true
    certManagerEmail: ops@acme.com
    metricsServer: true
  servers:
    - {host: 203.0.113.1, role: control-plane, privateIP: 10.0.0.1, name: cp-1}
    - {host: 203.0.113.2, role: control-plane, privateIP: 10.0.0.2, name: cp-2}
    - {host: 203.0.113.3, role: control-plane, privateIP: 10.0.0.3, name: cp-3}
    - {host: 203.0.113.10, role: worker, privateIP: 10.0.1.1, labels: {pool: web}}
    - {host: 203.0.113.11, role: worker, privateIP: 10.0.1.2, labels: {pool: gpu}, taints: ["gpu=true:NoSchedule"]}
`), Strict: true})
	if err != nil {
		t.Fatalf("a maximal infra config should be valid: %v", err)
	}
}

func TestInfraOverlaysPerEnvironment(t *testing.T) {
	// Staging and production should be able to target entirely different clusters.
	path := write(t, `
app: api
image: ghcr.io/acme/api
infra:
  kubernetes:
    distribution: k3s
  servers:
    - {host: 10.0.0.1, role: control-plane}
environments:
  staging:
    infra:
      servers:
        - {host: 10.1.0.1, role: control-plane}
  production:
    infra:
      kubernetes:
        distribution: rke2
        controlPlaneEndpoint: k8s.acme.com
      servers:
        - {host: 10.2.0.1, role: control-plane}
        - {host: 10.2.0.2, role: control-plane}
        - {host: 10.2.0.3, role: control-plane}
`)

	staging, err := Load(LoadOptions{Path: path, Environment: "staging", Strict: true})
	if err != nil {
		t.Fatalf("staging: %v", err)
	}
	if got := staging.Config.Infra.Servers[0].Host; got != "10.1.0.1" {
		t.Errorf("staging server = %q", got)
	}
	// Sequences are replaced, so staging has exactly one server.
	if n := len(staging.Config.Infra.Servers); n != 1 {
		t.Errorf("staging servers = %d, want 1", n)
	}
	if got := staging.Config.Infra.Kubernetes.Distribution; got != DistributionK3s {
		t.Errorf("staging distribution = %q, want the inherited k3s", got)
	}

	prod, err := Load(LoadOptions{Path: path, Environment: "production", Strict: true})
	if err != nil {
		t.Fatalf("production: %v", err)
	}
	if n := len(prod.Config.Infra.Servers); n != 3 {
		t.Errorf("production servers = %d, want 3", n)
	}
	if got := prod.Config.Infra.Kubernetes.Distribution; got != DistributionRKE2 {
		t.Errorf("production distribution = %q, want the overridden rke2", got)
	}
}

func TestInventoryProviderIsStatic(t *testing.T) {
	res, err := Load(LoadOptions{Path: write(t, infraBase), Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	provider := res.Config.Infra.InventoryProvider()
	if provider.Name() != "static" {
		t.Errorf("provider = %q, want static", provider.Name())
	}
}
