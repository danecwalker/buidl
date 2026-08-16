package build

import (
	"os"
	"strings"
	"testing"
	"time"

	bkclient "github.com/moby/buildkit/client"

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

func TestSolveOptLocalImageExportsDockerArchive(t *testing.T) {
	cfg := &config.Config{
		App:         "web",
		Image:       "buidl.local/web",
		Environment: "default",
		Build:       config.Build{Cache: "none"},
	}
	b := NewBuildKit(cfg, testLogger{})
	archive, err := os.CreateTemp(t.TempDir(), "img-*.tar")
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()

	rel := release.New("default", gitinfo.Info{}, time.Unix(1755000000, 0), "tester")
	rel.Repo = cfg.Image
	rel.Tag = rel.ID

	opt, err := b.solveOpt(Request{Config: cfg, Release: rel}, ".", "Dockerfile", []string{"linux/amd64"}, archive)
	if err != nil {
		t.Fatalf("solveOpt: %v", err)
	}
	if len(opt.Exports) != 1 {
		t.Fatalf("exports = %d, want 1", len(opt.Exports))
	}
	if opt.Exports[0].Type != bkclient.ExporterDocker {
		t.Errorf("export type = %q, want docker", opt.Exports[0].Type)
	}
	if opt.Exports[0].Output == nil {
		t.Error("local export must write an archive")
	}
	if len(opt.CacheExports) != 0 || len(opt.CacheImports) != 0 {
		t.Error("local image must not use registry cache")
	}
}

func TestSolveOptRegistryImagePushes(t *testing.T) {
	cfg := &config.Config{
		App:         "web",
		Image:       "ghcr.io/acme/web",
		Environment: "default",
		Build:       config.Build{Cache: "none"},
	}
	b := NewBuildKit(cfg, testLogger{})
	rel := release.New("default", gitinfo.Info{}, time.Unix(1755000000, 0), "tester")
	rel.Repo = cfg.Image
	rel.Tag = rel.ID

	opt, err := b.solveOpt(Request{Config: cfg, Release: rel, Push: true}, ".", "Dockerfile", []string{"linux/amd64"}, nil)
	if err != nil {
		t.Fatalf("solveOpt: %v", err)
	}
	if opt.Exports[0].Type != bkclient.ExporterImage {
		t.Errorf("export type = %q, want image", opt.Exports[0].Type)
	}
	if opt.Exports[0].Attrs["push"] != "true" {
		t.Errorf("push = %q, want true", opt.Exports[0].Attrs["push"])
	}
}

func TestPrebuiltRejectsLocalImage(t *testing.T) {
	p := NewPrebuilt(&config.Config{Image: "buidl.local/web"}, testLogger{})
	_, err := p.Build(t.Context(), Request{
		Config:  &config.Config{Image: "buidl.local/web"},
		Release: release.New("default", gitinfo.Info{}, time.Unix(1755000000, 0), "tester"),
	})
	if err == nil {
		t.Fatal("expected prebuilt to reject a local image")
	}
	if !strings.Contains(err.Error(), "local image") {
		t.Errorf("error = %v", err)
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
	for _, want := range []string{"BUILDKIT_HOST", "buildkitd", defaultBuilderImage, "rootless"} {
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
