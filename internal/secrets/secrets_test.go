package secrets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// project builds a temp project from a map of relative path to contents.
func project(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0o600)
		if strings.Contains(rel, CommonFile) || strings.HasPrefix(filepath.Base(rel), ".env") {
			mode = 0o644
		}
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// clearEnv removes variables so tests are not affected by the host environment.
func clearEnv(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		t.Setenv(name, "")
	}
}

func TestResolveFromEnvironment(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://from-env")

	res, err := Resolve(Options{Root: t.TempDir(), Names: []string{"DATABASE_URL"}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Values["DATABASE_URL"] != "postgres://from-env" {
		t.Errorf("value = %q", res.Values["DATABASE_URL"])
	}
	if res.Sources["DATABASE_URL"] != SourceEnv {
		t.Errorf("source = %q, want environment", res.Sources["DATABASE_URL"])
	}
}

// TestLayerPrecedence pins the whole resolution order in one test, since getting
// it wrong means deploying the wrong credential.
func TestLayerPrecedence(t *testing.T) {
	clearEnv(t, "TOKEN")

	root := project(t, map[string]string{
		".env":                               "TOKEN=from-dotenv\n",
		".env.production":                    "TOKEN=from-dotenv-production\n",
		filepath.Join(Directory, CommonFile): "TOKEN=from-common\n",
		filepath.Join(Directory, SharedFile): "TOKEN=from-shared\n",
	})

	base := Options{Root: root, Environment: "production", Names: []string{"TOKEN"}, Dotenv: true}

	// With every file present, the per-environment buidl file would win, but it
	// does not exist yet, so the shared one does.
	res, err := Resolve(base)
	if err != nil {
		t.Fatal(err)
	}
	if res.Values["TOKEN"] != "from-shared" {
		t.Errorf("TOKEN = %q, want the shared buidl file to beat dotenv and common", res.Values["TOKEN"])
	}

	// Add the per-environment file: it should now win.
	envPath := filepath.Join(root, EnvironmentFile("production"))
	if err := os.WriteFile(envPath, []byte("TOKEN=from-env-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err = Resolve(base)
	if err != nil {
		t.Fatal(err)
	}
	if res.Values["TOKEN"] != "from-env-file" {
		t.Errorf("TOKEN = %q, want secrets.production to win", res.Values["TOKEN"])
	}

	// The process environment beats every file, so CI is never overridden.
	t.Setenv("TOKEN", "from-process-env")
	res, err = Resolve(base)
	if err != nil {
		t.Fatal(err)
	}
	if res.Values["TOKEN"] != "from-process-env" {
		t.Errorf("TOKEN = %q, want the process environment to win", res.Values["TOKEN"])
	}
}

func TestDotenvIsOptIn(t *testing.T) {
	clearEnv(t, "API_KEY")
	root := project(t, map[string]string{".env": "API_KEY=from-dotenv\n"})

	// Off by default: a project's .env must not be deployed without being asked.
	res, err := Resolve(Options{Root: root, Names: []string{"API_KEY"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Missing) != 1 {
		t.Errorf("expected API_KEY to be missing with dotenv off, got %v", res.Values)
	}

	res, err = Resolve(Options{Root: root, Names: []string{"API_KEY"}, Dotenv: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Values["API_KEY"] != "from-dotenv" {
		t.Errorf("API_KEY = %q", res.Values["API_KEY"])
	}
	if res.Sources["API_KEY"] != SourceDotenv {
		t.Errorf("source = %q, want .env", res.Sources["API_KEY"])
	}
}

func TestPerEnvironmentDotenv(t *testing.T) {
	clearEnv(t, "REGION")
	root := project(t, map[string]string{
		".env":            "REGION=default\n",
		".env.production": "REGION=us-east\n",
		".env.staging":    "REGION=eu-west\n",
	})

	for env, want := range map[string]string{"production": "us-east", "staging": "eu-west"} {
		res, err := Resolve(Options{Root: root, Environment: env, Names: []string{"REGION"}, Dotenv: true})
		if err != nil {
			t.Fatal(err)
		}
		if res.Values["REGION"] != want {
			t.Errorf("%s REGION = %q, want %q", env, res.Values["REGION"], want)
		}
	}
}

// TestLocalDotenvFilesAreNeverRead is the important safety test here.
//
// By convention .env.local is gitignored machine-local dev config. Reading it at
// deploy time would ship a developer's localhost database URL to production.
func TestLocalDotenvFilesAreNeverRead(t *testing.T) {
	clearEnv(t, "DATABASE_URL")
	root := project(t, map[string]string{
		".env":                  "DATABASE_URL=postgres://prod-placeholder\n",
		".env.local":            "DATABASE_URL=postgres://localhost:5432/dev\n",
		".env.production.local": "DATABASE_URL=postgres://localhost:5432/also-dev\n",
	})

	res, err := Resolve(Options{
		Root: root, Environment: "production",
		Names: []string{"DATABASE_URL"}, Dotenv: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(res.Values["DATABASE_URL"], "localhost") {
		t.Fatalf("a .local dev value leaked into a deploy: %q", res.Values["DATABASE_URL"])
	}
	if res.Values["DATABASE_URL"] != "postgres://prod-placeholder" {
		t.Errorf("DATABASE_URL = %q", res.Values["DATABASE_URL"])
	}
	// Skipping them silently would look like a bug, so it is reported.
	if len(res.Warnings) != 2 {
		t.Errorf("expected a warning per ignored .local file, got %v", res.Warnings)
	}
	for _, w := range res.Warnings {
		if !strings.Contains(w, "never deployed") {
			t.Errorf("warning should explain why: %q", w)
		}
	}
}

func TestOnlyDeclaredNamesAreDeployed(t *testing.T) {
	clearEnv(t, "WANTED", "UNWANTED")
	root := project(t, map[string]string{
		".env": "WANTED=yes\nUNWANTED=should-not-ship\nALSO_UNWANTED=nope\n",
	})

	res, err := Resolve(Options{Root: root, Names: []string{"WANTED"}, Dotenv: true})
	if err != nil {
		t.Fatal(err)
	}

	// A dotenv file supplies values, not the list of what to ship.
	if _, shipped := res.Values["UNWANTED"]; shipped {
		t.Error("an undeclared variable must not be deployed")
	}
	if len(res.Values) != 1 {
		t.Errorf("expected exactly the declared name, got %v", keysOf(res.Values))
	}
	// But it is reported, so "works locally, missing in the cluster" is visible.
	if len(res.Discovered) != 2 {
		t.Errorf("Discovered = %v, want the two undeclared names", res.Discovered)
	}
	if res.Discovered[0] != "ALSO_UNWANTED" || res.Discovered[1] != "UNWANTED" {
		t.Errorf("Discovered = %v, want it sorted", res.Discovered)
	}
}

func TestCommonFileIndirection(t *testing.T) {
	clearEnv(t, "DATABASE_URL")
	t.Setenv("PROD_DATABASE_URL", "postgres://from-indirection")

	root := project(t, map[string]string{
		filepath.Join(Directory, CommonFile): "DATABASE_URL=$PROD_DATABASE_URL\n",
	})

	res, err := Resolve(Options{Root: root, Names: []string{"DATABASE_URL"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Values["DATABASE_URL"] != "postgres://from-indirection" {
		t.Errorf("value = %q, want the indirected value", res.Values["DATABASE_URL"])
	}
}

func TestUnresolvedIndirectionIsNotAValue(t *testing.T) {
	clearEnv(t, "DATABASE_URL", "MISSING_UPSTREAM")
	root := project(t, map[string]string{
		filepath.Join(Directory, CommonFile): "DATABASE_URL=$MISSING_UPSTREAM\n",
	})

	res, err := Resolve(Options{Root: root, Names: []string{"DATABASE_URL"}})
	if err != nil {
		t.Fatal(err)
	}
	// Deploying the literal "$MISSING_UPSTREAM" as a database URL would be worse
	// than failing.
	if v, ok := res.Values["DATABASE_URL"]; ok {
		t.Errorf("unresolved indirection became a value: %q", v)
	}
	if len(res.Missing) != 1 {
		t.Errorf("expected DATABASE_URL to be reported missing, got %v", res.Missing)
	}
}

func TestQuotedAndExportedValues(t *testing.T) {
	clearEnv(t, "QUOTED", "SINGLE", "EXPORTED")
	root := project(t, map[string]string{
		filepath.Join(Directory, SharedFile): `
QUOTED="value with spaces"
SINGLE='single quoted'
export EXPORTED=abc123
`,
	})

	res, err := Resolve(Options{Root: root, Names: []string{"QUOTED", "SINGLE", "EXPORTED"}})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"QUOTED":   "value with spaces",
		"SINGLE":   "single quoted",
		"EXPORTED": "abc123",
	}
	for name, expected := range want {
		if res.Values[name] != expected {
			t.Errorf("%s = %q, want %q", name, res.Values[name], expected)
		}
	}
}

func TestMissingSecretsAreSorted(t *testing.T) {
	clearEnv(t, "ZULU", "ALPHA")
	res, err := Resolve(Options{Root: t.TempDir(), Names: []string{"ZULU", "ALPHA"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Missing) != 2 || res.Missing[0] != "ALPHA" {
		t.Errorf("Missing = %v, want sorted", res.Missing)
	}
}

func TestEmptyValueCountsAsMissing(t *testing.T) {
	t.Setenv("TOKEN", "")
	res, err := Resolve(Options{Root: t.TempDir(), Names: []string{"TOKEN"}})
	if err != nil {
		t.Fatal(err)
	}
	// An empty secret is almost always a misconfiguration, not an intent.
	if len(res.Missing) != 1 {
		t.Errorf("Missing = %v, want TOKEN treated as missing", res.Missing)
	}
}

func TestAbsentFilesAreNotAnError(t *testing.T) {
	// In CI everything comes from the environment; no file will exist.
	res, err := Resolve(Options{Root: t.TempDir(), Names: []string{"ANYTHING"}, Dotenv: true})
	if err != nil {
		t.Fatalf("Resolve should tolerate missing files: %v", err)
	}
	if len(res.Files) != 0 {
		t.Errorf("Files = %v, want empty", res.Files)
	}
}

func TestFilesReadAreReported(t *testing.T) {
	clearEnv(t, "TOKEN")
	root := project(t, map[string]string{
		".env":                               "TOKEN=a\n",
		filepath.Join(Directory, SharedFile): "TOKEN=b\n",
	})

	res, err := Resolve(Options{Root: root, Names: []string{"TOKEN"}, Dotenv: true})
	if err != nil {
		t.Fatal(err)
	}
	// Knowing which files were consulted is how a user debugs a wrong value.
	if len(res.Files) != 2 {
		t.Errorf("Files = %v, want both files listed", res.Files)
	}
}

func TestExplicitDotenvFilesOverrideDiscovery(t *testing.T) {
	clearEnv(t, "TOKEN")
	root := project(t, map[string]string{
		".env":          "TOKEN=discovered\n",
		"config/custom": "TOKEN=explicit\n",
	})

	res, err := Resolve(Options{
		Root: root, Names: []string{"TOKEN"},
		Dotenv: true, DotenvFiles: []string{"config/custom"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Values["TOKEN"] != "explicit" {
		t.Errorf("TOKEN = %q, want the explicitly listed file", res.Values["TOKEN"])
	}
}

func TestMalformedLineIsRejected(t *testing.T) {
	root := project(t, map[string]string{
		filepath.Join(Directory, SharedFile): "THIS_IS_NOT_VALID\n",
	})
	if _, err := Resolve(Options{Root: root, Names: []string{"ANY"}}); err == nil {
		t.Fatal("expected an error for a malformed line")
	}
}

func TestPermissionWarnings(t *testing.T) {
	root := project(t, map[string]string{
		filepath.Join(Directory, SharedFile): "TOKEN=abc\n",
	})

	// 0600 is correct and must not warn.
	if w := PermissionWarnings(root, "production"); len(w) != 0 {
		t.Errorf("unexpected warning for mode 0600: %v", w)
	}

	if err := os.Chmod(filepath.Join(root, DefaultFile), 0o644); err != nil {
		t.Fatal(err)
	}
	if w := PermissionWarnings(root, "production"); len(w) != 1 {
		t.Errorf("expected a warning for a world-readable values file, got %v", w)
	}
}

// TestCommonFileIsNotPermissionChecked confirms the committed file is exempt: it
// holds no values, so world-readable is correct for it.
func TestCommonFileIsNotPermissionChecked(t *testing.T) {
	root := project(t, map[string]string{
		filepath.Join(Directory, CommonFile): "DATABASE_URL=$PROD_DATABASE_URL\n",
	})
	if err := os.Chmod(filepath.Join(root, CommonPath()), 0o644); err != nil {
		t.Fatal(err)
	}
	if w := PermissionWarnings(root, "production"); len(w) != 0 {
		t.Errorf("secrets-common should not be flagged: %v", w)
	}
}

func TestEnvironmentFilePaths(t *testing.T) {
	if got := EnvironmentFile("production"); got != ".buidl/secrets.production" {
		t.Errorf("EnvironmentFile = %q", got)
	}
	// No environment means the shared file, not "secrets." with a dangling dot.
	if got := EnvironmentFile(""); got != DefaultFile {
		t.Errorf("EnvironmentFile(\"\") = %q, want %q", got, DefaultFile)
	}
	if got := CommonPath(); got != ".buidl/secrets-common" {
		t.Errorf("CommonPath = %q", got)
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
