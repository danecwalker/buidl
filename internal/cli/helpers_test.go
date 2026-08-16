package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danecwalker/buidl/internal/config"
	"github.com/danecwalker/buidl/internal/inventory"
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

func TestRequireSideloadServers(t *testing.T) {
	app, _ := newTestApp(t, ui.ModePlain)
	app.cfg = &config.Config{Image: "buidl.local/web"}
	err := app.requireSideloadServers()
	if err == nil {
		t.Fatal("expected an error with no servers")
	}
	if !strings.Contains(err.Error(), "add server") {
		t.Errorf("error should tell the user to add a server, got: %v", err)
	}

	app.cfg.Infra = &config.Infra{
		Servers: []inventory.Server{{Host: "10.0.0.1", Role: inventory.RoleControlPlane}},
	}
	if err := app.requireSideloadServers(); err != nil {
		t.Fatalf("servers present: %v", err)
	}
}

func TestSideloadLocalImageDeletesArchive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "img.tar")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	app, _ := newTestApp(t, ui.ModePlain)
	app.cfg = &config.Config{Image: "ghcr.io/acme/web"}
	if err := app.sideloadLocalImage(t.Context(), nil, app.cfg, path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("archive should be deleted after deploy even when it is not transferred")
	}

	path = filepath.Join(t.TempDir(), "local.tar")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	app.cfg = &config.Config{Image: "buidl.local/web"}
	err := app.sideloadLocalImage(t.Context(), nil, app.cfg, path)
	if err == nil {
		t.Fatal("expected sideload to fail without servers")
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatal("archive should be deleted after a failed sideload")
	}
}
