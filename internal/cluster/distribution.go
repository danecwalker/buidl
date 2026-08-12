// Package cluster turns a fleet of servers into a Kubernetes cluster.
//
// buidl does not provision machines — see internal/inventory for why. It takes
// servers that exist and installs a Kubernetes distribution on them: bootstrap
// the first control plane, join any additional control planes, join the workers,
// then fetch a kubeconfig so `buidl deploy` works against the result.
//
// Both supported distributions (k3s and RKE2) share the same shape: install a
// binary via a shell script, write a config file, start a systemd unit, and join
// followers with a shared token read from the bootstrap node. That shared shape
// is what the Distro interface captures, and it is why supporting the second
// distribution costs little.
package cluster

import (
	"fmt"
	"strings"

	"github.com/danewalker/buidl/internal/config"
	"github.com/danewalker/buidl/internal/inventory"
)

// Distro abstracts the differences between Kubernetes distributions.
type Distro interface {
	// Name is the distribution identifier.
	Name() string

	// ConfigPath is where the node's configuration file lives.
	ConfigPath() string
	// TokenPath is where the bootstrap node writes the cluster join token.
	TokenPath() string
	// KubeconfigPath is where the admin kubeconfig is written on a server node.
	KubeconfigPath() string

	// ServiceName is the systemd unit for a role.
	ServiceName(role inventory.Role) string

	// InstallCommand returns a shell command that installs the distribution for a
	// role. It must be idempotent: re-running on an installed node is a no-op or
	// an in-place upgrade.
	InstallCommand(role inventory.Role, version string) string

	// StartCommand returns a command to enable and start the unit, or "" when the
	// installer already does it.
	StartCommand(role inventory.Role) string

	// KubectlCommand returns a command prefix for running kubectl on a server
	// node, so addons can be applied without a local kubectl.
	KubectlCommand() string

	// UninstallCommand removes the distribution from a node.
	UninstallCommand(role inventory.Role) string
}

// DistroFor returns the driver for a configured distribution.
func DistroFor(d config.Distribution) (Distro, error) {
	switch d {
	case config.DistributionK3s:
		return k3s{}, nil
	case config.DistributionRKE2:
		return rke2{}, nil
	default:
		return nil, fmt.Errorf("unsupported distribution %q", d)
	}
}

// --- k3s -------------------------------------------------------------------

// k3s is Rancher's lightweight distribution: one binary, embedded etcd.
type k3s struct{}

func (k3s) Name() string           { return "k3s" }
func (k3s) ConfigPath() string     { return "/etc/rancher/k3s/config.yaml" }
func (k3s) TokenPath() string      { return "/var/lib/rancher/k3s/server/token" }
func (k3s) KubeconfigPath() string { return "/etc/rancher/k3s/k3s.yaml" }

func (k3s) ServiceName(role inventory.Role) string {
	if role == inventory.RoleControlPlane {
		return "k3s"
	}
	return "k3s-agent"
}

func (k3s) InstallCommand(role inventory.Role, version string) string {
	exec := "server"
	if role != inventory.RoleControlPlane {
		exec = "agent"
	}
	// All behavior comes from the config file written beforehand, so the installer
	// only needs to know which mode to run in. That keeps the command identical
	// across nodes and makes re-runs idempotent.
	env := []string{
		"INSTALL_K3S_EXEC=" + exec,
	}
	if version != "" {
		env = append(env, "INSTALL_K3S_VERSION="+version)
	}
	return fmt.Sprintf("curl -sfL https://get.k3s.io | %s sh -s -", strings.Join(env, " "))
}

// StartCommand is empty: the k3s installer enables and starts the unit itself.
func (k3s) StartCommand(inventory.Role) string { return "" }

func (k3s) KubectlCommand() string { return "k3s kubectl" }

func (k3s) UninstallCommand(role inventory.Role) string {
	if role == inventory.RoleControlPlane {
		return "/usr/local/bin/k3s-uninstall.sh"
	}
	return "/usr/local/bin/k3s-agent-uninstall.sh"
}

// --- RKE2 ------------------------------------------------------------------

// rke2 is Rancher's security-hardened distribution, with CIS-aligned defaults
// and no bundled ingress.
type rke2 struct{}

func (rke2) Name() string           { return "rke2" }
func (rke2) ConfigPath() string     { return "/etc/rancher/rke2/config.yaml" }
func (rke2) TokenPath() string      { return "/var/lib/rancher/rke2/server/token" }
func (rke2) KubeconfigPath() string { return "/etc/rancher/rke2/rke2.yaml" }

func (rke2) ServiceName(role inventory.Role) string {
	if role == inventory.RoleControlPlane {
		return "rke2-server"
	}
	return "rke2-agent"
}

func (rke2) InstallCommand(role inventory.Role, version string) string {
	installType := "server"
	if role != inventory.RoleControlPlane {
		installType = "agent"
	}
	env := []string{"INSTALL_RKE2_TYPE=" + installType}
	if version != "" {
		env = append(env, "INSTALL_RKE2_VERSION="+version)
	}
	return fmt.Sprintf("curl -sfL https://get.rke2.io | %s sh -", strings.Join(env, " "))
}

// StartCommand is required: unlike k3s, the RKE2 installer does not start the
// unit, leaving the operator to review configuration first.
func (r rke2) StartCommand(role inventory.Role) string {
	return "systemctl enable --now " + r.ServiceName(role)
}

// KubectlCommand points at the bundled kubectl, which is not on PATH by default.
func (rke2) KubectlCommand() string {
	return "KUBECONFIG=/etc/rancher/rke2/rke2.yaml /var/lib/rancher/rke2/bin/kubectl"
}

func (rke2) UninstallCommand(inventory.Role) string {
	// RKE2 ships a single uninstall script for both roles.
	return "/usr/local/bin/rke2-uninstall.sh"
}
