package release

import (
	"strings"
	"testing"
	"time"

	"github.com/danecwalker/buidl/internal/gitinfo"
)

var (
	testTime  = time.Unix(1755000000, 0).UTC()
	cleanGit  = gitinfo.Info{Available: true, SHA: "c653135554592aaaebae29ce2845bd6cd58aace6", Branch: "main"}
	dirtyGit  = gitinfo.Info{Available: true, SHA: "c653135554592aaaebae29ce2845bd6cd58aace6", Branch: "main", Dirty: true}
	absentGit = gitinfo.Info{}
)

func TestIDFromCleanCommit(t *testing.T) {
	id := ID(cleanGit, testTime)

	if !strings.HasPrefix(id, "c653135-") {
		t.Errorf("ID = %q, want it to start with the short sha", id)
	}
	if strings.Contains(id, "dirty") {
		t.Errorf("ID = %q, must not be marked dirty", id)
	}
}

func TestIDMarksDirtyTree(t *testing.T) {
	id := ID(dirtyGit, testTime)
	// A dirty build must be visibly distinct, or someone will believe it is
	// reproducible from that commit.
	if !strings.Contains(id, "dirty") {
		t.Errorf("ID = %q, want it marked dirty", id)
	}
}

func TestIDWithoutGit(t *testing.T) {
	id := ID(absentGit, testTime)
	if !strings.HasPrefix(id, "dev-") {
		t.Errorf("ID = %q, want a dev- prefix outside a repo", id)
	}
}

func TestIDIsUniquePerDeployOfSameCommit(t *testing.T) {
	// Redeploying the same commit must still yield a distinct release, both for
	// blue-green object naming and for an honest deploy history.
	first := ID(cleanGit, testTime)
	second := ID(cleanGit, testTime.Add(time.Second))
	if first == second {
		t.Errorf("two deploys of the same commit produced the same id: %q", first)
	}
}

func TestIDIsLexicographicallyOrdered(t *testing.T) {
	// Sortable ids make release listings readable without parsing timestamps.
	earlier := ID(cleanGit, testTime)
	later := ID(cleanGit, testTime.Add(24*time.Hour))
	if earlier >= later {
		t.Errorf("expected %q < %q", earlier, later)
	}
}

func TestIDIsDNSLabelSafe(t *testing.T) {
	for _, git := range []gitinfo.Info{cleanGit, dirtyGit, absentGit} {
		id := ID(git, testTime)
		if len(id) > 63 {
			t.Errorf("ID %q is too long for a DNS label", id)
		}
		for _, r := range id {
			valid := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-'
			if !valid {
				t.Errorf("ID %q contains an invalid character %q", id, r)
			}
		}
		if strings.HasPrefix(id, "-") || strings.HasSuffix(id, "-") {
			t.Errorf("ID %q must not start or end with a dash", id)
		}
	}
}

func TestRefPrefersDigest(t *testing.T) {
	rel := New("production", cleanGit, testTime, "tester")
	rel.Repo = "ghcr.io/acme/web"
	rel.Tag = rel.ID

	// Without a digest, a tag reference is the fallback.
	if got := rel.Ref(); got != "ghcr.io/acme/web:"+rel.ID {
		t.Errorf("Ref = %q, want the tag reference", got)
	}
	if rel.Pinned() {
		t.Error("a release without a digest must not report as pinned")
	}

	rel.Digest = "sha256:" + strings.Repeat("a", 64)
	if got := rel.Ref(); got != "ghcr.io/acme/web@"+rel.Digest {
		t.Errorf("Ref = %q, want the digest reference", got)
	}
	if !rel.Pinned() {
		t.Error("a digest release must report as pinned")
	}
}

func TestPinnedRejectsNonDigest(t *testing.T) {
	rel := Release{Digest: "latest"}
	if rel.Pinned() {
		t.Error("a non-sha256 digest must not count as pinned")
	}
}

func TestLabelsAndAnnotations(t *testing.T) {
	rel := New("production", dirtyGit, testTime, "tester")
	rel.Repo = "ghcr.io/acme/web"
	rel.Digest = "sha256:" + strings.Repeat("b", 64)

	labels := rel.Labels("web")
	if labels[LabelName] != "web" {
		t.Errorf("%s = %q", LabelName, labels[LabelName])
	}
	if labels[LabelInstance] != "web-production" {
		t.Errorf("%s = %q", LabelInstance, labels[LabelInstance])
	}
	if labels[LabelManagedBy] != ManagedBy {
		t.Errorf("%s = %q", LabelManagedBy, labels[LabelManagedBy])
	}

	ann := rel.Annotations()
	if ann[AnnotationRelease] != rel.ID {
		t.Errorf("release annotation = %q", ann[AnnotationRelease])
	}
	if ann[AnnotationDigest] != rel.Digest {
		t.Errorf("digest annotation = %q", ann[AnnotationDigest])
	}
	if ann[AnnotationGitDirty] != "true" {
		t.Error("a dirty build must be recorded in annotations")
	}
	if ann[AnnotationActor] != "tester" {
		t.Errorf("actor = %q", ann[AnnotationActor])
	}
}

func TestVersionLabelPrefersGitTag(t *testing.T) {
	git := cleanGit
	git.Tag = "v1.2.3"
	rel := New("production", git, testTime, "tester")

	if got := rel.Labels("web")[LabelVersion]; got != "v1.2.3" {
		t.Errorf("version label = %q, want the git tag", got)
	}
}

func TestLabelValuesAreSanitized(t *testing.T) {
	git := cleanGit
	// Slashes are legal in git tags but not in Kubernetes label values.
	git.Tag = "release/2026-08-13+build.1"
	rel := New("production", git, testTime, "tester")

	value := rel.Labels("web")[LabelVersion]
	if strings.ContainsAny(value, "/+") {
		t.Errorf("label value %q contains characters Kubernetes rejects", value)
	}
	if len(value) > 63 {
		t.Errorf("label value %q exceeds 63 characters", value)
	}
}

func TestShortDigest(t *testing.T) {
	rel := Release{Digest: "sha256:abcdef0123456789abcdef"}
	if got := rel.ShortDigest(); got != "sha256:abcdef012345" {
		t.Errorf("ShortDigest = %q", got)
	}
	if got := (Release{}).ShortDigest(); got != "-" {
		t.Errorf("empty ShortDigest = %q, want -", got)
	}
}

func TestObjectName(t *testing.T) {
	if got := ObjectName("web"); got != "web" {
		t.Errorf("ObjectName = %q", got)
	}
	if got := ObjectName("web", "env"); got != "web-env" {
		t.Errorf("ObjectName = %q", got)
	}
	// Empty parts are skipped rather than producing a trailing dash.
	if got := ObjectName("web", "", "tls"); got != "web-tls" {
		t.Errorf("ObjectName = %q", got)
	}

	long := ObjectName(strings.Repeat("a", 70))
	if len(long) > 63 {
		t.Errorf("length = %d, want <= 63", len(long))
	}
}
