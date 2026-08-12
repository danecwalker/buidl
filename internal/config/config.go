// Package config defines the buidl.yaml schema and the rules for resolving it
// into a concrete, per-environment deployment spec.
//
// The schema borrows its ergonomics from Kamal (a single declarative file, named
// environments that overlay a common base, accessories, proxy config) and its
// semantics from Kubernetes (server-side apply, rollout gating, immutable
// releases addressed by image digest).
package config

import (
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the fully resolved configuration for exactly one environment.
//
// A Config is never read directly off disk: Load parses the base document,
// overlays the selected environment, interpolates variables, applies defaults,
// and validates. See load.go.
type Config struct {
	// Version pins the schema so we can evolve it without breaking existing files.
	Version int `yaml:"version"`

	// App is the logical application name. It seeds Kubernetes object names and
	// selector labels, so it must be a valid DNS label.
	App string `yaml:"app"`

	// Image is the repository to push to, without a tag (e.g. ghcr.io/acme/web).
	// Tags are derived per release; deploys always resolve to a digest.
	Image string `yaml:"image"`

	Registry    Registry             `yaml:"registry"`
	Build       Build                `yaml:"build"`
	Deploy      Deploy               `yaml:"deploy"`
	Env         Env                  `yaml:"env"`
	Proxy       Proxy                `yaml:"proxy"`
	Accessories map[string]Accessory `yaml:"accessories"`

	// Infra describes the machines behind the cluster and how to turn them into
	// one. It is optional: against a managed cluster (EKS, GKE, or someone else's
	// kubeconfig) buidl only deploys and never touches servers.
	Infra *Infra `yaml:"infra"`

	// RetainReleases caps how many superseded ReplicaSets/releases we keep for
	// rollback. Mirrors Kamal's retain_containers.
	RetainReleases int `yaml:"retainReleases"`

	// HooksPath holds executable lifecycle hooks (pre-build, post-deploy, ...).
	HooksPath string `yaml:"hooksPath"`

	// Environments holds the raw per-environment overlays. It is consumed during
	// Load and is empty on a resolved Config.
	Environments map[string]any `yaml:"environments"`

	// Environment is the name of the environment this Config was resolved for.
	// Populated by Load; not settable in YAML.
	Environment string `yaml:"-"`
}

// Registry describes how to authenticate to the image registry.
//
// There are two distinct authentication problems here, and conflating them is a
// common source of "it built fine but the pods can't start":
//
//   - buidl needs credentials to *push*. These come from the local Docker config,
//     so `docker login`, `gcloud auth configure-docker` and docker/login-action all
//     work with no buidl-specific setup.
//   - The cluster needs credentials to *pull*. Those live in the cluster, as an
//     imagePullSecret. A private registry will not serve the kubelet just because
//     the developer's laptop is authenticated.
type Registry struct {
	Server   string `yaml:"server"`
	Username string `yaml:"username"`
	// Password should almost always be an interpolated reference such as
	// ${GITHUB_TOKEN} rather than a literal.
	Password string `yaml:"password"`

	// PullSecret names an existing Kubernetes Secret of type
	// kubernetes.io/dockerconfigjson to use for pulling. Prefer this when an
	// external operator (External Secrets, a cluster bootstrap job) already
	// manages registry credentials.
	PullSecret string `yaml:"pullSecret"`

	// CreatePullSecret has buidl create and maintain the pull secret itself,
	// sourcing credentials from Username/Password when set and otherwise from the
	// same local Docker config it pushes with.
	//
	// This copies a registry credential from this machine into the cluster, which
	// is a real trust decision — hence opt-in rather than automatic.
	CreatePullSecret bool `yaml:"createPullSecret"`
}

// BuildDriver selects how images are produced.
type BuildDriver string

const (
	// DriverBuildKit builds via a BuildKit frontend and pushes straight to the
	// registry. No Docker daemon is involved.
	DriverBuildKit BuildDriver = "buildkit"
	// DriverPrebuilt skips building; the image must already exist in the
	// registry. Used by `promote` and by CI jobs that build separately.
	DriverPrebuilt BuildDriver = "prebuilt"
)

// Build configures image production.
type Build struct {
	Driver BuildDriver `yaml:"driver"`

	// Context is the build context directory, relative to the config file.
	Context string `yaml:"context"`
	// Dockerfile is relative to Context. If it does not exist, `buidl init`
	// can generate one from detected project type.
	Dockerfile string `yaml:"dockerfile"`
	// Target selects a specific stage in a multi-stage Dockerfile.
	Target string `yaml:"target"`

	// Platforms enables multi-arch builds, e.g. [linux/amd64, linux/arm64].
	Platforms []string `yaml:"platforms"`

	// Addr is the BuildKit endpoint. Empty means "discover one":
	// $BUILDKIT_HOST, then a local buildkitd socket, then an in-cluster
	// buildkitd found via the Kubernetes context.
	Addr string `yaml:"addr"`

	// Cache selects the cache backend: "registry" (portable across CI runners),
	// "inline", or "none".
	Cache string `yaml:"cache"`
	// CacheRef overrides where the registry cache is stored. Defaults to
	// <Image>:buildcache.
	CacheRef string `yaml:"cacheRef"`

	Args map[string]string `yaml:"args"`
	// Secrets are id=path pairs mounted via BuildKit secret mounts, so they
	// never land in an image layer.
	Secrets map[string]string `yaml:"secrets"`
}

// Deploy configures the runtime rollout.
type Deploy struct {
	// Target selects the deployment backend.
	Target string `yaml:"target"`

	Kubernetes Kubernetes `yaml:"kubernetes"`

	Replicas *int32 `yaml:"replicas"`
	// Port is the container port the app listens on.
	Port int32 `yaml:"port"`
	// Command overrides the image entrypoint's command.
	Command []string `yaml:"command"`

	Healthcheck Healthcheck `yaml:"healthcheck"`
	Resources   Resources   `yaml:"resources"`
	Strategy    Strategy    `yaml:"strategy"`
	Autoscale   *Autoscale  `yaml:"autoscale"`

	// DeployTimeout bounds how long we wait for a rollout to become healthy
	// before failing (and rolling back, if --auto-rollback).
	DeployTimeout Duration `yaml:"deployTimeout"`
	// DrainTimeout is the grace period given to terminating pods.
	DrainTimeout Duration `yaml:"drainTimeout"`
}

// Kubernetes holds cluster addressing. An empty Context means "use the current
// kubeconfig context", which is what CI runners with a mounted kubeconfig want.
type Kubernetes struct {
	Context        string            `yaml:"context"`
	Namespace      string            `yaml:"namespace"`
	ServiceAccount string            `yaml:"serviceAccount"`
	NodeSelector   map[string]string `yaml:"nodeSelector"`
	// CreateNamespace makes `deploy` create the namespace if absent. Handy for
	// ephemeral preview environments.
	CreateNamespace bool `yaml:"createNamespace"`
}

// Healthcheck drives both the readiness probe and the rollout gate. A release
// is only considered live once this passes, which is what makes deploys
// zero-downtime.
type Healthcheck struct {
	Path                string `yaml:"path"`
	Port                int32  `yaml:"port"`
	InitialDelaySeconds int32  `yaml:"initialDelaySeconds"`
	PeriodSeconds       int32  `yaml:"periodSeconds"`
	TimeoutSeconds      int32  `yaml:"timeoutSeconds"`
	FailureThreshold    int32  `yaml:"failureThreshold"`
	// Command, when set, replaces the HTTP probe with an exec probe. Use for
	// workers and other non-HTTP roles.
	Command []string `yaml:"command"`
}

// Resources maps to Kubernetes resource requests/limits. Values are opaque
// quantity strings ("100m", "512Mi") validated at render time.
type Resources struct {
	Requests map[string]string `yaml:"requests"`
	Limits   map[string]string `yaml:"limits"`
}

// StrategyType selects the rollout mechanism.
type StrategyType string

const (
	// StrategyRolling is a standard Kubernetes RollingUpdate. With
	// MaxUnavailable=0 it is zero-downtime.
	StrategyRolling StrategyType = "rolling"
	// StrategyBlueGreen boots the new release alongside the old one, waits for
	// it to be fully healthy, then flips the Service selector in one atomic
	// step. This is the closest analogue to kamal-proxy's request switching and
	// gives a true instant cutover plus instant rollback.
	StrategyBlueGreen StrategyType = "bluegreen"
	// StrategyRecreate tears down before booting. Accepts downtime; correct for
	// singleton workloads holding an exclusive lock.
	StrategyRecreate StrategyType = "recreate"
)

// Strategy configures rollout behavior.
type Strategy struct {
	Type StrategyType `yaml:"type"`
	// MaxSurge/MaxUnavailable accept counts ("1") or percentages ("25%").
	MaxSurge       string `yaml:"maxSurge"`
	MaxUnavailable string `yaml:"maxUnavailable"`
	// ReadinessDelay adds a settle period after pods report ready before we
	// declare success. Mirrors Kamal's readiness_delay.
	ReadinessDelay Duration `yaml:"readinessDelay"`
}

// Autoscale configures a HorizontalPodAutoscaler. When set, Replicas is treated
// as the floor and is not reconciled on subsequent deploys (so we don't fight
// the HPA).
type Autoscale struct {
	Min           int32 `yaml:"min"`
	Max           int32 `yaml:"max"`
	CPUPercent    int32 `yaml:"cpuPercent"`
	MemoryPercent int32 `yaml:"memoryPercent"`
}

// Env splits configuration into values safe to render into manifests (Clear)
// and values that must be sourced from a Secret (Secret).
type Env struct {
	Clear map[string]string `yaml:"clear"`
	// Secret lists variable NAMES only. Values are read from the local
	// environment or a secrets provider at deploy time and written to a
	// Kubernetes Secret; they are never written to buidl.yaml.
	Secret []string `yaml:"secret"`
	// SecretRefs mounts pre-existing Kubernetes Secrets by name. Preferred when
	// an external operator (External Secrets, Vault) already syncs them.
	SecretRefs []string `yaml:"secretRefs"`

	// Dotenv reads values for the names in Secret from .env and
	// .env.<environment>, so a project that already keeps its configuration there
	// need not restate every value in .buidl/secrets.
	//
	// Only *declared* names are deployed: these files supply values, not the list
	// of what to ship. That keeps a stray local variable from silently becoming
	// part of a release.
	//
	// `.env.local` and `.env.<environment>.local` are never read. By convention
	// those are gitignored machine-local dev config, and deploying one would ship
	// a developer's localhost database URL to production.
	Dotenv bool `yaml:"dotenv"`

	// DotenvFiles replaces the discovered dotenv files with an explicit list,
	// relative to the project root, lowest precedence first.
	DotenvFiles []string `yaml:"dotenvFiles"`
}

// Proxy configures ingress. It intentionally mirrors Kamal's `proxy` block: a
// host and a TLS toggle is all most apps need.
type Proxy struct {
	// Host is the external hostname. Supports interpolation, which is how
	// preview environments get per-branch hostnames:
	//   host: ${BUIDL_SLUG}.preview.acme.com
	Host string `yaml:"host"`
	// Hosts allows additional hostnames beyond Host.
	Hosts []string `yaml:"hosts"`
	// SSL requests a TLS certificate via cert-manager.
	SSL bool `yaml:"ssl"`
	// ClusterIssuer names the cert-manager ClusterIssuer. Defaults to
	// "letsencrypt-prod" when SSL is on.
	ClusterIssuer string `yaml:"clusterIssuer"`
	// Class is the IngressClass name (e.g. "nginx", "traefik").
	Class string `yaml:"class"`
	// Annotations passes backend-specific ingress tuning straight through.
	Annotations map[string]string `yaml:"annotations"`
	// Enabled defaults to true when a Host is set. Set false for workers.
	Enabled *bool `yaml:"enabled"`
}

// Accessory is a supporting stateful service (database, cache, queue) deployed
// alongside the app. Modeled on Kamal's accessories, rendered as a StatefulSet
// plus a headless Service when Storage is set.
//
// Accessories are deliberately not reconciled on every deploy; they are managed
// by `buidl accessory` so an app rollout can never restart your database.
type Accessory struct {
	Image string   `yaml:"image"`
	Port  int32    `yaml:"port"`
	Env   Env      `yaml:"env"`
	Cmd   []string `yaml:"cmd"`
	// Storage requests a PersistentVolumeClaim of this size, e.g. "10Gi".
	Storage      string    `yaml:"storage"`
	StorageClass string    `yaml:"storageClass"`
	MountPath    string    `yaml:"mountPath"`
	Resources    Resources `yaml:"resources"`
}

// Duration is a time.Duration that accepts Go duration strings ("30s", "5m") in
// YAML, and also bare integers interpreted as seconds for convenience.
type Duration struct {
	time.Duration
}

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return fmt.Errorf("duration must be a string like \"30s\": %w", err)
	}
	s = strings.TrimSpace(s)
	if s == "" {
		d.Duration = 0
		return nil
	}
	// Accept a bare number as seconds, matching Kamal's integer timeouts.
	if !strings.ContainsAny(s, "smhdunµ") {
		parsed, err := time.ParseDuration(s + "s")
		if err != nil {
			return fmt.Errorf("invalid duration %q", s)
		}
		d.Duration = parsed
		return nil
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	d.Duration = parsed
	return nil
}

// MarshalYAML implements yaml.Marshaler.
func (d Duration) MarshalYAML() (any, error) {
	return d.String(), nil
}

// Or returns d if set, otherwise fallback.
func (d Duration) Or(fallback time.Duration) time.Duration {
	if d.Duration == 0 {
		return fallback
	}
	return d.Duration
}
