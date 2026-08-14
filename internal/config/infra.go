package config

import (
	"github.com/danecwalker/buidl/internal/inventory"
)

// Infra describes the machines behind a cluster and how buidl should turn them
// into one.
//
// The division of labor is deliberate: buidl never provisions infrastructure.
// OpenTofu, Terraform, Ansible and cloud consoles already create VMs, networks
// and firewalls well, and they own that state. buidl takes machines that exist
// and makes them a cluster it can deploy to.
type Infra struct {
	// Provider selects how the server list is discovered. Only "static" is
	// implemented; the inventory layer is an interface so providers that read
	// `tofu output -json`, an Ansible inventory, or an arbitrary script can be
	// added without touching the cluster code.
	Provider string `yaml:"provider"`

	// Servers is the fleet, used when Provider is "static".
	Servers []inventory.Server `yaml:"servers"`

	SSH        SSH               `yaml:"ssh"`
	Kubernetes ClusterKubernetes `yaml:"kubernetes"`
	Addons     Addons            `yaml:"addons"`
}

// SSH configures how buidl reaches the machines.
type SSH struct {
	// User defaults to root, which is what freshly provisioned cloud images
	// typically provide and what installing a Kubernetes distribution requires.
	User string `yaml:"user"`
	Port int    `yaml:"port"`

	// KeyPath is the private key to authenticate with. When empty, buidl tries
	// the running ssh-agent and then the conventional key locations.
	KeyPath string `yaml:"keyPath"`

	// KnownHosts overrides the host key database. Defaults to
	// ~/.ssh/known_hosts.
	KnownHosts string `yaml:"knownHosts"`

	// AcceptNewHostKeys records and trusts host keys not yet in known_hosts.
	//
	// This defaults to false, and that default is a security decision worth
	// stating: cluster bootstrap installs root-level software and copies a
	// cluster-admin credential back. Blindly trusting an unknown key would open a
	// machine-in-the-middle window at exactly the worst moment. Fresh servers do
	// have unknown keys, so buidl's error explains how to pre-seed them.
	AcceptNewHostKeys bool `yaml:"acceptNewHostKeys"`

	// Sudo runs privileged commands through sudo. Required when User is not root.
	Sudo bool `yaml:"sudo"`
}

// Distribution selects which Kubernetes distribution to install.
type Distribution string

const (
	// DistributionK3s is a single-binary, fully conformant Kubernetes with
	// embedded etcd. The default: it installs in minutes and needs no external
	// etcd, load balancer, or CNI decision to get a working cluster.
	DistributionK3s Distribution = "k3s"
	// DistributionRKE2 is Rancher's security-hardened distribution. Same
	// token-and-join model as k3s, with CIS-aligned defaults and no bundled
	// ingress. Choose it when a compliance baseline is a requirement.
	DistributionRKE2 Distribution = "rke2"
)

// ClusterKubernetes configures the cluster buidl installs.
type ClusterKubernetes struct {
	Distribution Distribution `yaml:"distribution"`

	// Version pins the distribution release, e.g. "v1.34.1+k3s1". Empty installs
	// the distribution's current stable, which is convenient for a first cluster
	// but means two deploys months apart can install different
	// versions — pin it for anything you intend to keep.
	Version string `yaml:"version"`

	// ControlPlaneEndpoint is the stable address clients and joining nodes use to
	// reach the API server. Required for a multi-control-plane cluster: without
	// it every node would hard-code the bootstrap machine's address, and losing
	// that machine would break joins and kubeconfigs even though the cluster
	// itself survived.
	ControlPlaneEndpoint string `yaml:"controlPlaneEndpoint"`

	// TLSSANs are additional names to include in the API server certificate.
	// ControlPlaneEndpoint and every server address are added automatically.
	TLSSANs []string `yaml:"tlsSANs"`

	// ClusterCIDR is the pod network, k3s/RKE2 style: one CIDR or IPv4,IPv6.
	// The default is dual-stack. Let's Encrypt prefers IPv6 whenever a name has
	// an AAAA, so an IPv4-only cluster with a public AAAA cannot complete
	// HTTP-01 and never gets a certificate.
	ClusterCIDR string `yaml:"clusterCIDR"`
	// ServiceCIDR is the Service network. Must cover the same IP families as
	// ClusterCIDR.
	ServiceCIDR string `yaml:"serviceCIDR"`

	// Disable turns off bundled components, e.g. [traefik] to run a different
	// ingress controller.
	Disable []string `yaml:"disable"`

	// ExtraArgs are appended to the distribution's server/agent configuration
	// verbatim, as config-file keys.
	ExtraArgs map[string]string `yaml:"extraArgs"`

	// Token is the shared secret nodes use to join. When empty, buidl reads the
	// token the bootstrap node generated, which is the recommended path — a token
	// in a config file is a cluster-admin-equivalent credential.
	Token string `yaml:"token"`
}

