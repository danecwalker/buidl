package cluster

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/danewalker/buidl/internal/config"
	"github.com/danewalker/buidl/internal/inventory"
	"github.com/danewalker/buidl/internal/remote"
)

// kubectlPlaceholder is substituted with the distribution's kubectl invocation
// when an addon's commands are run.
//
// Addons cannot hardcode "kubectl": k3s exposes it as `k3s kubectl`, and RKE2
// ships it at an absolute path that is not on PATH.
const kubectlPlaceholder = "{{kubectl}}"

// Addon is a cluster component buidl can install after bootstrap.
type Addon struct {
	Name string
	// Description explains what enabling it gets you.
	Description string
	// Commands run on a control-plane node. Occurrences of kubectlPlaceholder are
	// replaced with the distribution's kubectl invocation.
	Commands []string
	// Manifest is applied after Commands, for resources that depend on CRDs the
	// commands install.
	Manifest string
	// Present is a command that exits zero when the addon is already installed.
	//
	// Addons are checked on every deploy rather than only when the cluster is
	// first built, because a configured addon that is missing is a silent
	// failure: proxy.ssl renders an Ingress annotated for an issuer that does not
	// exist, and the site quietly serves the ingress controller's self-signed
	// certificate. This check keeps that verification cheap enough to always do.
	Present string
}

// Addons returns the enabled addons in dependency order.
func (m *Manager) Addons() []Addon {
	var out []Addon
	cfg := m.infra.Addons

	// cert-manager first: an Ingress created by a deploy references a
	// ClusterIssuer, so the CRDs must exist before any app is deployed.
	if cfg.CertManager {
		out = append(out, certManagerAddon(cfg.CertManagerEmail, m.defaultIngressClass()))
	}
	if cfg.MetricsServer {
		out = append(out, metricsServerAddon())
	}
	if cfg.BuildKit {
		out = append(out, buildKitAddon())
	}
	return out
}

// defaultIngressClass reports which ingress controller the cluster will have.
//
// k3s bundles Traefik unless it was disabled; RKE2 bundles ingress-nginx. The
// ACME http01 solver must name the right one or certificate challenges never
// reach the cluster.
func (m *Manager) defaultIngressClass() string {
	if m.infra.Kubernetes.Distribution == config.DistributionRKE2 {
		return "nginx"
	}
	for _, disabled := range m.infra.Kubernetes.Disable {
		if disabled == "traefik" {
			// Traefik was turned off, so the user is running their own controller.
			// nginx is the most common choice; they can override with extraArgs.
			return "nginx"
		}
	}
	return "traefik"
}

// ApplyAddons installs the enabled addons via a control-plane node.
//
// Applying through the node rather than a local client means addons install
// immediately after bootstrap, before any kubeconfig has been fetched, and works
// from any machine with SSH access.
func (m *Manager) ApplyAddons(ctx context.Context) error {
	addons := m.Addons()
	if len(addons) == 0 {
		return nil
	}

	inv, err := m.provider.Resolve(ctx)
	if err != nil {
		return err
	}
	bootstrap, err := inv.Bootstrap()
	if err != nil {
		return err
	}
	client, err := m.connect(ctx, bootstrap)
	if err != nil {
		return err
	}

	kubectl := m.distro.KubectlCommand()

	for _, addon := range addons {
		if addon.Present != "" {
			check := strings.ReplaceAll(addon.Present, kubectlPlaceholder, kubectl)
			if res, err := client.TrySudo(ctx, check+" >/dev/null 2>&1"); err == nil && res.ExitCode == 0 {
				m.log.Detail("%s is already installed", addon.Name)
				continue
			}
		}

		m.log.Step("Installing " + addon.Name)

		for _, cmd := range addon.Commands {
			resolved := strings.ReplaceAll(cmd, kubectlPlaceholder, kubectl)
			m.log.Detail("%s", firstLine(resolved))
			if _, err := client.Sudo(ctx, resolved); err != nil {
				return fmt.Errorf("installing %s: %w", addon.Name, err)
			}
		}

		if addon.Manifest != "" {
			path := fmt.Sprintf("/tmp/buidl-addon-%s.yaml", addon.Name)
			if err := client.WriteFile(ctx, path, addon.Manifest, "0600"); err != nil {
				return fmt.Errorf("staging the %s manifest: %w", addon.Name, err)
			}
			// Retry briefly: a manifest referencing a freshly installed CRD can
			// race the API server's discovery cache.
			if err := m.applyWithRetry(ctx, bootstrap, kubectl, path, addon.Name); err != nil {
				return err
			}
			_, _ = client.Sudo(ctx, "rm -f "+remote.Quote(path))
		}

		m.log.Success("%s installed", addon.Name)
	}

	return nil
}

