package build

import (
	"strings"
	"testing"
	"time"

	"github.com/danecwalker/buidl/internal/config"
	"github.com/danecwalker/buidl/internal/gitinfo"
	"github.com/danecwalker/buidl/internal/release"
)

func TestNormalizeRemote(t *testing.T) {
	tests := []struct{ in, want string }{
		{"git@github.com:acme/web.git", "https://github.com/acme/web"},
		{"git@gitlab.com:group/sub/web.git", "https://gitlab.com/group/sub/web"},
		{"https://github.com/acme/web.git", "https://github.com/acme/web"},
		{"https://github.com/acme/web", "https://github.com/acme/web"},
	}
	for _, tt := range tests {
		if got := normalizeRemote(tt.in); got != tt.want {
			t.Errorf("normalizeRemote(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestImageLabels(t *testing.T) {
	cfg := &config.Config{App: "web", Environment: "production"}
	rel := release.New("production", gitinfo.Info{
		Available: true,
		SHA:       "c653135554592aaaebae29ce2845bd6cd58aace6",
		Remote:    "git@github.com:acme/web.git",
		Tag:       "v1.2.3",
	}, time.Unix(1755000000, 0), "tester")

	labels := imageLabels(cfg, rel)

	if labels[labelRevision] != rel.Git.SHA {
		t.Errorf("revision = %q, want the full sha", labels[labelRevision])
	}
	if labels[labelSource] != "https://github.com/acme/web" {
		t.Errorf("source = %q", labels[labelSource])
	}
	// A git tag is a more meaningful version than a release id.
	if labels[labelVersion] != "v1.2.3" {
		t.Errorf("version = %q, want v1.2.3", labels[labelVersion])
	}
	if labels[labelTitle] != "web" {
		t.Errorf("title = %q", labels[labelTitle])
	}
	if labels[labelBuidlRel] != rel.ID {
		t.Errorf("release label = %q", labels[labelBuidlRel])
	}
}

func TestImageLabelsMarkDirtyRevision(t *testing.T) {
	cfg := &config.Config{App: "web"}
	rel := release.New("staging", gitinfo.Info{
		Available: true,
		SHA:       "c653135554592aaaebae29ce2845bd6cd58aace6",
		Dirty:     true,
	}, time.Unix(1755000000, 0), "tester")

	labels := imageLabels(cfg, rel)

	// Without this suffix, a reader would believe the image is reproducible from
	// that commit.
	if !strings.HasSuffix(labels[labelRevision], "-dirty") {
		t.Errorf("revision = %q, want a -dirty suffix", labels[labelRevision])
	}
}

func TestImageLabelsFallBackToReleaseID(t *testing.T) {
	cfg := &config.Config{App: "web"}
	rel := release.New("staging", gitinfo.Info{}, time.Unix(1755000000, 0), "tester")

	labels := imageLabels(cfg, rel)

	if labels[labelVersion] != rel.ID {
		t.Errorf("version = %q, want the release id when no git tag exists", labels[labelVersion])
	}
	// No repository means no revision or source to record.
	if _, ok := labels[labelRevision]; ok {
		t.Error("revision must be absent outside a repository")
	}
}

func TestForSelectsDriver(t *testing.T) {
	buildkit, err := For(&config.Config{Build: config.Build{Driver: config.DriverBuildKit}}, testLogger{})
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	if buildkit.Name() != "buildkit" {
		t.Errorf("Name = %q, want buildkit", buildkit.Name())
	}

	prebuilt, err := For(&config.Config{Build: config.Build{Driver: config.DriverPrebuilt}}, testLogger{})
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	if prebuilt.Name() != "prebuilt" {
		t.Errorf("Name = %q, want prebuilt", prebuilt.Name())
	}

	if _, err := For(&config.Config{Build: config.Build{Driver: "magic"}}, testLogger{}); err == nil {
		t.Error("expected an error for an unknown driver")
	}
}

func TestResolveRejectsInvalidReference(t *testing.T) {
	if _, err := Resolve(t.Context(), "NOT A VALID REF!!"); err == nil {
		t.Error("expected an error for an invalid reference")
	}
}

func TestResolvePassesThroughDigest(t *testing.T) {
	// A digest reference needs no network round trip.
	digest := "sha256:" + strings.Repeat("a", 64)
	got, err := Resolve(t.Context(), "ghcr.io/acme/web@"+digest)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != digest {
		t.Errorf("Resolve = %q, want %q", got, digest)
	}
}

func TestBuildKitHintIsActionable(t *testing.T) {
	// "no buildkit" is the most likely first-run failure, so the message must
	// contain a command the user can run.
	hint := buildkitHint()
	for _, want := range []string{"BUILDKIT_HOST", "docker run", "rootless"} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint should mention %q:\n%s", want, hint)
		}
	}
}

// testLogger discards output.
type testLogger struct{}

func (testLogger) Info(string, ...any)    {}
func (testLogger) Detail(string, ...any)  {}
func (testLogger) Warn(string, ...any)    {}
func (testLogger) Success(string, ...any) {}
