// Package build produces container images and resolves them to immutable
// digests.
//
// Deliberately, no Docker daemon is involved. BuildKit is used directly over its
// gRPC API, which means:
//
//   - No 200MB+ daemon to install on a CI runner or a developer laptop.
//   - Builds can run rootless, or be delegated to a buildkitd Pod inside the
//     target cluster, so CI needs no privileged container.
//   - The image is exported straight to the registry. It never lands in a local
//     image store, so there is no `docker push` step to fail separately.
package build

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/danewalker/buidl/internal/config"
	"github.com/danewalker/buidl/internal/release"
)

// Request describes one build.
type Request struct {
	// Root is the directory containing buidl.yaml. Relative paths resolve here.
	Root string
	// Config is the resolved configuration for the target environment.
	Config *config.Config
	// Release supplies the tag and provenance labels to stamp onto the image.
	Release release.Release
	// Push exports the image to the registry. A deploy always requires this,
	// since the cluster pulls the image; `buidl build --no-push` exists only for
	// validating that a build succeeds.
	Push bool
	// NoCache ignores all cache sources.
	NoCache bool
	// Platforms overrides Config.Build.Platforms when non-empty.
	Platforms []string
	// Plain forces line-oriented build progress rather than an interactive
	// redrawing display.
	Plain bool
}

// Result is the outcome of a successful build.
type Result struct {
	// Digest is the manifest digest ("sha256:..."). This is the release's true
	// identity; everything downstream pins to it.
	Digest string
	// Ref is the fully pinned reference, repo@digest.
	Ref string
	// Tag is the mutable tag also pushed, for registry browsing.
	Tag       string
	Platforms []string
	Duration  time.Duration
	// Pushed reports whether the image reached the registry.
	Pushed bool
}

// Builder produces an image for a Request.
type Builder interface {
	// Name identifies the builder in logs.
	Name() string
	// Available reports whether this builder can run, with an actionable error
	// if not. Callers check this before doing expensive work so failures arrive
	// early with a fix attached.
	Available(ctx context.Context) error
	// Build produces and (if requested) pushes the image.
	Build(ctx context.Context, req Request) (Result, error)
	// Close releases any connections.
	Close() error
}

// For selects a builder for the configured driver.
func For(cfg *config.Config, log Logger) (Builder, error) {
	switch cfg.Build.Driver {
	case config.DriverBuildKit:
		return NewBuildKit(cfg, log), nil
	case config.DriverPrebuilt:
		return NewPrebuilt(cfg, log), nil
	default:
		return nil, fmt.Errorf("unknown build driver %q", cfg.Build.Driver)
	}
}

// Logger is the subset of ui.Printer that this package needs. Depending on an
// interface rather than the concrete printer keeps build testable without a
// terminal.
type Logger interface {
	Info(format string, args ...any)
	Detail(format string, args ...any)
	Warn(format string, args ...any)
	Success(format string, args ...any)
}

// OCI label keys stamped onto every built image. These are the standard
// annotation keys, so `docker inspect` and registry UIs display them without
// buidl-specific tooling.
const (
	labelRevision = "org.opencontainers.image.revision"
	labelSource   = "org.opencontainers.image.source"
	labelCreated  = "org.opencontainers.image.created"
	labelVersion  = "org.opencontainers.image.version"
	labelTitle    = "org.opencontainers.image.title"
	labelBuidlRel = "dev.buidl.release"
	labelBuidlEnv = "dev.buidl.environment"
)

// imageLabels builds the OCI label set for a release.
func imageLabels(cfg *config.Config, rel release.Release) map[string]string {
	labels := map[string]string{
		labelTitle:    cfg.App,
		labelCreated:  rel.CreatedAt.UTC().Format(time.RFC3339),
		labelBuidlRel: rel.ID,
		labelBuidlEnv: rel.Environment,
	}
	if rel.Git.SHA != "" {
		revision := rel.Git.SHA
		if rel.Git.Dirty {
			// Marking the revision dirty prevents a later reader from believing
			// this image is reproducible from that commit.
			revision += "-dirty"
		}
		labels[labelRevision] = revision
	}
	if rel.Git.Remote != "" {
		labels[labelSource] = normalizeRemote(rel.Git.Remote)
	}
	version := rel.Git.Tag
	if version == "" {
		version = rel.ID
	}
	labels[labelVersion] = version
	return labels
}

// normalizeRemote converts an SSH git remote into an https URL, which is what
// the OCI source annotation is expected to contain.
func normalizeRemote(remote string) string {
	if after, ok := strings.CutPrefix(remote, "git@"); ok {
		// git@github.com:acme/web.git -> https://github.com/acme/web
		host, path, found := strings.Cut(after, ":")
		if found {
			return "https://" + host + "/" + strings.TrimSuffix(path, ".git")
		}
	}
	return strings.TrimSuffix(remote, ".git")
}