// applyWithRetry applies a manifest, tolerating transient CRD-discovery races.
func (m *Manager) applyWithRetry(ctx context.Context, server inventory.Server, kubectl, path, name string) error {
	client, err := m.connect(ctx, server)
	if err != nil {
		return err
	}

	command := fmt.Sprintf("%s apply -f %s", kubectl, remote.Quote(path))
	deadline := time.Now().Add(2 * time.Minute)

	var last error
	for {
		if _, err := client.Sudo(ctx, command); err == nil {
			return nil
		} else {
			last = err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("applying the %s manifest: %w", name, last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// certManagerAddon installs cert-manager and a Let's Encrypt ClusterIssuer.
//
// The ClusterIssuer is created here rather than left to the user because a deploy
// with proxy.ssl annotates its Ingress with "letsencrypt-prod": without a
// matching issuer, certificates would silently never be issued and the site would
// serve a default certificate.
func certManagerAddon(email, ingressClass string) Addon {
	const version = "v1.16.2"

	manifest := fmt.Sprintf(`apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: letsencrypt-prod
spec:
  acme:
    server: https://acme-v02.api.letsencrypt.org/directory
    email: %s
    privateKeySecretRef:
      name: letsencrypt-prod-account-key
    solvers:
      - http01:
          ingress:
            class: %s
`, email, ingressClass)

	return Addon{
		Name:        "cert-manager",
		Description: "issues and renews TLS certificates for proxy.ssl",
		// The CRD is the thing an Ingress actually depends on.
		Present: kubectlPlaceholder + " get crd certificates.cert-manager.io",
		Commands: []string{
			fmt.Sprintf("%s apply -f https://github.com/cert-manager/cert-manager/releases/download/%s/cert-manager.yaml",
				kubectlPlaceholder, version),
			// The admission webhook must be serving before a ClusterIssuer is
			// accepted, so the manifest below would otherwise fail.
			kubectlPlaceholder + " -n cert-manager rollout status deployment/cert-manager-webhook --timeout=180s",
		},
		Manifest: manifest,
	}
}

// metricsServerAddon installs metrics-server, required by deploy.autoscale.
func metricsServerAddon() Addon {
	return Addon{
		Name:        "metrics-server",
		Description: "provides CPU and memory metrics for deploy.autoscale",
		Present:     kubectlPlaceholder + " -n kube-system get deployment metrics-server",
		Commands: []string{
			kubectlPlaceholder + " apply -f https://github.com/kubernetes-sigs/metrics-server/releases/latest/download/components.yaml",
		},
	}
}

// buildKitAddon installs a rootless in-cluster buildkitd.
//
// This closes the loop on buidl's build story: after `cluster up`, `buidl deploy`
// has a builder without anyone installing Docker, running a privileged container,
// or configuring a CI runner.
func buildKitAddon() Addon {
	return Addon{
		Name:        "buildkit",
		Description: "in-cluster rootless image builder for `buidl deploy`",
		Present:     kubectlPlaceholder + " -n " + config.DefaultBuildKitNS + " get deployment buildkitd",
		Manifest:    buildKitManifest,
	}
}

// buildKitManifest is a rootless buildkitd Deployment and Service.
//
// Rootless matters: a privileged buildkitd is effectively root on the node, and a
// build runs the least trustworthy code in the system.
const buildKitManifest = `apiVersion: v1
kind: Namespace
metadata:
  name: buidl-system
  labels:
    app.kubernetes.io/managed-by: buidl
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: buildkitd
  namespace: buidl-system
  labels:
    app: buildkitd
    app.kubernetes.io/managed-by: buidl
spec:
  replicas: 1
  selector:
    matchLabels:
      app: buildkitd
  template:
    metadata:
      labels:
        app: buildkitd
      annotations:
        # Rootless buildkit needs an unconfined profile to use user namespaces.
        container.apparmor.security.beta.kubernetes.io/buildkitd: unconfined
    spec:
      containers:
        - name: buildkitd
          image: moby/buildkit:master-rootless
          args:
            - --addr
            - unix:///run/user/1000/buildkit/buildkitd.sock
            - --addr
            - tcp://0.0.0.0:1234
            - --oci-worker-no-process-sandbox
          readinessProbe:
            exec:
              command: [buildctl, debug, workers]
            initialDelaySeconds: 5
            periodSeconds: 30
          livenessProbe:
            exec:
              command: [buildctl, debug, workers]
            initialDelaySeconds: 30
            periodSeconds: 30
          securityContext:
            # Not privileged: rootless buildkit needs only an unmasked user
            # namespace, a far smaller grant than privileged.
            seccompProfile:
              type: Unconfined
            runAsUser: 1000
            runAsGroup: 1000
          ports:
            - containerPort: 1234
              name: grpc
          volumeMounts:
            - name: cache
              mountPath: /home/user/.local/share/buildkit
      volumes:
        - name: cache
          emptyDir: {}
---
apiVersion: v1
kind: Service
metadata:
  name: buildkitd
  namespace: buidl-system
  labels:
    app: buildkitd
spec:
  selector:
    app: buildkitd
  ports:
    - name: grpc
      port: 1234
      targetPort: 1234
`

// BuildKitAddress is the in-cluster address of the buildkit addon, for pointing
// build.addr at it.
func BuildKitAddress() string {
	return fmt.Sprintf("tcp://buildkitd.%s:1234", config.DefaultBuildKitNS)
}

// AddonNames lists the enabled addon names, for display.
func (m *Manager) AddonNames() []string {
	addons := m.Addons()
	names := make([]string, 0, len(addons))
	for _, a := range addons {
		names = append(names, a.Name)
	}
	return names
}

// AddonSummary renders the enabled addons on one line.
func (m *Manager) AddonSummary() string {
	names := m.AddonNames()
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}
