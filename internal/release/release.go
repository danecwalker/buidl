// Package release defines the identity of a single immutable deployment.
//
// The central idea, borrowed from Vercel: a release is immutable and addressed
// by content, not by a moving tag. Every deploy produces a new release ID, the
// image is pinned by digest, and promoting between environments re-points an
// alias at an existing digest rather than rebuilding. That is what makes
// "staging is exactly what will be in production" true instead of aspirational.
package release

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/danewalker/buidl/internal/gitinfo"
)

// Label and annotation keys written onto every object buidl manages. The
// "app.kubernetes.io" keys are the standard recommended set, so third-party
// tooling (dashboards, service meshes) understands our objects for free.
const (
	LabelName      = "app.kubernetes.io/name"
	LabelInstance  = "app.kubernetes.io/instance"
	LabelManagedBy = "app.kubernetes.io/managed-by"
	LabelVersion   = "app.kubernetes.io/version"
	LabelComponent = "app.kubernetes.io/component"

	// LabelRelease is the selector-relevant release ID. Blue-green flips the
	// Service selector across values of this label.
	LabelRelease = "buidl.dev/release"
	LabelEnv     = "buidl.dev/environment"

	AnnotationRelease   = "buidl.dev/release"
	AnnotationDigest    = "buidl.dev/image-digest"
	AnnotationGitSHA    = "buidl.dev/git-sha"
	AnnotationGitBranch = "buidl.dev/git-branch"
	AnnotationGitDirty  = "buidl.dev/git-dirty"
	AnnotationActor     = "buidl.dev/deployed-by"
	AnnotationTime      = "buidl.dev/deployed-at"
	AnnotationConfigSum = "buidl.dev/config-checksum"

	// ManagedBy marks objects as ours so `buidl` can prune what it created
	// without touching hand-written manifests in the same namespace.
	ManagedBy = "buidl"
)

// Release identifies one deployable artifact plus its provenance.
type Release struct {
	// ID is unique, DNS-label-safe, and roughly time-ordered.
	ID string
	// Environment this release was built for.
	Environment string
	// Repo is the image repository without tag or digest.
	Repo string
	// Tag is the mutable tag pushed alongside the digest, for human browsing of
	// the registry. Equal to ID.
	Tag string
	// Digest is the resolved image digest ("sha256:..."). Empty until a build or
	// a registry resolution has happened.
	Digest string

	Git       gitinfo.Info
	Actor     string
	CreatedAt time.Time
}

// New mints a release ID from git provenance and a timestamp.
//
// The ID is <short-sha>-<time-token> so that redeploying the same commit still
// produces a distinct, orderable release — necessary for blue-green object
// naming and for an honest deploy history.
func New(env string, git gitinfo.Info, now time.Time, actor string) Release {
	return Release{
		ID:          ID(git, now),
		Environment: env,
		Git:         git,
		Actor:       actor,
		CreatedAt:   now.UTC(),
	}
}

// ID builds the release identifier.
func ID(git gitinfo.Info, now time.Time) string {
	token := timeToken(now)
	switch {
	case !git.Available || git.SHA == "":
		// Not a git repo: the timestamp is all the identity we have.
		return "dev-" + token
	case git.Dirty:
		// Mark uncommitted builds so nobody mistakes them for a real commit.
		return short(git.SHA) + "-dirty-" + token
	default:
		return short(git.SHA) + "-" + token
	}
}

// timeToken renders the time as a compact, lexicographically sortable,
// lowercase base-36 second count.
func timeToken(now time.Time) string {
	return strconv.FormatInt(now.UTC().Unix(), 36)
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// Ref returns the fully pinned image reference used by manifests. Pinning by
// digest means a pod restart can never silently pick up different bytes.
func (r Release) Ref() string {
	if r.Digest != "" {
		return r.Repo + "@" + r.Digest
	}
	if r.Tag != "" {
		return r.Repo + ":" + r.Tag
	}
	return r.Repo
}

// TagRef returns the human-browsable tagged reference.
func (r Release) TagRef() string {
	return r.Repo + ":" + r.Tag
}

// Pinned reports whether this release is safe to deploy: only a digest-pinned
// release is immutable.
func (r Release) Pinned() bool {
	return strings.HasPrefix(r.Digest, "sha256:")
}

// ShortDigest renders the digest for display, e.g. "sha256:a1b2c3d4".
func (r Release) ShortDigest() string {
	if r.Digest == "" {
		return "-"
	}
	hex := strings.TrimPrefix(r.Digest, "sha256:")
	if len(hex) > 12 {
		hex = hex[:12]
	}
	return "sha256:" + hex
}

// Labels returns the selector-safe label set for this release.
func (r Release) Labels(app string) map[string]string {
	l := map[string]string{
		LabelName:      app,
		LabelInstance:  app + "-" + r.Environment,
		LabelManagedBy: ManagedBy,
		LabelEnv:       r.Environment,
	}
	if v := r.version(); v != "" {
		l[LabelVersion] = v
	}
	return l
}

// version prefers a git tag (a real version) over a commit sha.
func (r Release) version() string {
	if r.Git.Tag != "" {
		return sanitizeLabelValue(r.Git.Tag)
	}
	if r.Git.SHA != "" {
		return short(r.Git.SHA)
	}
	return ""
}

// Annotations returns the provenance recorded on every managed object.
func (r Release) Annotations() map[string]string {
	a := map[string]string{
		AnnotationRelease: r.ID,
		AnnotationTime:    r.CreatedAt.UTC().Format(time.RFC3339),
	}
	if r.Digest != "" {
		a[AnnotationDigest] = r.Digest
	}
	if r.Actor != "" {
		a[AnnotationActor] = r.Actor
	}
	if r.Git.Available {
		a[AnnotationGitSHA] = r.Git.SHA
		if r.Git.Branch != "" {
			a[AnnotationGitBranch] = r.Git.Branch
		}
		if r.Git.Dirty {
			a[AnnotationGitDirty] = "true"
		}
	}
	return a
}

// sanitizeLabelValue coerces a string into a valid Kubernetes label value:
// alphanumerics, '-', '_', '.', at most 63 chars, must start and end
// alphanumeric.
func sanitizeLabelValue(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-_.")
	if len(out) > 63 {
		out = strings.Trim(out[:63], "-_.")
	}
	return out
}

// ObjectName composes a Kubernetes object name from an app name and optional
// suffixes, keeping it within the 63-character limit.
func ObjectName(app string, parts ...string) string {
	name := app
	for _, p := range parts {
		if p != "" {
			name += "-" + p
		}
	}
	if len(name) > 63 {
		name = strings.Trim(name[:63], "-")
	}
	return name
}

// String renders a one-line summary for logs.
func (r Release) String() string {
	return fmt.Sprintf("%s (%s) -> %s", r.ID, r.Environment, r.ShortDigest())
}
