package cli

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/danewalker/buidl/internal/config"
	"github.com/danewalker/buidl/internal/project"
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
	// The three-stage shape is the point of the template.
	for _, job := range []string{"preview", "staging", "production"} {
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
	// Full history is needed for buidl to record provenance.
	if !strings.Contains(githubWorkflow, "fetch-depth: 0") {
		t.Error("checkout should fetch full history for git provenance")
	}
}

// TestRenderedConfigIsValid asserts that what `init` writes actually loads.
//
// init validates its own output at runtime, but only for one environment. This
// checks every environment for several detected stacks, which is where a broken
// template would otherwise hide.
func TestRenderedConfigIsValid(t *testing.T) {
	detections := []project.Detection{
		{Kind: project.KindGo, Stack: project.KindGo, Name: "payments", Port: 8080, HealthPath: "/up"},
		{Kind: project.KindNode, Stack: project.KindNode, Name: "web", Port: 3000, HealthPath: "/up", Framework: "next"},
		{Kind: project.KindRuby, Stack: project.KindRuby, Name: "monolith", Port: 3000, HealthPath: "/up", Framework: "rails"},
		{Kind: project.KindStatic, Stack: project.KindStatic, Name: "marketing", Port: 80, HealthPath: "/"},
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
				if cfg.Deploy.Healthcheck.Path != det.HealthPath {
					t.Errorf("healthcheck path = %q, want %q", cfg.Deploy.Healthcheck.Path, det.HealthPath)
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
		})
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
  staging:
  production:
`),
		Strict: true,
	})
	if err == nil {
		t.Fatal("expected an error when no environment is selected")
	}

	// `config validate` recovers the environment list from this message.
	names := environmentsFromError(err)
	if len(names) != 2 {
		t.Fatalf("environmentsFromError = %v, want two names", names)
	}
	if names[0] != "production" || names[1] != "staging" {
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
		Name: "hello", Port: 8080, HealthPath: "/up",
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
