package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const commented = `# buidl configuration. Docs: https://github.com/danecwalker/buidl
version: 1

app: web
image: ghcr.io/acme/web

# Used when -e is omitted. Production is never implied.
defaultEnvironment: staging

env:
  clear:
    LOG_LEVEL: info
  # Names only.
  secret: []

# Environments overlay the settings above.
environments:
  staging:
    proxy:
      host: staging.example.com
  production:
    proxy:
      host: example.com
`

func TestFilePreservesComments(t *testing.T) {
	path := write(t, commented)
	f, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.SetString([]string{"defaultEnvironment"}, "production"); err != nil {
		t.Fatal(err)
	}
	if err := f.Save(); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	for _, want := range []string{
		"# buidl configuration.",
		"# Used when -e is omitted.",
		"# Environments overlay the settings above.",
		"# Names only.",
		"defaultEnvironment: production",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("saved file missing %q:\n%s", want, s)
		}
	}
	if strings.HasPrefix(s, "---") {
		t.Errorf("save introduced a document marker:\n%s", s)
	}
}

func TestFileEnvironmentAndSecretEdits(t *testing.T) {
	path := write(t, commented)
	f, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	overlay, err := OverlayNode(EnvironmentStaging, "qa", "web", "qa.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Set([]string{"environments", "qa"}, overlay); err != nil {
		t.Fatal(err)
	}
	if err := f.AppendUnique([]string{"env", "secret"}, "DATABASE_URL"); err != nil {
		t.Fatal(err)
	}
	if err := f.Save(); err != nil {
		t.Fatal(err)
	}

	res, err := Load(LoadOptions{Path: path, Environment: "qa", Strict: true})
	if err != nil {
		t.Fatalf("edited file must still load: %v", err)
	}
	if res.Config.Proxy.Host != "qa.example.com" {
		t.Errorf("qa host = %q", res.Config.Proxy.Host)
	}
	if !containsString(res.Config.Env.Secret, "DATABASE_URL") {
		t.Errorf("env.secret = %v, want DATABASE_URL", res.Config.Env.Secret)
	}

	f, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.EnvironmentNames(); !containsString(got, "qa") || !containsString(got, "staging") {
		t.Errorf("EnvironmentNames = %v", got)
	}
	if f.DefaultEnvironment() != "staging" {
		t.Errorf("DefaultEnvironment = %q", f.DefaultEnvironment())
	}
}

func TestFileAppendUniqueDoesNotDuplicate(t *testing.T) {
	path := write(t, `
app: web
image: ghcr.io/acme/web
env:
  secret: [DATABASE_URL]
`)
	f, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.AppendUnique([]string{"env", "secret"}, "DATABASE_URL"); err != nil {
		t.Fatal(err)
	}
	if err := f.AppendUnique([]string{"env", "secret"}, "REDIS_URL"); err != nil {
		t.Fatal(err)
	}
	if err := f.Save(); err != nil {
		t.Fatal(err)
	}

	res, err := Load(LoadOptions{Path: path, Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Config.Env.Secret) != 2 {
		t.Errorf("secret = %v, want two unique names", res.Config.Env.Secret)
	}
}

func TestFileDeleteEnvironment(t *testing.T) {
	path := write(t, commented)
	f, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if !f.Delete("environments", "preview") {
		// preview is not in this fixture; deleting staging should work.
	}
	if !f.Delete("environments", "staging") {
		t.Fatal("expected to delete staging")
	}
	if err := f.Save(); err != nil {
		t.Fatal(err)
	}

	f, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if containsString(f.EnvironmentNames(), "staging") {
		t.Errorf("staging should be gone: %v", f.EnvironmentNames())
	}
	if !containsString(f.EnvironmentNames(), "production") {
		t.Errorf("production should remain: %v", f.EnvironmentNames())
	}
}

func TestFilePromotesNullOverlay(t *testing.T) {
	path := write(t, `
app: web
image: ghcr.io/acme/web
environments:
  staging:
`)
	f, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.SetString([]string{"environments", "staging", "proxy", "host"}, "staging.example.com"); err != nil {
		t.Fatal(err)
	}
	if err := f.Save(); err != nil {
		t.Fatal(err)
	}

	res, err := Load(LoadOptions{Path: path, Environment: "staging", Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Config.Proxy.Host != "staging.example.com" {
		t.Errorf("host = %q", res.Config.Proxy.Host)
	}
}

func TestResolvePathWalksUp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "buidl.yaml")
	if err := os.WriteFile(path, []byte(minimal), 0o644); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(dir, "pkg", "api")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := ResolvePath(LoadOptions{Dir: nested})
	if err != nil {
		t.Fatal(err)
	}
	if got != path {
		t.Errorf("ResolvePath = %q, want %q", got, path)
	}
}

func TestCloneNodeIsIndependent(t *testing.T) {
	src, err := ParseNode("proxy:\n  host: a.example.com\n")
	if err != nil {
		t.Fatal(err)
	}
	dup := CloneNode(src)
	if err := func() error {
		// Mutate the clone's host.
		host := mapGet(mapGet(dup, "proxy"), "host")
		host.Value = "b.example.com"
		return nil
	}(); err != nil {
		t.Fatal(err)
	}
	if got := mapGet(mapGet(src, "proxy"), "host").Value; got != "a.example.com" {
		t.Errorf("clone shared state with source: %q", got)
	}
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
