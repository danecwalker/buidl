package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danecwalker/buidl/internal/config"
	"github.com/danecwalker/buidl/internal/project"
	"github.com/danecwalker/buidl/internal/secrets"
	"github.com/danecwalker/buidl/internal/ui"
)

func TestAddDatabasePostgres(t *testing.T) {
	path := writeTempConfig(t, renderConfig(project.Detection{
		Kind: project.KindGo, Name: "web", Port: 8080, HealthPath: "/up",
	}, "ghcr.io/acme/web"))

	app, out := newTestApp(t, ui.ModePlain)
	app.opts.configPath = path

	cmd := newAddCmd(app)
	cmd.SetArgs([]string{"--database", "postgres"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if !strings.Contains(s, "type: postgres") {
		t.Errorf("typed accessory missing:\n%s", s)
	}
	if !strings.Contains(s, "Used when -e is omitted") {
		t.Errorf("init comments were stripped:\n%s", s)
	}

	res, err := config.Load(config.LoadOptions{Path: path, Environment: "staging", Strict: true})
	if err != nil {
		t.Fatalf("typed postgres must load: %v", err)
	}
	acc := res.Config.Accessories["postgres"]
	if acc.Image != config.DefaultPostgresImage {
		t.Errorf("image = %q", acc.Image)
	}
	if !containsString(res.Config.Env.Secret, "DATABASE_URL") {
		t.Errorf("DATABASE_URL not declared: %v", res.Config.Env.Secret)
	}

	root := filepath.Dir(path)
	got, err := os.ReadFile(filepath.Join(root, secrets.DefaultFile))
	if err != nil {
		t.Fatal(err)
	}
	sec := string(got)
	if !strings.Contains(sec, "POSTGRES_PASSWORD=") {
		t.Errorf("password missing from secrets file")
	}
	if !strings.Contains(sec, "DATABASE_URL=") {
		t.Errorf("DATABASE_URL missing from secrets file")
	}

	// The generated password must never appear in command output.
	for _, line := range strings.Split(sec, "\n") {
		name, value, ok := strings.Cut(line, "=")
		if !ok || name != "POSTGRES_PASSWORD" || value == "" {
			continue
		}
		if strings.Contains(out.String(), value) {
			t.Errorf("password leaked into command output:\n%s", out)
		}
	}

	cmd = newAddCmd(app)
	cmd.SetArgs([]string{"--database", "postgres"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("second add of the same accessory must fail")
	}
}

func TestAddDatabaseRedis(t *testing.T) {
	path := writeTempConfig(t, `
app: web
image: ghcr.io/acme/web
`)
	app, _ := newTestApp(t, ui.ModePlain)
	app.opts.configPath = path

	cmd := newAddCmd(app)
	cmd.SetArgs([]string{"--database", "redis", "--disk", "2Gi"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	res, err := config.Load(config.LoadOptions{Path: path, Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	acc := res.Config.Accessories["redis"]
	if acc.Type != "redis" {
		t.Errorf("type = %q", acc.Type)
	}
	if acc.Storage != "2Gi" {
		t.Errorf("storage = %q, want the --disk value", acc.Storage)
	}
}

func TestAddServiceSecondNameRejected(t *testing.T) {
	path := writeTempConfig(t, `
app: web
image: ghcr.io/acme/web
`)
	app, _ := newTestApp(t, ui.ModePlain)
	app.opts.configPath = path

	cmd := newAddCmd(app)
	cmd.SetArgs([]string{"--service", "--name", "api"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected a second named service to fail")
	}
	if !strings.Contains(err.Error(), "not supported yet") {
		t.Errorf("error should say this is not supported, got: %v", err)
	}
}

func TestAddServiceUpdatesHost(t *testing.T) {
	path := writeTempConfig(t, `
app: web
image: ghcr.io/acme/web
defaultEnvironment: staging
environments:
  staging:
    proxy:
      host: staging.example.com
`)
	app, _ := newTestApp(t, ui.ModePlain)
	app.opts.configPath = path

	cmd := newAddCmd(app)
	cmd.SetArgs([]string{"--service", "--host", "web.example.com", "--path", "/healthz"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	res, err := config.Load(config.LoadOptions{Path: path, Environment: "staging", Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Config.Proxy.Host != "web.example.com" {
		t.Errorf("host = %q", res.Config.Proxy.Host)
	}
	if res.Config.Deploy.Healthcheck.Path != "/healthz" {
		t.Errorf("path = %q", res.Config.Deploy.Healthcheck.Path)
	}
}
