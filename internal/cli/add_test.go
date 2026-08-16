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
	if !strings.Contains(s, "buidl configuration") {
		t.Errorf("init comments were stripped:\n%s", s)
	}

	res, err := config.Load(config.LoadOptions{Path: path, Strict: true})
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

func TestAddPostgresSubcommand(t *testing.T) {
	path := writeTempConfig(t, `
app: web
image: ghcr.io/acme/web
`)
	app, _ := newTestApp(t, ui.ModePlain)
	app.opts.configPath = path

	cmd := newAddCmd(app)
	cmd.SetArgs([]string{"postgres", "--disk", "20Gi"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	res, err := config.Load(config.LoadOptions{Path: path, Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	acc := res.Config.Accessories["postgres"]
	if acc.Type != "postgres" {
		t.Errorf("type = %q", acc.Type)
	}
	if acc.Storage != "20Gi" {
		t.Errorf("storage = %q", acc.Storage)
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
	cmd.SetArgs([]string{"redis", "--disk", "2Gi"})
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

func TestAddHiddenAppSubcommandWritesProcessApp(t *testing.T) {
	path := writeTempConfig(t, `
app: web
image: ghcr.io/acme/web
`)
	app, _ := newTestApp(t, ui.ModePlain)
	app.opts.configPath = path

	cmd := newAddCmd(app)
	cmd.SetArgs([]string{"app", "worker", "--image", "ghcr.io/acme/web"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	res, err := config.Load(config.LoadOptions{Path: path, Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.Config.Apps["worker"]; !ok {
		t.Fatalf("apps.worker missing: %#v", res.Config.Apps)
	}
}

func TestAddProcessApp(t *testing.T) {
	path := writeTempConfig(t, `
app: web
image: ghcr.io/acme/web
`)
	app, _ := newTestApp(t, ui.ModePlain)
	app.opts.configPath = path

	cmd := newAddCmd(app)
	cmd.SetArgs([]string{"api", "--image", "ghcr.io/acme/api", "--host", "api.example.com", "--port", "3001"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	res, err := config.Load(config.LoadOptions{Path: path, Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	spec, ok := res.Config.Apps["api"]
	if !ok {
		t.Fatalf("apps.api missing: %#v", res.Config.Apps)
	}
	if spec.Image != "ghcr.io/acme/api" {
		t.Errorf("image = %q", spec.Image)
	}
	if spec.Deploy.Port != 3001 {
		t.Errorf("port = %d", spec.Deploy.Port)
	}
	if spec.Proxy.Host != "api.example.com" {
		t.Errorf("host = %q", spec.Proxy.Host)
	}

	cmd = newAddCmd(app)
	cmd.SetArgs([]string{"domain", "api-admin.example.com", "--app", "api"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	f, err := config.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.Strings("apps", "api", "proxy", "hosts"); len(got) != 1 || got[0] != "api-admin.example.com" {
		t.Errorf("alias = %v", got)
	}
}

func TestAddDomainPrimaryAndAlias(t *testing.T) {
	path := writeTempConfig(t, `
app: web
image: ghcr.io/acme/web
`)
	app, out := newTestApp(t, ui.ModePlain)
	app.opts.configPath = path

	cmd := newAddCmd(app)
	cmd.SetArgs([]string{"domain", "example.com"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	cmd = newAddCmd(app)
	cmd.SetArgs([]string{"domain", "api.example.com"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	res, err := config.Load(config.LoadOptions{Path: path, Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Config.Proxy.Host != "example.com" {
		t.Errorf("host = %q", res.Config.Proxy.Host)
	}
	if len(res.Config.Proxy.Hosts) != 1 || res.Config.Proxy.Hosts[0] != "api.example.com" {
		t.Errorf("hosts = %v, want [api.example.com]", res.Config.Proxy.Hosts)
	}
	if !res.Config.Proxy.SSL {
		t.Error("ssl should be on")
	}
	if !strings.Contains(out.String(), "alias") {
		t.Errorf("second domain should be reported as an alias:\n%s", out)
	}

	cmd = newAddCmd(app)
	cmd.SetArgs([]string{"domain", "api.example.com"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("duplicate domain must fail")
	}
}

func TestAddDomainWritesOverlayWhenDefaultEnvExists(t *testing.T) {
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
	cmd.SetArgs([]string{"domain", "api.staging.example.com"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	res, err := config.Load(config.LoadOptions{Path: path, Environment: "staging", Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Config.Proxy.Host != "staging.example.com" {
		t.Errorf("primary host = %q", res.Config.Proxy.Host)
	}
	if len(res.Config.Proxy.Hosts) != 1 || res.Config.Proxy.Hosts[0] != "api.staging.example.com" {
		t.Errorf("hosts = %v", res.Config.Proxy.Hosts)
	}
}

func TestAddDomainFillsTemplateOverlayHosts(t *testing.T) {
	path := writeTempConfig(t, `
app: web
image: ghcr.io/acme/web
defaultEnvironment: staging
environments:
  staging:
    proxy:
      host: staging.example.com
  production:
    proxy:
      host: example.com
  preview:
    proxy:
      host: ${BUIDL_SLUG}.preview.example.com
`)
	app, _ := newTestApp(t, ui.ModePlain)
	app.opts.configPath = path

	cmd := newAddCmd(app)
	cmd.SetArgs([]string{"domain", "myapp.com"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	f, err := config.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.String("environments", "production", "proxy", "host"); got != "myapp.com" {
		t.Errorf("production host = %q, want myapp.com", got)
	}
	if got := f.String("environments", "staging", "proxy", "host"); got != "staging.myapp.com" {
		t.Errorf("staging host = %q, want staging.myapp.com", got)
	}
	if got := f.String("environments", "preview", "proxy", "host"); got != "${BUIDL_SLUG}.preview.myapp.com" {
		t.Errorf("preview host = %q", got)
	}

	// A later name is an alias, not another rewrite of the apex.
	cmd = newAddCmd(app)
	cmd.SetArgs([]string{"domain", "api.myapp.com"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	f, err = config.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.String("environments", "production", "proxy", "host"); got != "myapp.com" {
		t.Errorf("second domain rewrote production: %q", got)
	}
	staging, err := config.Load(config.LoadOptions{Path: path, Environment: "staging", Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	if staging.Config.Proxy.Host != "staging.myapp.com" {
		t.Errorf("staging host after alias = %q", staging.Config.Proxy.Host)
	}
}

func TestAddServerWritesInfra(t *testing.T) {
	path := writeTempConfig(t, `
app: web
image: ghcr.io/acme/web
`)
	app, out := newTestApp(t, ui.ModePlain)
	app.opts.configPath = path

	cmd := newAddCmd(app)
	cmd.SetArgs([]string{"server", "203.0.113.10", "--email", "you@example.com"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if !strings.Contains(s, "203.0.113.10") {
		t.Errorf("server missing:\n%s", s)
	}
	if !strings.Contains(s, "you@example.com") {
		t.Errorf("email missing:\n%s", s)
	}
	if !strings.Contains(s, "distribution: k3s") {
		t.Errorf("distribution missing:\n%s", s)
	}

	res, err := config.Load(config.LoadOptions{Path: path, Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Config.Infra == nil || len(res.Config.Infra.Servers) != 1 {
		t.Fatalf("servers = %v", res.Config.Infra)
	}
	if res.Config.Infra.Servers[0].Host != "203.0.113.10" {
		t.Errorf("host = %q", res.Config.Infra.Servers[0].Host)
	}
	if res.Config.Infra.Addons.CertManagerEmail != "you@example.com" {
		t.Errorf("email = %q", res.Config.Infra.Addons.CertManagerEmail)
	}
	if !strings.Contains(out.String(), "ssh-keyscan") {
		t.Errorf("expected known_hosts hint:\n%s", out)
	}

	cmd = newAddCmd(app)
	cmd.SetArgs([]string{"server", "203.0.113.10"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("duplicate server must fail")
	}
}

func TestAddServerAfterDomainRequiresEmail(t *testing.T) {
	path := writeTempConfig(t, `
app: web
image: ghcr.io/acme/web
`)
	app, _ := newTestApp(t, ui.ModePlain)
	app.opts.configPath = path

	cmd := newAddCmd(app)
	cmd.SetArgs([]string{"domain", "example.com"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	cmd = newAddCmd(app)
	cmd.SetArgs([]string{"server", "203.0.113.10"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("server after domain without --email must fail")
	}
	if !strings.Contains(err.Error(), "--email") {
		t.Errorf("error should mention --email, got: %v", err)
	}

	cmd = newAddCmd(app)
	cmd.SetArgs([]string{"server", "203.0.113.10", "--email", "ops@example.com"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
}

func TestAddHiddenServiceFlagStillWorks(t *testing.T) {
	path := writeTempConfig(t, `
app: web
image: ghcr.io/acme/web
`)
	app, _ := newTestApp(t, ui.ModePlain)
	app.opts.configPath = path

	cmd := newAddCmd(app)
	cmd.SetArgs([]string{"--service", "--host", "web.example.com", "--path", "/healthz"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	res, err := config.Load(config.LoadOptions{Path: path, Strict: true})
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
