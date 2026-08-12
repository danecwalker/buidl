package gitinfo

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSlug(t *testing.T) {
	tests := []struct{ in, want string }{
		{"main", "main"},
		{"feature/add-oauth", "feature-add-oauth"},
		{"feature/Add-OAuth!", "feature-add-oauth"},
		{"DW-123_fix", "dw-123-fix"},
		{"release/v1.2.3", "release-v1-2-3"},
		{"", "unknown"},
		{"///", "unknown"},
	}
	for _, tt := range tests {
		if got := Slug(tt.in); got != tt.want {
			t.Errorf("Slug(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestSlugIsHostnameSafe(t *testing.T) {
	// Slugs become hostname and namespace components, so they must be valid DNS
	// labels with room left for buidl's prefixes and suffixes.
	long := strings.Repeat("very-long-branch-name/", 10)
	slug := Slug(long)

	if len(slug) > 40 {
		t.Errorf("slug length = %d, want <= 40 to leave room for prefixes", len(slug))
	}
	if strings.HasPrefix(slug, "-") || strings.HasSuffix(slug, "-") {
		t.Errorf("slug %q must not start or end with a dash", slug)
	}
	for _, r := range slug {
		valid := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-'
		if !valid {
			t.Errorf("slug %q contains invalid character %q", slug, r)
		}
	}
}

func TestLoadOutsideRepositoryIsNotAnError(t *testing.T) {
	// buidl must work in a directory that is not a git repo; it just records less.
	info, err := Load(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if info.Available {
		t.Error("Available should be false outside a repository")
	}
}

func TestLoadInRepository(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	run("init", "-q", "-b", "main")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "Test")
	run("commit", "-q", "--allow-empty", "-m", "initial commit")

	info, err := Load(context.Background(), dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if !info.Available {
		t.Fatal("Available should be true inside a repository")
	}
	if len(info.SHA) != 40 {
		t.Errorf("SHA = %q, want a full 40-character sha", info.SHA)
	}
	if info.Branch != "main" {
		t.Errorf("Branch = %q, want main", info.Branch)
	}
	if info.Subject != "initial commit" {
		t.Errorf("Subject = %q", info.Subject)
	}
	if info.Dirty {
		t.Error("a freshly committed tree should not be dirty")
	}

	// An untracked file makes the tree dirty, which must be detected: it means
	// the release is not reproducible from any commit.
	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err = Load(context.Background(), dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !info.Dirty {
		t.Error("expected Dirty to be true with an untracked file")
	}
}
