package cli

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/danecwalker/buidl/internal/config"
	"github.com/danecwalker/buidl/internal/project"
)

// TestGithubWorkflowIsValidYAML guards the CI template.
//
// It is a Go string literal containing nested backticks, `${{ }}` expressions and
// embedded JavaScript, all of which are easy to break without noticing. A broken
// workflow would only surface as a CI syntax error in a user's repo.
func TestGithubWorkflowIsValidYAML(t *testing.T) {
	var doc struct {
		Name string                    `yaml:"name"`
		On   map[string]any            `yaml:"on"`
		Jobs map[string]map[string]any `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(githubWorkflow), &doc); err != nil {
		t.Fatalf("the generated workflow is not valid YAML: %v", err)
	}

	if doc.Name == "" {
		t.Error("workflow has no name")
	}
	// The four-stage shape is the point of the template.
	for _, job := range []string{"preview", "teardown", "staging", "production"} {
		if _, ok := doc.Jobs[job]; !ok {
			t.Errorf("workflow is missing the %q job", job)
		}
	}
	if len(doc.On) == 0 {
		t.Error("workflow has no triggers")
	}

	// Production must be a promotion, never a rebuild.
	if !strings.Contains(githubWorkflow, "buidl promote --from staging --to production") {
		t.Error("the production job should promote rather than rebuild")
	}
	// The download URL must hit the published repo. github.com/danewalker/buidl 404s.
	if !strings.Contains(githubWorkflow, "https://github.com/danecwalker/buidl/releases/latest/download/buidl-linux-amd64") {
		t.Error("the install step should download from github.com/danecwalker/buidl")
	}
	if strings.Contains(githubWorkflow, "github.com/danewalker/buidl") {
		t.Error("the workflow still points at github.com/danewalker/buidl, which does not exist")
	}
	// Full history is needed for buidl to record provenance.
	if !strings.Contains(githubWorkflow, "fetch-depth: 0") {
		t.Error("checkout should fetch full history for git provenance")
	}
	// Closing a PR must tear the preview down; the default pull_request types
	// omit `closed`, so the workflow has to list it.
	if !strings.Contains(githubWorkflow, "buidl destroy -e preview --yes") {
		t.Error("the teardown job should destroy the preview environment")
	}
	if !strings.Contains(githubWorkflow, "closed") {
		t.Error("the workflow must listen for pull_request closed so previews do not leak")
	}
	if !strings.Contains(githubWorkflow, "github.event.action != 'closed'") {
		t.Error("the preview job must not run when the PR is closed")
	}
}

// TestRenderedConfigIsValid asserts that what `init` writes actually loads.
//
// init validates its own output at runtime, but only for one environment. This
// checks every environment for several detected stacks, which is where a broken
// template would otherwise hide.
func TestRenderedConfigIsValid(t *testing.T) {
	detections := []project.Detection{
		{Kind: project.KindGo, Stack: project.KindGo, Name: "payments", Port: 8080},
		{Kind: project.KindNode, Stack: project.KindNode, Name: "web", Port: 3000, Framework: "next"},
		{Kind: project.KindRuby, Stack: project.KindRuby, Name: "monolith", Port: 3000, HealthPath: "/up", Framework: "rails"},
		{Kind: project.KindStatic, Stack: project.KindStatic, Name: "marketing", Port: 80},
	}

	for _, det := range detections {
		t.Run(det.Name, func(t *testing.T) {
			rendered := renderConfig(det, "ghcr.io/acme/"+det.Name)
			path := writeTempConfig(t, rendered)

			// Every environment the template declares must load and validate.
			for _, env := range []string{"staging", "production", "preview"} {
				res, err := config.Load(config.LoadOptions{
					Path:        path,
					Environment: env,
					Strict:      true,
					Vars:        map[string]string{"BUIDL_SLUG": "example-branch"},
				})
				if err != nil {
					t.Fatalf("environment %q did not validate:\n%v\n\nrendered config:\n%s", env, err, rendered)
				}

				cfg := res.Config
				if cfg.App != det.Name {
					t.Errorf("App = %q, want %q", cfg.App, det.Name)
				}
				if cfg.Deploy.Port != det.Port {
					t.Errorf("Port = %d, want %d", cfg.Deploy.Port, det.Port)
				}
				if det.HealthPath != "" {
					if cfg.Deploy.Healthcheck.Path != det.HealthPath {
						t.Errorf("healthcheck path = %q, want %q", cfg.Deploy.Healthcheck.Path, det.HealthPath)
					}
					if cfg.Deploy.Healthcheck.Readiness != det.HealthPath {
						t.Errorf("readiness = %q, want the explicit path %q", cfg.Deploy.Healthcheck.Readiness, det.HealthPath)
					}
				} else {
					if cfg.Deploy.Healthcheck.Path != "" {
						t.Errorf("healthcheck path = %q, want empty so z-pages apply", cfg.Deploy.Healthcheck.Path)
					}
					if cfg.Deploy.Healthcheck.Readiness != config.DefaultReadinessPath {
						t.Errorf("readiness = %q, want %s", cfg.Deploy.Healthcheck.Readiness, config.DefaultReadinessPath)
					}
					if cfg.Deploy.Healthcheck.Liveness != config.DefaultLivenessPath {
						t.Errorf("liveness = %q, want %s", cfg.Deploy.Healthcheck.Liveness, config.DefaultLivenessPath)
					}
					if cfg.Deploy.Healthcheck.Startup != config.DefaultStartupPath {
						t.Errorf("startup = %q, want %s", cfg.Deploy.Healthcheck.Startup, config.DefaultStartupPath)
					}
				}
				if env == "preview" {
					if cfg.Deploy.Autoscale != nil {
						t.Error("generated preview should stay at one replica, not an HPA")
					}
				} else if cfg.Deploy.Autoscale == nil {
					t.Errorf("generated %s should default to an HPA", env)
				}
			}

			// The preview environment must derive a per-branch namespace, or every
			// branch would collide on one namespace.
			preview, err := config.Load(config.LoadOptions{
				Path: path, Environment: "preview", Strict: true,
				Vars: map[string]string{"BUIDL_SLUG": "my-branch"},
			})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(preview.Config.Deploy.Kubernetes.Namespace, "my-branch") {
				t.Errorf("preview namespace = %q, want it derived from the slug",
					preview.Config.Deploy.Kubernetes.Namespace)
			}
			if !preview.Config.Deploy.Kubernetes.CreateNamespace {
				t.Error("preview should create its namespace, since it is ephemeral")
			}
			if preview.Config.Deploy.Kubernetes.Ephemeral == nil || !*preview.Config.Deploy.Kubernetes.Ephemeral {
				t.Error("preview should be marked ephemeral so destroy deletes the namespace")
			}
			if preview.Config.Deploy.Autoscale != nil {
				t.Error("generated preview should stay at one replica, not an HPA")
			}
			if preview.Config.Deploy.Replicas == nil || *preview.Config.Deploy.Replicas != 1 {
				t.Errorf("preview replicas = %v, want 1", preview.Config.Deploy.Replicas)
			}
		})
	}
}

func TestRenderedConfigImpliesStaging(t *testing.T) {
	rendered := renderConfig(project.Detection{
		Kind: project.KindGo, Stack: project.KindGo,
		Name: "web", Port: 8080,
	}, "ghcr.io/acme/web")

	if !strings.Contains(rendered, "defaultEnvironment: staging") {
		t.Error("generated config should set defaultEnvironment: staging")
	}
	if strings.Contains(rendered, "replicas: 2") || strings.Contains(rendered, "replicas: 3") {
		t.Error("generated config should not pin staging/production replica counts")
	}
	if !strings.Contains(rendered, "#infra:") {
		t.Error("generated config should include a commented infra block")
	}
	if !strings.Contains(rendered, "certManagerEmail:") {
		t.Error("generated infra comments should mention certManagerEmail")
	}
}

func TestResolveImage(t *testing.T) {
	if got, _ := resolveImage("GHCR.io/Acme/Web", "", "web"); got != "ghcr.io/acme/web" {
		t.Errorf("resolveImage = %q, want it lowercased", got)
	}
	if got, _ := resolveImage("", "ghcr.io/acme/", "web"); got != "ghcr.io/acme/web" {
		t.Errorf("resolveImage = %q", got)
	}
	// With neither, the placeholder must be obviously a placeholder — and lowercase,
	// since an uppercase image reference is invalid.
	got, _ := resolveImage("", "", "web")
	if !strings.Contains(got, "change-me") {
		t.Errorf("resolveImage = %q, want an obvious placeholder", got)
	}
}

func TestEnvironmentsFromError(t *testing.T) {
	_, err := config.Load(config.LoadOptions{
		Path: writeTempConfig(t, `
app: web
image: ghcr.io/acme/web
environments:
  live:
  production:
`),
		Strict: true,
	})
	if err == nil {
		t.Fatal("expected an error when no environment is selected and staging is absent")
	}

	// `config validate` recovers the environment list from this message.
	names := environmentsFromError(err)
	if len(names) != 2 {
		t.Fatalf("environmentsFromError = %v, want two names", names)
	}
	if names[0] != "live" || names[1] != "production" {
		t.Errorf("environmentsFromError = %v, want sorted names", names)
	}
}

func TestIsProductionLike(t *testing.T) {
	for _, env := range []string{"production", "prod", "live", "main"} {
		if !isProductionLike(env) {
			t.Errorf("%q should be treated as production-like", env)
		}
	}
	for _, env := range []string{"staging", "preview", "dev", "test"} {
		if isProductionLike(env) {
			t.Errorf("%q should not prompt for confirmation", env)
		}
	}
}

func TestShortDigest(t *testing.T) {
	if got := shortDigest("sha256:abcdef0123456789abcdef"); got != "sha256:abcdef012345" {
		t.Errorf("shortDigest = %q", got)
	}
}

func TestHumanAge(t *testing.T) {
	tests := []struct {
		seconds int
		want    string
	}{
		{30, "30s"},
		{90, "1m"},
		{3700, "1h"},
		{90000, "1d"},
	}
	for _, tt := range tests {
		if got := humanAge(secondsDuration(tt.seconds)); got != tt.want {
			t.Errorf("humanAge(%ds) = %q, want %q", tt.seconds, got, tt.want)
		}
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("short", 10); got != "short" {
		t.Errorf("truncate = %q", got)
	}
	got := truncate("a very long message indeed", 10)
	if len([]rune(got)) != 10 {
		t.Errorf("truncate = %q, want 10 runes", got)
	}
}

// TestInitWithoutRegistryProducesAValidConfig guards a regression that made the
// simplest possible invocation fail.
//
// `buidl init` with no --registry writes a placeholder image reference. An
// uppercase placeholder is not a valid image reference, so the config buidl had
// just written failed its own validation.
func TestInitWithoutRegistryProducesAValidConfig(t *testing.T) {
	image, err := resolveImage("", "", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if image != strings.ToLower(image) {
		t.Errorf("placeholder image %q must be lowercase to be a valid reference", image)
	}

	rendered := renderConfig(project.Detection{
		Kind: project.KindGo, Stack: project.KindGo,
		Name: "hello", Port: 8080,
	}, image)

	// Exactly what init does after writing the file.
	if _, err := config.Load(config.LoadOptions{
		Path:        writeTempConfig(t, rendered),
		Environment: "staging",
		Strict:      true,
		Vars:        map[string]string{"BUIDL_SLUG": "example"},
	}); err != nil {
		t.Fatalf("a config generated without --registry must validate: %v", err)
	}
}
