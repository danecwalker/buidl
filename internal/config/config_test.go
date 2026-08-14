package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// write creates a config file in a temp dir and returns its path.
func write(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "buidl.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	return path
}

const minimal = `
app: web
image: ghcr.io/acme/web
`

func TestLoadMinimalAppliesDefaults(t *testing.T) {
	res, err := Load(LoadOptions{Path: write(t, minimal), Strict: true})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cfg := res.Config

	// A four-line config must produce a safe, complete deployment spec.
	if cfg.Deploy.Target != "kubernetes" {
		t.Errorf("Target = %q, want kubernetes", cfg.Deploy.Target)
	}
	if cfg.Deploy.Port != DefaultPort {
		t.Errorf("Port = %d, want %d", cfg.Deploy.Port, DefaultPort)
	}
	if cfg.Deploy.Healthcheck.Path != "" {
		t.Errorf("Healthcheck.Path = %q, want empty so each probe uses its z-page", cfg.Deploy.Healthcheck.Path)
	}
	if got := cfg.Deploy.Healthcheck.Readiness; got != DefaultReadinessPath {
		t.Errorf("Healthcheck.Readiness = %q, want %q", got, DefaultReadinessPath)
	}
	if got := cfg.Deploy.Healthcheck.Liveness; got != DefaultLivenessPath {
		t.Errorf("Healthcheck.Liveness = %q, want %q", got, DefaultLivenessPath)
	}
	if got := cfg.Deploy.Healthcheck.Startup; got != DefaultStartupPath {
		t.Errorf("Healthcheck.Startup = %q, want %q", got, DefaultStartupPath)
	}
	// The healthcheck must default to the app's port, not a fixed one.
	if cfg.Deploy.Healthcheck.Port != DefaultPort {
		t.Errorf("Healthcheck.Port = %d, want %d", cfg.Deploy.Healthcheck.Port, DefaultPort)
	}
	if cfg.Deploy.Kubernetes.Namespace != "web" {
		t.Errorf("Namespace = %q, want web (derived from app)", cfg.Deploy.Kubernetes.Namespace)
	}
	if cfg.Registry.Server != "ghcr.io" {
		t.Errorf("Registry.Server = %q, want ghcr.io (derived from image)", cfg.Registry.Server)
	}
	// A four-line GHCR config must be able to pull: the cluster cannot use
	// the developer's docker login, so buidl copies it in by default.
	if !cfg.Registry.ManagesPullSecret() {
		t.Error("CreatePullSecret should default to true so the cluster can pull")
	}
	if !cfg.Registry.PullSecretOptional() {
		t.Error("an omitted createPullSecret must be optional so missing creds skip")
	}
	if cfg.Build.CacheRef != "ghcr.io/acme/web:buildcache" {
		t.Errorf("CacheRef = %q", cfg.Build.CacheRef)
	}
	// Zero-downtime by default is the core promise; assert it explicitly.
	if cfg.Deploy.Strategy.MaxUnavailable != "0" {
		t.Errorf("MaxUnavailable = %q, want 0 for zero-downtime", cfg.Deploy.Strategy.MaxUnavailable)
	}
	// No host configured means no ingress; a worker must not get one.
	if cfg.Proxy.Enabled == nil || *cfg.Proxy.Enabled {
		t.Error("Proxy.Enabled should default to false with no host")
	}
	if cfg.Environment != "default" {
		t.Errorf("Environment = %q, want default", cfg.Environment)
	}
	// An HTTP app with no replica pin gets an HPA, not a static replica count.
	if cfg.Deploy.Autoscale == nil {
		t.Fatal("expected a default HPA for an HTTP app")
	}
	if cfg.Deploy.Replicas != nil {
		t.Errorf("Replicas = %d, want unset when an HPA owns scaling", *cfg.Deploy.Replicas)
	}
	if cfg.Deploy.Autoscale.CPUPercent != DefaultAutoscaleCPU {
		t.Errorf("CPUPercent = %d, want %d", cfg.Deploy.Autoscale.CPUPercent, DefaultAutoscaleCPU)
	}
	if cfg.Deploy.Autoscale.Min != 1 || cfg.Deploy.Autoscale.Max != 4 {
		t.Errorf("HPA bounds = %d/%d, want 1/4 on a single-node fallback", cfg.Deploy.Autoscale.Min, cfg.Deploy.Autoscale.Max)
	}
	if cfg.Deploy.Resources.Requests["cpu"] != DefaultRequestCPU {
		t.Errorf("cpu request = %q, want %s (HPA needs a request)", cfg.Deploy.Resources.Requests["cpu"], DefaultRequestCPU)
	}
}

