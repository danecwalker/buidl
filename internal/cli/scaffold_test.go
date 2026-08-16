package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/danecwalker/buidl/internal/config"
	"github.com/danecwalker/buidl/internal/project"
	"github.com/danecwalker/buidl/internal/ui"
)

// TestGithubWorkflowIsValidYAML guards the CI template.
//
// It is a Go string literal containing nested backticks, `${{ }}` expressions and
// embedded JavaScript, all of which are easy to break without noticing. A broken
// workflow would only surface as a CI syntax error in a user's repo.
func TestGithubWorkflowIsValidYAML(t *testing.T) {
	assertWorkflow := func(t *testing.T, raw string, wantJobs, wantAbsent []string, promote, destroyPreview bool) {
		t.Helper()
		var doc struct {
			Name string                    `yaml:"name"`
			On   map[string]any            `yaml:"on"`
			Jobs map[string]map[string]any `yaml:"jobs"`
		}
		if err := yaml.Unmarshal([]byte(raw), &doc); err != nil {
			t.Fatalf("the generated workflow is not valid YAML: %v", err)
		}
		if doc.Name == "" {
			t.Error("workflow has no name")
		}
		if len(doc.On) == 0 {
			t.Error("workflow has no triggers")
		}
		for _, job := range wantJobs {
			if _, ok := doc.Jobs[job]; !ok {
				t.Errorf("workflow is missing the %q job", job)
			}
		}
		for _, job := range wantAbsent {
			if _, ok := doc.Jobs[job]; ok {
				t.Errorf("workflow should not include a %q job", job)
			}
		}
		if !strings.Contains(raw, "https://github.com/danecwalker/buidl/releases/latest/download/buidl-linux-amd64") {
			t.Error("the install step should download from github.com/danecwalker/buidl")
		}
		if strings.Contains(raw, "github.com/danewalker/buidl") {
			t.Error("the workflow still points at github.com/danewalker/buidl, which does not exist")
		}
		if !strings.Contains(raw, "fetch-depth: 0") {
			t.Error("checkout should fetch full history for git provenance")
		}
		if promote != strings.Contains(raw, "buidl promote") {
			t.Errorf("promote present = %v, want %v", strings.Contains(raw, "buidl promote"), promote)
		}
		if destroyPreview != strings.Contains(raw, "buidl destroy -e preview") {
			t.Errorf("preview destroy present = %v, want %v", strings.Contains(raw, "buidl destroy -e preview"), destroyPreview)
		}
	}

	t.Run("single", func(t *testing.T) {
		raw := renderGithubWorkflow(false, false)
		assertWorkflow(t, raw, []string{"deploy"}, []string{"preview", "teardown", "staging", "production"}, false, false)
		if !strings.Contains(raw, "buidl deploy --auto-rollback") {
			t.Error("the deploy job should run buidl deploy with no -e")
		}
	})
	t.Run("staging", func(t *testing.T) {
		raw := renderGithubWorkflow(true, false)
		assertWorkflow(t, raw, []string{"staging", "production"}, []string{"deploy", "preview", "teardown"}, true, false)
	})
	t.Run("review apps", func(t *testing.T) {
		raw := renderGithubWorkflow(true, true)
		assertWorkflow(t, raw, []string{"preview", "teardown", "staging", "production"}, []string{"deploy"}, true, true)
		if !strings.Contains(raw, "types: [opened, synchronize, reopened, closed]") {
			t.Error("review-app workflow must listen for pull_request closed")
		}
	})
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

			res, err := config.Load(config.LoadOptions{
				Path:   path,
				Strict: true,
				Vars:   map[string]string{"BUIDL_SLUG": "example-branch"},
			})
			if err != nil {
				t.Fatalf("generated config did not validate:\n%v\n\nrendered config:\n%s", err, rendered)
			}

			cfg := res.Config
			if cfg.App != det.Name {
				t.Errorf("App = %q, want %q", cfg.App, det.Name)
			}
			if cfg.Environment != "default" {
				t.Errorf("Environment = %q, want default (no overlays)", cfg.Environment)
			}
			if cfg.Deploy.Port != det.Port {
				t.Errorf("Port = %d, want %d", cfg.Deploy.Port, det.Port)
			}
			if cfg.Deploy.Strategy.Type != config.StrategyBlueGreen {
				t.Errorf("strategy = %q, want bluegreen", cfg.Deploy.Strategy.Type)
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
			if !cfg.Registry.ManagesPullSecret() {
				t.Error("generated config must enable createPullSecret so the first deploy can pull")
			}
			if !cfg.Deploy.Kubernetes.CreatesNamespace() {
				t.Error("generated config must create the app namespace on first deploy")
			}
			if cfg.Deploy.Autoscale == nil {
				t.Error("generated config should default to an HPA")
			}
		})
	}
}

