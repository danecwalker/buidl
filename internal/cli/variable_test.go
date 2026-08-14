package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danecwalker/buidl/internal/config"
	"github.com/danecwalker/buidl/internal/secrets"
	"github.com/danecwalker/buidl/internal/ui"
)

func TestVariableSetAndListNeverPrintsSecret(t *testing.T) {
	path := writeTempConfig(t, `
app: web
image: ghcr.io/acme/web
env:
  clear:
    LOG_LEVEL: info
  secret: []
`)
	app, out := newTestApp(t, ui.ModePlain)
	app.opts.configPath = path

	const secret = "super-secret-value-do-not-print"
	cmd := newVariableCmd(app)
	cmd.SetArgs([]string{"set", "DEMO_SECRET=" + secret})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), secret) {
		t.Fatalf("variable set printed the secret:\n%s", out)
	}

	res, err := config.Load(config.LoadOptions{Path: path, Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(res.Config.Env.Secret, "DEMO_SECRET") {
		t.Errorf("DEMO_SECRET not declared: %v", res.Config.Env.Secret)
	}

	got, err := os.ReadFile(filepath.Join(filepath.Dir(path), secrets.DefaultFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "DEMO_SECRET="+secret) {
		t.Errorf("secret file missing value:\n%s", got)
	}

	out.Reset()
	app.cfg = nil
	cmd = newVariableCmd(app)
	cmd.SetArgs([]string{"list"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	listed := out.String()
	if !strings.Contains(listed, "DEMO_SECRET") {
		t.Errorf("list missing the name:\n%s", listed)
	}
	if strings.Contains(listed, secret) {
		t.Errorf("variable list printed the secret:\n%s", listed)
	}
	if !strings.Contains(listed, "set,") {
		t.Errorf("list should report presence by length:\n%s", listed)
	}
}

func TestVariableSetClearWritesYAML(t *testing.T) {
	path := writeTempConfig(t, `
app: web
image: ghcr.io/acme/web
`)
	app, _ := newTestApp(t, ui.ModePlain)
	app.opts.configPath = path

	cmd := newVariableCmd(app)
	cmd.SetArgs([]string{"set", "LOG_LEVEL=debug", "--clear"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	res, err := config.Load(config.LoadOptions{Path: path, Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Config.Env.Clear["LOG_LEVEL"] != "debug" {
		t.Errorf("LOG_LEVEL = %q", res.Config.Env.Clear["LOG_LEVEL"])
	}
}

func TestVariableDelete(t *testing.T) {
	path := writeTempConfig(t, `
app: web
image: ghcr.io/acme/web
env:
  secret: [DEMO_SECRET]
`)
	root := filepath.Dir(path)
	if _, err := secrets.Set(root, "", "DEMO_SECRET", "x"); err != nil {
		t.Fatal(err)
	}

	app, _ := newTestApp(t, ui.ModePlain)
	app.opts.configPath = path

	cmd := newVariableCmd(app)
	cmd.SetArgs([]string{"delete", "DEMO_SECRET"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	res, err := config.Load(config.LoadOptions{Path: path, Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	if containsString(res.Config.Env.Secret, "DEMO_SECRET") {
		t.Errorf("declaration still present: %v", res.Config.Env.Secret)
	}
	if secrets.Has(root, "", "DEMO_SECRET") {
		t.Error("value still present in secrets file")
	}
}