func TestRegistryFromImage(t *testing.T) {
	tests := []struct {
		image string
		want  string
	}{
		{"ghcr.io/acme/web", "ghcr.io"},
		{"registry.example.com:5000/web", "registry.example.com:5000"},
		{"localhost/web", "localhost"},
		// A first segment without a dot is a Docker Hub namespace, not a host.
		{"acme/web", "docker.io"},
		{"web", "docker.io"},
	}
	for _, tt := range tests {
		if got := registryFromImage(tt.image); got != tt.want {
			t.Errorf("registryFromImage(%q) = %q, want %q", tt.image, got, tt.want)
		}
	}
}

func TestCreatePullSecretExplicitFalse(t *testing.T) {
	res, err := Load(LoadOptions{Path: write(t, `
app: web
image: ghcr.io/acme/web
registry:
  createPullSecret: false
`), Strict: true})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// A public image, or a node-level registries.yaml, must be able to opt out.
	if res.Config.Registry.ManagesPullSecret() {
		t.Error("createPullSecret: false must not be overwritten by the default")
	}
	if res.Config.Registry.PullSecretOptional() {
		t.Error("an explicit false is not a defaulted pull secret")
	}
}

func TestCreatePullSecretExplicitTrueIsRequired(t *testing.T) {
	res, err := Load(LoadOptions{Path: write(t, `
app: web
image: ghcr.io/acme/web
registry:
  createPullSecret: true
`), Strict: true})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !res.Config.Registry.ManagesPullSecret() {
		t.Error("createPullSecret: true must stay on")
	}
	if res.Config.Registry.PullSecretOptional() {
		t.Error("an explicit true must fail when credentials are missing, not skip")
	}
}

func TestCreatePullSecretOmittedWhenPullSecretSet(t *testing.T) {
	res, err := Load(LoadOptions{Path: write(t, `
app: web
image: ghcr.io/acme/web
registry:
  pullSecret: already-managed
`), Strict: true})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// An existing secret means someone else owns the credential; do not also
	// mint one from the local Docker config.
	if res.Config.Registry.ManagesPullSecret() {
		t.Error("CreatePullSecret should stay off when pullSecret is set")
	}
}

const withEnvironments = `
app: web
image: ghcr.io/acme/web
deploy:
  replicas: 2
  port: 3000
  resources:
    requests: {cpu: 100m, memory: 128Mi}
env:
  clear:
    LOG_LEVEL: info
    REGION: us-east
environments:
  staging:
    deploy:
      replicas: 1
    env:
      clear:
        LOG_LEVEL: debug
  production:
    deploy:
      replicas: 5
`

func TestEnvironmentOverlayMergesDeeply(t *testing.T) {
	path := write(t, withEnvironments)

	staging, err := Load(LoadOptions{Path: path, Environment: "staging", Strict: true})
	if err != nil {
		t.Fatalf("Load staging: %v", err)
	}

	// The overlay replaces one nested key...
	if got := *staging.Config.Deploy.Replicas; got != 1 {
		t.Errorf("staging replicas = %d, want 1", got)
	}
	// ...while sibling keys at the same level survive.
	if got := staging.Config.Deploy.Port; got != 3000 {
		t.Errorf("staging port = %d, want 3000 (inherited from base)", got)
	}
	if got := staging.Config.Deploy.Resources.Requests["cpu"]; got != "100m" {
		t.Errorf("staging cpu request = %q, want 100m (inherited)", got)
	}
	// A map value is merged, not replaced wholesale.
	if got := staging.Config.Env.Clear["LOG_LEVEL"]; got != "debug" {
		t.Errorf("staging LOG_LEVEL = %q, want debug (overridden)", got)
	}
	if got := staging.Config.Env.Clear["REGION"]; got != "us-east" {
		t.Errorf("staging REGION = %q, want us-east (inherited)", got)
	}

	prod, err := Load(LoadOptions{Path: path, Environment: "production", Strict: true})
	if err != nil {
		t.Fatalf("Load production: %v", err)
	}
	if got := *prod.Config.Deploy.Replicas; got != 5 {
		t.Errorf("production replicas = %d, want 5", got)
	}
	// Environments must not leak into each other.
	if got := prod.Config.Env.Clear["LOG_LEVEL"]; got != "info" {
		t.Errorf("production LOG_LEVEL = %q, want info", got)
	}

	if len(staging.Environments) != 2 {
		t.Errorf("Environments = %v, want 2 entries", staging.Environments)
	}
}