func TestRenderedConfigIsSingleTarget(t *testing.T) {
	rendered := renderConfig(project.Detection{
		Kind: project.KindGo, Stack: project.KindGo,
		Name: "web", Port: 8080,
	}, "ghcr.io/acme/web")

	if strings.Contains(rendered, "defaultEnvironment:") {
		t.Error("generated config should not set defaultEnvironment")
	}
	if strings.Contains(rendered, "environments:") {
		t.Error("generated config should not declare environment overlays")
	}
	if strings.Contains(rendered, "replicas: 2") || strings.Contains(rendered, "replicas: 3") {
		t.Error("generated config should not pin replica counts")
	}
	if strings.Contains(rendered, "infra:") {
		t.Error("generated config should not include an infra block; add server writes one")
	}
	if !strings.Contains(rendered, "type: bluegreen") {
		t.Error("generated config should default to a blue-green update")
	}
	// Without this, init then deploy dies on ErrImagePull against GHCR.
	if !strings.Contains(rendered, "createPullSecret: true") {
		t.Error("generated config should enable registry.createPullSecret")
	}
	// init writes the default so the generated file shows it. Omitted also
	// means true; this is visibility, not a required YAML edit.
	if !strings.Contains(rendered, "createNamespace: true") {
		t.Error("generated config should enable deploy.kubernetes.createNamespace")
	}

	res, err := config.Load(config.LoadOptions{
		Path:   writeTempConfig(t, rendered),
		Strict: true,
		Vars:   map[string]string{"BUIDL_SLUG": "example"},
	})
	if err != nil {
		t.Fatalf("generated config must load: %v", err)
	}
	if !res.Config.Registry.ManagesPullSecret() {
		t.Error("generated config must resolve createPullSecret to true")
	}
	if !res.Config.Deploy.Kubernetes.CreatesNamespace() {
		t.Error("generated config must resolve createNamespace to true")
	}
	if res.Config.Environment != "default" {
		t.Errorf("Environment = %q, want default", res.Config.Environment)
	}
	if res.Config.Deploy.Strategy.Type != config.StrategyBlueGreen {
		t.Errorf("strategy = %q, want bluegreen", res.Config.Deploy.Strategy.Type)
	}
}

