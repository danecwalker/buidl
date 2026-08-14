package secrets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetPreservesCommentsAndOtherKeys(t *testing.T) {
	root := project(t, map[string]string{
		filepath.Join(Directory, SharedFile): "# keep me\nEXISTING=one\n",
	})

	rel, err := Set(root, "", "DATABASE_URL", "postgres://local")
	if err != nil {
		t.Fatal(err)
	}
	if rel != DefaultFile {
		t.Errorf("rel = %q", rel)
	}

	got := read(t, filepath.Join(root, DefaultFile))
	if !strings.Contains(got, "# keep me") {
		t.Errorf("comment lost:\n%s", got)
	}
	if !strings.Contains(got, "EXISTING=one") {
		t.Errorf("existing key lost:\n%s", got)
	}
	if !strings.Contains(got, "DATABASE_URL=postgres://local") {
		t.Errorf("new key missing:\n%s", got)
	}

	if _, err := Set(root, "", "EXISTING", "two"); err != nil {
		t.Fatal(err)
	}
	got = read(t, filepath.Join(root, DefaultFile))
	if strings.Count(got, "EXISTING=") != 1 {
		t.Errorf("Set should replace in place:\n%s", got)
	}
	if !strings.Contains(got, "EXISTING=two") {
		t.Errorf("replaced value missing:\n%s", got)
	}
}

func TestSetEnvironmentFile(t *testing.T) {
	root := t.TempDir()
	rel, err := Set(root, "production", "TOKEN", "prod-value")
	if err != nil {
		t.Fatal(err)
	}
	if rel != EnvironmentFile("production") {
		t.Errorf("rel = %q", rel)
	}
	got := read(t, filepath.Join(root, rel))
	if !strings.Contains(got, "TOKEN=prod-value") {
		t.Errorf("got %q", got)
	}
	info, err := os.Stat(filepath.Join(root, rel))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Errorf("secrets file should be 0600, got %04o", info.Mode().Perm())
	}
}

func TestUnsetRemovesKey(t *testing.T) {
	root := project(t, map[string]string{
		filepath.Join(Directory, SharedFile): "KEEP=yes\nDROP=no\n",
	})
	if err := Unset(root, "", "DROP"); err != nil {
		t.Fatal(err)
	}
	got := read(t, filepath.Join(root, DefaultFile))
	if strings.Contains(got, "DROP=") {
		t.Errorf("DROP still present:\n%s", got)
	}
	if !strings.Contains(got, "KEEP=yes") {
		t.Errorf("KEEP lost:\n%s", got)
	}
}

func TestSetQuotesValuesWithSpaces(t *testing.T) {
	root := t.TempDir()
	if _, err := Set(root, "", "NOTE", "hello world"); err != nil {
		t.Fatal(err)
	}
	res, err := Resolve(Options{Root: root, Names: []string{"NOTE"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Values["NOTE"] != "hello world" {
		t.Errorf("NOTE = %q", res.Values["NOTE"])
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