func TestSequencesAreReplacedNotAppended(t *testing.T) {
	path := write(t, `
app: web
image: ghcr.io/acme/web
build:
  platforms: [linux/amd64, linux/arm64]
environments:
  staging:
    build:
      platforms: [linux/amd64]
`)
	res, err := Load(LoadOptions{Path: path, Environment: "staging", Strict: true})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// "platforms: [linux/amd64]" in an overlay must mean exactly that.
	got := res.Config.Build.Platforms
	if len(got) != 1 || got[0] != "linux/amd64" {
		t.Errorf("platforms = %v, want [linux/amd64]", got)
	}
}

func TestUnknownEnvironmentIsRejected(t *testing.T) {
	_, err := Load(LoadOptions{Path: write(t, withEnvironments), Environment: "nope", Strict: true})
	if err == nil {
		t.Fatal("expected an error for an unknown environment")
	}
	// The message must list the valid choices.
	for _, want := range []string{"nope", "staging", "production"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

func TestMissingEnvironmentPicksStaging(t *testing.T) {
	res, err := Load(LoadOptions{Path: write(t, withEnvironments), Strict: true})
	if err != nil {
		t.Fatalf("staging should be implied: %v", err)
	}
	if res.Config.Environment != "staging" {
		t.Errorf("Environment = %q, want staging", res.Config.Environment)
	}
	if got := *res.Config.Deploy.Replicas; got != 1 {
		t.Errorf("implied staging replicas = %d, want 1", got)
	}
}

func TestMissingEnvironmentIsRejectedWhenOnlyProductionExists(t *testing.T) {
	_, err := Load(LoadOptions{Path: write(t, `
app: web
image: ghcr.io/acme/web
environments:
  production:
    deploy: {replicas: 3}
`), Strict: true})
	if err == nil {
		t.Fatal("expected an error when the only environment is production")
	}
	if !strings.Contains(err.Error(), "--env") {
		t.Errorf("error should tell the user to pass --env, got: %v", err)
	}
	if !strings.Contains(err.Error(), "production") {
		t.Errorf("error should list production, got: %v", err)
	}
}

func TestDefaultEnvironment(t *testing.T) {
	res, err := Load(LoadOptions{Path: write(t, `
app: web
image: ghcr.io/acme/web
defaultEnvironment: staging
environments:
  staging:
    deploy: {replicas: 1}
`), Strict: true})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if res.Config.Environment != "staging" {
		t.Errorf("Environment = %q, want staging", res.Config.Environment)
	}
}

func TestEmptyEnvironmentBodyInheritsBase(t *testing.T) {
	res, err := Load(LoadOptions{Path: write(t, `
app: web
image: ghcr.io/acme/web
deploy: {replicas: 4}
environments:
  staging:
`), Environment: "staging", Strict: true})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := *res.Config.Deploy.Replicas; got != 4 {
		t.Errorf("replicas = %d, want 4", got)
	}
}

func TestInterpolation(t *testing.T) {
	t.Setenv("MY_HOST", "app.example.com")

	res, err := Load(LoadOptions{
		Path: write(t, `
app: web
image: ghcr.io/acme/web
proxy:
  host: ${MY_HOST}
  hosts:
    - ${BUIDL_SLUG}.preview.example.com
    - ${MISSING:-fallback.example.com}
env:
  clear:
    ENVIRONMENT: ${BUIDL_ENV}
`),
		Environment: "",
		Vars:        map[string]string{"BUIDL_SLUG": "feature-x"},
		Strict:      true,
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cfg := res.Config

	if cfg.Proxy.Host != "app.example.com" {
		t.Errorf("host = %q, want app.example.com", cfg.Proxy.Host)
	}
	if cfg.Proxy.Hosts[0] != "feature-x.preview.example.com" {
		t.Errorf("hosts[0] = %q", cfg.Proxy.Hosts[0])
	}
	// ${VAR:-default} must fall back rather than fail.
	if cfg.Proxy.Hosts[1] != "fallback.example.com" {
		t.Errorf("hosts[1] = %q, want the default", cfg.Proxy.Hosts[1])
	}
	// BUIDL_ENV is synthesized from the selected environment.
	if cfg.Env.Clear["ENVIRONMENT"] != "default" {
		t.Errorf("ENVIRONMENT = %q, want default", cfg.Env.Clear["ENVIRONMENT"])
	}
	// A host set means ingress is on.
	if cfg.Proxy.Enabled == nil || !*cfg.Proxy.Enabled {
		t.Error("Proxy.Enabled should be true when a host is set")
	}
}

func TestInterpolationUnsetVariableFails(t *testing.T) {
	_, err := Load(LoadOptions{Path: write(t, `
app: web
image: ghcr.io/acme/web
proxy:
  host: ${DEFINITELY_NOT_SET_ANYWHERE}
`), Strict: true})
	if err == nil {
		t.Fatal("expected an error for an unset variable")
	}
	// Silently deploying an empty hostname would be far worse than failing.
	if !strings.Contains(err.Error(), "DEFINITELY_NOT_SET_ANYWHERE") {
		t.Errorf("error should name the variable, got: %v", err)
	}
}

func TestInterpolationRequiredVariableMessage(t *testing.T) {
	_, err := Load(LoadOptions{Path: write(t, `
app: web
image: ghcr.io/acme/web
registry:
  password: ${REGISTRY_TOKEN:?create a token with packages:write}
`), Strict: true})
	if err == nil {
		t.Fatal("expected an error for a required variable")
	}
	if !strings.Contains(err.Error(), "packages:write") {
		t.Errorf("error should include the custom message, got: %v", err)
	}
}

func TestInterpolationDoesNotCorruptStructure(t *testing.T) {
	// A value containing YAML metacharacters must survive interpolation without
	// changing the shape of the document.
	t.Setenv("TRICKY", "a: b\nc: d")

	res, err := Load(LoadOptions{Path: write(t, `
app: web
image: ghcr.io/acme/web
env:
  clear:
    TRICKY: ${TRICKY}
`), Strict: true})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := res.Config.Env.Clear["TRICKY"]; got != "a: b\nc: d" {
		t.Errorf("TRICKY = %q, want the literal value", got)
	}
	if len(res.Config.Env.Clear) != 1 {
		t.Errorf("interpolation altered the document shape: %v", res.Config.Env.Clear)
	}
}

func TestStrictModeRejectsUnknownFields(t *testing.T) {
	_, err := Load(LoadOptions{Path: write(t, `
app: web
image: ghcr.io/acme/web
deploy:
  replicas: 2
  replicase: 3
`), Strict: true})
	if err == nil {
		t.Fatal("expected an error for an unknown field")
	}
	if !strings.Contains(err.Error(), "replicase") {
		t.Errorf("error should name the offending field, got: %v", err)
	}
}

func TestConfigDiscoveryWalksUpward(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "buidl.yaml"), []byte(minimal), 0o644); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(dir, "services", "api")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	// Running from a subdirectory must find the project's config, like git does.
	res, err := Load(LoadOptions{Dir: nested, Strict: true})
	if err != nil {
		t.Fatalf("Load from nested dir: %v", err)
	}
	if res.Config.App != "web" {
		t.Errorf("App = %q", res.Config.App)
	}
}

func TestMissingConfigMentionsInit(t *testing.T) {
	_, err := Load(LoadOptions{Dir: t.TempDir(), Strict: true})
	if err == nil {
		t.Fatal("expected an error when no config exists")
	}
	if !strings.Contains(err.Error(), "buidl init") {
		t.Errorf("error should suggest `buidl init`, got: %v", err)
	}
}

func TestDurationParsing(t *testing.T) {
	res, err := Load(LoadOptions{Path: write(t, `
app: web
image: ghcr.io/acme/web
deploy:
  deployTimeout: 10m
  drainTimeout: "45"
`), Strict: true})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := res.Config.Deploy.DeployTimeout.Duration; got != 10*time.Minute {
		t.Errorf("DeployTimeout = %v, want 10m", got)
	}
	// A bare number is seconds, matching Kamal's integer timeouts.
	if got := res.Config.Deploy.DrainTimeout.Duration; got != 45*time.Second {
		t.Errorf("DrainTimeout = %v, want 45s", got)
	}
}

func TestValidation(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name:    "missing app",
			yaml:    "image: ghcr.io/acme/web\n",
			wantErr: "`app` is required",
		},
		{
			name:    "uppercase app",
			yaml:    "app: Web\nimage: ghcr.io/acme/web\n",
			wantErr: "lowercase",
		},
		{
			name:    "missing image",
			yaml:    "app: web\n",
			wantErr: "`image` is required",
		},
		{
			name:    "image with tag",
			yaml:    "app: web\nimage: ghcr.io/acme/web:latest\n",
			wantErr: "must not include a tag",
		},
		{
			name:    "image with digest",
			yaml:    "app: web\nimage: ghcr.io/acme/web@sha256:abc\n",
			wantErr: "must not include a digest",
		},
		{
			name:    "bad port",
			yaml:    "app: web\nimage: ghcr.io/acme/web\ndeploy: {port: 99999}\n",
			wantErr: "between 1 and 65535",
		},
		{
			name:    "bad quantity",
			yaml:    "app: web\nimage: ghcr.io/acme/web\ndeploy: {resources: {requests: {cpu: fast}}}\n",
			wantErr: "not a valid quantity",
		},
		{
			name:    "bad strategy",
			yaml:    "app: web\nimage: ghcr.io/acme/web\ndeploy: {strategy: {type: yolo}}\n",
			wantErr: "must be one of rolling",
		},
		{
			name:    "stalled rolling update",
			yaml:    "app: web\nimage: ghcr.io/acme/web\ndeploy: {strategy: {maxSurge: \"0\", maxUnavailable: \"0\"}}\n",
			wantErr: "can never make progress",
		},
		{
			name:    "healthcheck path and command",
			yaml:    "app: web\nimage: ghcr.io/acme/web\ndeploy: {healthcheck: {path: /up, command: [true]}}\n",
			wantErr: "not both",
		},
		{
			name:    "healthcheck readiness and command",
			yaml:    "app: web\nimage: ghcr.io/acme/web\ndeploy: {healthcheck: {readiness: /readyz, command: [true]}}\n",
			wantErr: "not both",
		},
		{
			name:    "healthcheck path missing slash",
			yaml:    "app: web\nimage: ghcr.io/acme/web\ndeploy: {healthcheck: {path: up}}\n",
			wantErr: "must start with /",
		},
		{
			name:    "healthcheck readiness missing slash",
			yaml:    "app: web\nimage: ghcr.io/acme/web\ndeploy: {healthcheck: {readiness: readyz}}\n",
			wantErr: "must start with /",
		},
		{
			name:    "host with scheme",
			yaml:    "app: web\nimage: ghcr.io/acme/web\nproxy: {host: \"https://acme.com\"}\n",
			wantErr: "bare hostname",
		},
		{
			name:    "autoscale min above max",
			yaml:    "app: web\nimage: ghcr.io/acme/web\ndeploy: {autoscale: {min: 5, max: 2, cpuPercent: 70}}\n",
			wantErr: "must be >= min",
		},
		{
			name:    "autoscale negative min",
			yaml:    "app: web\nimage: ghcr.io/acme/web\ndeploy: {autoscale: {min: -1, max: 5, cpuPercent: 70}}\n",
			wantErr: "cannot be negative",
		},
		{
			name:    "secret also in clear",
			yaml:    "app: web\nimage: ghcr.io/acme/web\nenv: {clear: {TOKEN: x}, secret: [TOKEN]}\n",
			wantErr: "both clear and secret",
		},
		{
			name:    "duplicate secret",
			yaml:    "app: web\nimage: ghcr.io/acme/web\nenv: {secret: [A, A]}\n",
			wantErr: "listed twice",
		},
		{
			name:    "accessory without image",
			yaml:    "app: web\nimage: ghcr.io/acme/web\naccessories: {db: {port: 5432}}\n",
			wantErr: "image is required",
		},
		{
			name:    "unsupported target",
			yaml:    "app: web\nimage: ghcr.io/acme/web\ndeploy: {target: heroku}\n",
			wantErr: "not supported yet",
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

func TestValidationAggregatesProblems(t *testing.T) {
	// Reporting every problem at once beats one-per-run.
	_, err := Load(LoadOptions{Path: write(t, `
app: Web
image: ghcr.io/acme/web:tag
deploy:
  port: 0
`), Strict: true})
	if err == nil {
		t.Fatal("expected errors")
	}
	msg := err.Error()
	if !strings.Contains(msg, "configuration problems") {
		t.Errorf("expected an aggregated error, got: %v", err)
	}
	for _, want := range []string{"lowercase", "must not include a tag"} {
		if !strings.Contains(msg, want) {
			t.Errorf("aggregated error should mention %q, got: %v", want, err)
		}
	}
}

func TestValidConfigWithEverything(t *testing.T) {
	// A maximal config must round-trip without error, so the schema stays usable
	// as it grows.
	_, err := Load(LoadOptions{Path: write(t, `
version: 1
app: web
image: ghcr.io/acme/web
registry:
  server: ghcr.io
  username: acme
build:
  driver: buildkit
  context: .
  dockerfile: Dockerfile
  target: runtime
  platforms: [linux/amd64, linux/arm64]
  cache: registry
  args: {NODE_ENV: production}
  secrets: {npm_token: env:NPM_TOKEN}
deploy:
  target: kubernetes
  replicas: 3
  port: 3000
  command: [node, server.js]
  kubernetes:
    namespace: acme
    createNamespace: true
    nodeSelector: {pool: web}
  healthcheck:
    path: /healthz
    initialDelaySeconds: 5
  resources:
    requests: {cpu: 100m, memory: 256Mi}
    limits: {cpu: "2", memory: 1Gi}
  strategy:
    type: bluegreen
    maxSurge: 100%
    maxUnavailable: "0"
    readinessDelay: 5s
  autoscale: {min: 3, max: 20, cpuPercent: 70, memoryPercent: 80}
  deployTimeout: 10m
  drainTimeout: 30s
env:
  clear: {LOG_LEVEL: info}
  secret: [DATABASE_URL]
  secretRefs: [shared-config]
proxy:
  host: acme.com
  hosts: [www.acme.com]
  ssl: true
  class: nginx
  annotations: {nginx.ingress.kubernetes.io/proxy-body-size: 20m}
accessories:
  postgres:
    image: postgres:16
    port: 5432
    storage: 20Gi
    env:
      secret: [POSTGRES_PASSWORD]
retainReleases: 20
`), Strict: true})
	if err != nil {
		t.Fatalf("maximal config should be valid: %v", err)
	}
}

func TestAccessoryMountPathDefaults(t *testing.T) {
	res, err := Load(LoadOptions{Path: write(t, `
app: web
image: ghcr.io/acme/web
accessories:
  postgres:
    image: postgres:16
    storage: 10Gi
  cache:
    image: redis:7
    storage: 1Gi
`), Strict: true})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := res.Config.Accessories["postgres"].MountPath; got != "/var/lib/postgresql/data" {
		t.Errorf("postgres mountPath = %q", got)
	}
	if got := res.Config.Accessories["cache"].MountPath; got != "/data" {
		t.Errorf("redis mountPath = %q", got)
	}
}

func TestAutoscaleWithoutSignalDefaultsCPU(t *testing.T) {
	res, err := Load(LoadOptions{Path: write(t, `
app: web
image: ghcr.io/acme/web
deploy:
  autoscale: {min: 1, max: 5}
`), Strict: true})
	if err != nil {
		t.Fatalf("an HPA without a signal should default cpuPercent: %v", err)
	}
	if got := res.Config.Deploy.Autoscale.CPUPercent; got != DefaultAutoscaleCPU {
		t.Errorf("CPUPercent = %d, want %d", got, DefaultAutoscaleCPU)
	}
}

func TestPreviewStaysAtOneReplica(t *testing.T) {
	res, err := Load(LoadOptions{Path: write(t, `
app: web
image: ghcr.io/acme/web
environments:
  preview:
    deploy:
      kubernetes: {namespace: web-preview, createNamespace: true, ephemeral: true}
`), Environment: "preview", Strict: true})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if res.Config.Deploy.Autoscale != nil {
		t.Error("preview must not get an HPA")
	}
	if res.Config.Deploy.Replicas == nil || *res.Config.Deploy.Replicas != 1 {
		t.Errorf("preview replicas = %v, want 1", res.Config.Deploy.Replicas)
	}
}

func TestHealthcheckPathAppliesToAllProbes(t *testing.T) {
	res, err := Load(LoadOptions{Path: write(t, `
app: web
image: ghcr.io/acme/web
deploy:
  healthcheck:
    path: /up
`), Strict: true})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	hc := res.Config.Deploy.Healthcheck
	if hc.Path != "/up" {
		t.Errorf("Path = %q, want /up", hc.Path)
	}
	for _, got := range []struct{ name, val string }{
		{"readiness", hc.Readiness},
		{"liveness", hc.Liveness},
		{"startup", hc.Startup},
	} {
		if got.val != "/up" {
			t.Errorf("%s = %q, want /up", got.name, got.val)
		}
	}
}

func TestHealthcheckPerProbeOverride(t *testing.T) {
	res, err := Load(LoadOptions{Path: write(t, `
app: web
image: ghcr.io/acme/web
deploy:
  healthcheck:
    path: /up
    readiness: /readyz
`), Strict: true})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	hc := res.Config.Deploy.Healthcheck
	if hc.Readiness != "/readyz" {
		t.Errorf("Readiness = %q, want /readyz (explicit override)", hc.Readiness)
	}
	if hc.Liveness != "/up" {
		t.Errorf("Liveness = %q, want /up from path", hc.Liveness)
	}
	if hc.Startup != "/up" {
		t.Errorf("Startup = %q, want /up from path", hc.Startup)
	}
}

func TestWorkerStaysAtOneReplica(t *testing.T) {
	res, err := Load(LoadOptions{Path: write(t, `
app: worker
image: ghcr.io/acme/worker
deploy:
  healthcheck:
    command: [pgrep, sidekiq]
`), Strict: true})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if res.Config.Deploy.Autoscale != nil {
		t.Error("a worker with an exec probe must not get an HPA")
	}
	if res.Config.Deploy.Replicas == nil || *res.Config.Deploy.Replicas != 1 {
		t.Errorf("worker replicas = %v, want 1", res.Config.Deploy.Replicas)
	}
}

func TestExplicitReplicasStayStatic(t *testing.T) {
	res, err := Load(LoadOptions{Path: write(t, `
app: web
image: ghcr.io/acme/web
deploy:
  replicas: 3
`), Strict: true})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if res.Config.Deploy.Autoscale != nil {
		t.Error("explicit replicas must not grow an HPA")
	}
	if got := *res.Config.Deploy.Replicas; got != 3 {
		t.Errorf("replicas = %d, want 3", got)
	}
}

func TestFleetSizesDefaultHPA(t *testing.T) {
	res, err := Load(LoadOptions{Path: write(t, `
app: web
image: ghcr.io/acme/web
infra:
  servers:
    - {host: 10.0.0.1, role: control-plane}
    - {host: 10.0.1.1, role: worker}
    - {host: 10.0.1.2, role: worker}
`), Strict: true})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	as := res.Config.Deploy.Autoscale
	if as == nil {
		t.Fatal("expected a default HPA")
	}
	if as.Min != 2 || as.Max != 9 {
		t.Errorf("HPA bounds = %d/%d, want 2/9 for a 3-node fleet", as.Min, as.Max)
	}
}
