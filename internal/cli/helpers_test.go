package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danecwalker/buidl/internal/config"
	"github.com/danecwalker/buidl/internal/secrets"
	"github.com/danecwalker/buidl/internal/ui"
)

// writeTempConfig writes a buidl.yaml into a temp dir and returns its path.
func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "buidl.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	return path
}

// secondsDuration converts seconds to a Duration, keeping table tests readable.
func secondsDuration(s int) time.Duration {
	return time.Duration(s) * time.Second
}

func TestResolveSecretsIncludesAccessoryPassword(t *testing.T) {
	path := writeTempConfig(t, `
app: web
image: ghcr.io/acme/web
env:
  secret: [DATABASE_URL]
accessories:
  postgres:
    type: postgres
`)
	const password = "s3cret-from-file-do-not-print"
	root := filepath.Dir(path)
	if _, err := secrets.Set(root, "", "POSTGRES_PASSWORD", password); err != nil {
		t.Fatal(err)
	}

	res, err := config.Load(config.LoadOptions{Path: path, Strict: true})
	if err != nil {
		t.Fatal(err)
	}

	app, out := newTestApp(t, ui.ModePlain)
	app.cfg = res.Config
	app.root = root
	app.path = path

	got, err := app.resolveSecrets()
	if err != nil {
		t.Fatal(err)
	}
	if got["POSTGRES_PASSWORD"] != password {
		t.Errorf("POSTGRES_PASSWORD = %q, want the value from .buidl/secrets", got["POSTGRES_PASSWORD"])
	}
	if !strings.Contains(got["DATABASE_URL"], password) {
		t.Errorf("DATABASE_URL should be derived from the accessory password, got %q", got["DATABASE_URL"])
	}
	if containsString(res.Config.Env.Secret, "POSTGRES_PASSWORD") {
		t.Error("POSTGRES_PASSWORD must stay off the app's env.secret")
	}
	if strings.Contains(out.String(), password) {
		t.Errorf("password leaked into command output:\n%s", out)
	}
}

func TestResolveSecretsMissingAccessoryPassword(t *testing.T) {
	path := writeTempConfig(t, `
app: web
image: ghcr.io/acme/web
accessories:
  postgres:
    type: postgres
`)
	res, err := config.Load(config.LoadOptions{Path: path, Strict: true})
	if err != nil {
		t.Fatal(err)
	}

	app, _ := newTestApp(t, ui.ModePlain)
	app.cfg = res.Config
	app.root = filepath.Dir(path)
	app.path = path

	_, err = app.resolveSecrets()
	if err == nil {
		t.Fatal("expected a missing accessory secret to fail")
	}
	if !strings.Contains(err.Error(), "POSTGRES_PASSWORD") {
		t.Errorf("error should name the missing secret, got: %v", err)
	}
	if !strings.Contains(err.Error(), ".buidl/secrets") {
		t.Errorf("error should point at .buidl/secrets, got: %v", err)
	}
}