// Addons are cluster components buidl can install after bootstrap.
type Addons struct {
	// BuildKit installs an in-cluster rootless buildkitd, so `buidl deploy` works
	// immediately once the cluster exists, with no builder to set up separately
	// and no privileged CI runner.
	BuildKit bool `yaml:"buildkit"`

	// CertManager installs cert-manager, required by proxy.ssl.
	CertManager bool `yaml:"certManager"`

	// CertManagerEmail is the ACME registration address used by the ClusterIssuer
	// that CertManager creates. Let's Encrypt requires one.
	CertManagerEmail string `yaml:"certManagerEmail"`

	// MetricsServer installs metrics-server, required by deploy.autoscale. k3s
	// bundles it, so this is only needed on RKE2 or when it has been disabled.
	MetricsServer bool `yaml:"metricsServer"`
}

// Defaults applied to the infra block.
const (
	DefaultSSHUser = "root"
	DefaultSSHPort = 22
	// Dual-stack defaults. Pod/service IPv6 is ULA so it does not consume the
	// host's public /64; flannel masquerades egress to the node's address.
	DefaultClusterCIDR = "10.42.0.0/16,fd00:42::/56"
	DefaultServiceCIDR = "10.43.0.0/16,fd00:43::/112"
	DefaultBuildKitNS  = "buidl-system"
)

// applyInfraDefaults fills in omitted infra fields.
func applyInfraDefaults(c *Config) {
	if c.Infra == nil {
		return
	}
	in := c.Infra

	if in.Provider == "" {
		in.Provider = "static"
	}
	if in.SSH.User == "" {
		in.SSH.User = DefaultSSHUser
	}
	if in.SSH.Port == 0 {
		in.SSH.Port = DefaultSSHPort
	}
	// Any non-root user needs sudo to install system packages and services.
	if in.SSH.User != "root" && !in.SSH.Sudo {
		in.SSH.Sudo = true
	}

	if in.Kubernetes.Distribution == "" {
		in.Kubernetes.Distribution = DistributionK3s
	}
	if in.Kubernetes.ClusterCIDR == "" {
		in.Kubernetes.ClusterCIDR = DefaultClusterCIDR
	}
	if in.Kubernetes.ServiceCIDR == "" {
		in.Kubernetes.ServiceCIDR = DefaultServiceCIDR
	}

	// Normalize roles so a single-server list yields a working control plane.
	inv := &inventory.Inventory{Servers: in.Servers}
	inventory.Normalize(inv)
	in.Servers = inv.Servers
}

// validateInfra checks the infra block.
func validateInfra(c *Config, add func(string, ...any)) {
	if c.Infra == nil {
		return
	}
	in := c.Infra

	if in.Provider != "static" {
		add("`infra.provider` %q is not supported yet (only \"static\" is implemented)", in.Provider)
	}

	switch in.Kubernetes.Distribution {
	case DistributionK3s, DistributionRKE2:
	default:
		add("`infra.kubernetes.distribution` must be %q or %q (got %q)",
			DistributionK3s, DistributionRKE2, in.Kubernetes.Distribution)
	}

	if in.SSH.Port < 1 || in.SSH.Port > 65535 {
		add("`infra.ssh.port` must be between 1 and 65535 (got %d)", in.SSH.Port)
	}
	if in.SSH.User != "root" && !in.SSH.Sudo {
		add("`infra.ssh.user` is %q but sudo is disabled; installing Kubernetes requires root", in.SSH.User)
	}

	inv := &inventory.Inventory{Servers: in.Servers}
	if err := inventory.Validate(inv); err != nil {
		add("%s", err)
		return
	}

	// A multi-control-plane cluster without a stable endpoint would bake the
	// bootstrap node's address into every joining node and kubeconfig.
	if inv.HighlyAvailable() && in.Kubernetes.ControlPlaneEndpoint == "" {
		add("`infra.kubernetes.controlPlaneEndpoint` is required with %d control-plane servers "+
			"(point a load balancer or DNS record at them)", len(inv.ControlPlanes()))
	}

	if in.Addons.CertManager && in.Addons.CertManagerEmail == "" {
		add("`infra.addons.certManagerEmail` is required when certManager is enabled (ACME needs a contact address)")
	}

	if err := checkDualStackCIDRs(in.Kubernetes.ClusterCIDR, in.Kubernetes.ServiceCIDR); err != nil {
		add("%s", err)
	}

	// proxy.ssl depends on cert-manager existing in the cluster.
	if c.Proxy.SSL && !in.Addons.CertManager {
		add("`proxy.ssl` is enabled but `infra.addons.certManager` is not; " +
			"enable it, or install cert-manager yourself")
	}
	// deploy.autoscale depends on metrics-server. k3s bundles it unless disabled.
	if c.Deploy.Autoscale != nil && !in.Addons.MetricsServer {
		bundled := in.Kubernetes.Distribution == DistributionK3s && !slicesContains(in.Kubernetes.Disable, "metrics-server")
		if !bundled {
			add("`deploy.autoscale` needs metrics-server; enable `infra.addons.metricsServer`")
		}
	}
}

// slicesContains reports whether s contains v.
func slicesContains(s []string, v string) bool {
	for _, item := range s {
		if item == v {
			return true
		}
	}
	return false
}

// InventoryProvider builds the provider described by this config.
func (in *Infra) InventoryProvider() inventory.Provider {
	// Only static is implemented; validation rejects anything else, so this is a
	// total function over valid configs.
	return inventory.Static{Servers: in.Servers}
}