func TestResolveImage(t *testing.T) {
	if got, _ := resolveImage("GHCR.io/Acme/Web", "", "web"); got != "ghcr.io/acme/web" {
		t.Errorf("resolveImage = %q, want it lowercased", got)
	}
	if got, _ := resolveImage("", "ghcr.io/acme/", "web"); got != "ghcr.io/acme/web" {
		t.Errorf("resolveImage = %q", got)
	}
	// With neither, deploy sideloads a local image. The host must be lowercase,
	// since an uppercase image reference is invalid.
	got, _ := resolveImage("", "", "web")
	if got != "buidl.local/web" {
		t.Errorf("resolveImage = %q, want buidl.local/web", got)
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
func TestInitWithoutCIDoesNotWriteWorkflow(t *testing.T) {
	dir := runInitCmd(t, "--registry", "ghcr.io/acme", "--no-ci")
	if _, err := os.Stat(filepath.Join(dir, ".github", "workflows", "deploy.yml")); !os.IsNotExist(err) {
		t.Fatalf("no-ci must not write a workflow: %v", err)
	}
	f, err := config.Open(filepath.Join(dir, "buidl.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if names := f.EnvironmentNames(); len(names) != 0 {
		t.Errorf("no-ci must not write overlays, got %v", names)
	}
}

func TestInitCIWritesSingleJobWorkflow(t *testing.T) {
	dir := runInitCmd(t, "--registry", "ghcr.io/acme", "--ci")
	raw, err := os.ReadFile(filepath.Join(dir, ".github", "workflows", "deploy.yml"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if !strings.Contains(s, "buidl deploy --auto-rollback") {
		t.Errorf("single-target CI should deploy with no -e:\n%s", s)
	}
	if strings.Contains(s, "buidl promote") {
		t.Error("CI without staging should not promote")
	}
	f, err := config.Open(filepath.Join(dir, "buidl.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if names := f.EnvironmentNames(); len(names) != 0 {
		t.Errorf("--ci alone must not write overlays, got %v", names)
	}
}

func TestInitStagingWritesOverlaysAndPromoteWorkflow(t *testing.T) {
	dir := runInitCmd(t, "--registry", "ghcr.io/acme", "--staging")
	f, err := config.Open(filepath.Join(dir, "buidl.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(f.EnvironmentNames(), "staging") || !containsString(f.EnvironmentNames(), "production") {
		t.Fatalf("environments = %v, want staging and production", f.EnvironmentNames())
	}
	if containsString(f.EnvironmentNames(), "preview") {
		t.Error("--staging without --preview must not write a preview overlay")
	}
	if f.DefaultEnvironment() != "staging" {
		t.Errorf("defaultEnvironment = %q, want staging", f.DefaultEnvironment())
	}
	raw, err := os.ReadFile(filepath.Join(dir, ".github", "workflows", "deploy.yml"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if !strings.Contains(s, "buidl deploy -e staging") {
		t.Error("staging workflow should deploy staging")
	}
	if !strings.Contains(s, "buidl promote --from staging --to production") {
		t.Error("staging workflow should promote to production")
	}
	if strings.Contains(s, "buidl destroy -e preview") {
		t.Error("staging without review apps should not tear down previews")
	}
}

func TestInitPreviewWritesReviewApps(t *testing.T) {
	dir := runInitCmd(t, "--registry", "ghcr.io/acme", "--preview")
	f, err := config.Open(filepath.Join(dir, "buidl.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"staging", "production", "preview"} {
		if !containsString(f.EnvironmentNames(), name) {
			t.Errorf("missing environment %q: %v", name, f.EnvironmentNames())
		}
	}
	raw, err := os.ReadFile(filepath.Join(dir, ".github", "workflows", "deploy.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "buidl destroy -e preview") {
		t.Error("review-app workflow must destroy the preview on PR close")
	}
}

func TestInitWizardAnswersWriteCIAndStaging(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/web\n\ngo 1.25\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	app, _ := newTestApp(t, ui.ModePlain)
	yes := true
	app.forcePrompt = &yes
	cmd := newInitCmd(app)
	cmd.SetArgs([]string{"--registry", "ghcr.io/acme"})
	cmd.SetIn(strings.NewReader("y\ny\nn\n"))
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	f, err := config.Open(filepath.Join(dir, "buidl.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(f.EnvironmentNames(), "staging") || containsString(f.EnvironmentNames(), "preview") {
		t.Errorf("wizard y/y/n should write staging but not preview: %v", f.EnvironmentNames())
	}
	if f.DefaultEnvironment() != "staging" {
		t.Errorf("defaultEnvironment = %q, want staging", f.DefaultEnvironment())
	}
	if _, err := os.Stat(filepath.Join(dir, ".github", "workflows", "deploy.yml")); err != nil {
		t.Fatalf("wizard yes to Actions must write a workflow: %v", err)
	}
}

func TestInitNoCIRejectsStaging(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/web\n\ngo 1.25\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	app, _ := newTestApp(t, ui.ModePlain)
	cmd := newInitCmd(app)
	cmd.SetArgs([]string{"--registry", "ghcr.io/acme", "--no-ci", "--staging"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected --no-ci --staging to fail")
	}
}

func TestInitWizardNoSkipsCI(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/web\n\ngo 1.25\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	app, _ := newTestApp(t, ui.ModePlain)
	yes := true
	app.forcePrompt = &yes
	cmd := newInitCmd(app)
	cmd.SetArgs([]string{"--registry", "ghcr.io/acme"})
	cmd.SetIn(strings.NewReader("n\n"))
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".github", "workflows", "deploy.yml")); !os.IsNotExist(err) {
		t.Fatalf("wizard no must not write a workflow: %v", err)
	}
	assertDetectStepClosedBeforeWrite(t, app)
}

func TestInitClosesDetectStepBeforeWizard(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/web\n\ngo 1.25\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	app, _ := newTestApp(t, ui.ModePlain)
	cmd := newInitCmd(app)
	cmd.SetArgs([]string{"--registry", "ghcr.io/acme", "--no-ci"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	assertDetectStepClosedBeforeWrite(t, app)
}

func assertDetectStepClosedBeforeWrite(t *testing.T, app *App) {
	t.Helper()
	steps := app.log.Steps()
	if len(steps) == 0 || steps[0].Name != "Detecting project" {
		t.Fatalf("first step = %+v, want Detecting project closed first", steps)
	}
	if steps[0].Status != ui.StepOK {
		t.Errorf("detect step status = %q, want ok", steps[0].Status)
	}
	for i, s := range steps[1:] {
		if s.Name == "Detecting project" {
			t.Errorf("detect step recorded again at index %d", i+1)
		}
	}
}

func runInitCmd(t *testing.T, args ...string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/web\n\ngo 1.25\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	app, _ := newTestApp(t, ui.ModePlain)
	cmd := newInitCmd(app)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestInitWithoutRegistryProducesAValidConfig(t *testing.T) {
	image, err := resolveImage("", "", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if image != "buidl.local/hello" {
		t.Errorf("image = %q, want buidl.local/hello", image)
	}

	rendered := renderConfig(project.Detection{
		Kind: project.KindGo, Stack: project.KindGo,
		Name: "hello", Port: 8080,
	}, image)

	if !strings.Contains(rendered, "createPullSecret: false") {
		t.Error("local image config must not create a pull secret")
	}
	if strings.Contains(rendered, "createPullSecret: true") {
		t.Error("local image config must not enable createPullSecret")
	}

	res, err := config.Load(config.LoadOptions{
		Path:   writeTempConfig(t, rendered),
		Strict: true,
		Vars:   map[string]string{"BUIDL_SLUG": "example"},
	})
	if err != nil {
		t.Fatalf("a config generated without --registry must validate: %v", err)
	}
	if !res.Config.LocalImage() {
		t.Error("generated config must be treated as a local image")
	}
	if res.Config.Registry.ManagesPullSecret() {
		t.Error("local image must not manage a pull secret")
	}
	if res.Config.Build.Cache != "none" {
		t.Errorf("Cache = %q, want none (no registry to store it)", res.Config.Build.Cache)
	}
}
