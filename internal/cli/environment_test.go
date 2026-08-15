package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/danecwalker/buidl/internal/config"
	"github.com/danecwalker/buidl/internal/project"
	"github.com/danecwalker/buidl/internal/ui"
)

func TestEnvironmentListAndNew(t *testing.T) {
	path := writeTempConfig(t, renderConfig(project.Detection{
		Kind: project.KindGo, Name: "web", Port: 8080, HealthPath: "/up",
	}, "ghcr.io/acme/web"))

	app, out := newTestApp(t, ui.ModePlain)
	app.opts.configPath = path

	cmd := newEnvironmentCmd(app)
	cmd.SetArgs([]string{"list"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "single environment") && !strings.Contains(got, "no environments") {
		t.Errorf("list on a slim init file should say there are no overlays:\n%s", got)
	}
	// The rename: env list is environments, not variables.
	if strings.Contains(got, "LOG_LEVEL") {
		t.Errorf("environment list printed variables:\n%s", got)
	}

	out.Reset()
	cmd = newEnvironmentCmd(app)
	cmd.SetArgs([]string{"new", "qa", "--host", "qa.example.com"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if !strings.Contains(s, "qa:") {
		t.Errorf("qa overlay missing:\n%s", s)
	}
	if !strings.Contains(s, "qa.example.com") {
		t.Errorf("host missing:\n%s", s)
	}
	// init comments must survive the edit.
	if !strings.Contains(s, "buidl configuration") {
		t.Errorf("init comments were stripped:\n%s", s)
	}

	f, err := config.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if f.DefaultEnvironment() != "qa" {
		t.Errorf("first environment should become the default, got %q", f.DefaultEnvironment())
	}

	res, err := config.Load(config.LoadOptions{Path: path, Environment: "qa", Strict: true})
	if err != nil {
		t.Fatalf("qa must load: %v", err)
	}
	if res.Config.Proxy.Host != "qa.example.com" {
		t.Errorf("host = %q", res.Config.Proxy.Host)
	}
	if !strings.Contains(res.Config.Deploy.Kubernetes.Namespace, "qa") {
		t.Errorf("namespace = %q", res.Config.Deploy.Kubernetes.Namespace)
	}
}

func TestEnvironmentNewFromExisting(t *testing.T) {
	path := writeTempConfig(t, `
app: web
image: ghcr.io/acme/web
defaultEnvironment: staging
environments:
  staging:
    proxy:
      host: staging.example.com
    env:
      clear:
        LOG_LEVEL: debug
`)
	app, _ := newTestApp(t, ui.ModePlain)
	app.opts.configPath = path

	cmd := newEnvironmentCmd(app)
	cmd.SetArgs([]string{"new", "qa", "--from", "staging"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	res, err := config.Load(config.LoadOptions{Path: path, Environment: "qa", Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Config.Env.Clear["LOG_LEVEL"] != "debug" {
		t.Errorf("clone lost LOG_LEVEL: %v", res.Config.Env.Clear)
	}
}

func TestEnvironmentNewRejectsDuplicate(t *testing.T) {
	path := writeTempConfig(t, `
app: web
image: ghcr.io/acme/web
environments:
  staging: {}
`)
	app, _ := newTestApp(t, ui.ModePlain)
	app.opts.configPath = path

	cmd := newEnvironmentCmd(app)
	cmd.SetArgs([]string{"new", "staging"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected duplicate environment to fail")
	}
}

func TestEnvironmentSetAndDelete(t *testing.T) {
	path := writeTempConfig(t, `
app: web
image: ghcr.io/acme/web
defaultEnvironment: staging
environments:
  staging:
    proxy: {host: staging.example.com}
  production:
    proxy: {host: example.com}
`)
	app, out := newTestApp(t, ui.ModePlain)
	app.opts.configPath = path

	cmd := newEnvironmentCmd(app)
	cmd.SetArgs([]string{"set", "production"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	f, err := config.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if f.DefaultEnvironment() != "production" {
		t.Errorf("default = %q", f.DefaultEnvironment())
	}

	out.Reset()
	cmd = newEnvironmentCmd(app)
	cmd.SetArgs([]string{"delete", "production"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("deleting the default environment must require --force")
	}

	cmd = newEnvironmentCmd(app)
	cmd.SetArgs([]string{"delete", "production", "--force"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	f, err = config.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if containsString(f.EnvironmentNames(), "production") {
		t.Errorf("production still present: %v", f.EnvironmentNames())
	}
	// Staging remains, so it becomes the default rather than leaving
	// defaultEnvironment pointing at a missing overlay.
	if f.DefaultEnvironment() != "staging" {
		t.Errorf("default after force-delete = %q, want staging", f.DefaultEnvironment())
	}
}
