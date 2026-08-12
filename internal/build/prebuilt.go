package build

import (
	"context"
	"fmt"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	buidlconfig "github.com/danewalker/buidl/internal/config"
)

// Prebuilt does not build anything. It resolves an existing registry reference
// to a digest.
//
// This driver is what makes promotion honest. `buidl promote --from staging --to
// production` resolves the digest already running in staging and deploys that
// exact digest — no rebuild, so there is no possibility of the production
// artifact differing from the one that was tested.
//
// It is also the right driver for pipelines that build in a separate CI job:
// that job runs `buidl build`, and the deploy job runs with driver=prebuilt.
type Prebuilt struct {
	cfg *buidlconfig.Config
	log Logger
}

// NewPrebuilt constructs the prebuilt resolver.
func NewPrebuilt(cfg *buidlconfig.Config, log Logger) *Prebuilt {
	return &Prebuilt{cfg: cfg, log: log}
}

// Name implements Builder.
func (p *Prebuilt) Name() string { return "prebuilt" }

// Available implements Builder. There is nothing to check ahead of time; the
// registry lookup in Build reports any problem with a better message.
func (p *Prebuilt) Available(context.Context) error { return nil }

// Close implements Builder.
func (p *Prebuilt) Close() error { return nil }

// Build resolves the release's reference to a digest.
func (p *Prebuilt) Build(ctx context.Context, req Request) (Result, error) {
	start := time.Now()

	ref := req.Release.Ref()
	digest, err := Resolve(ctx, ref)
	if err != nil {
		return Result{}, err
	}

	p.log.Detail("resolved %s to %s", ref, digest)

	return Result{
		Digest:   digest,
		Ref:      req.Config.Image + "@" + digest,
		Tag:      req.Release.Tag,
		Duration: time.Since(start),
		// Nothing was pushed, but the image is in the registry, which is what
		// callers actually need to know before deploying.
		Pushed: true,
	}, nil
}

// Resolve looks up the manifest digest for an image reference.
//
// For a multi-arch image this returns the digest of the manifest index, which is
// the correct thing to deploy: the kubelet on each node selects the matching
// platform manifest from it.
func Resolve(ctx context.Context, ref string) (string, error) {
	parsed, err := name.ParseReference(ref)
	if err != nil {
		return "", fmt.Errorf("invalid image reference %q: %w", ref, err)
	}

	// If the caller already handed us a digest reference, trust it rather than
	// spending a network round trip.
	if d, ok := parsed.(name.Digest); ok {
		return d.DigestStr(), nil
	}

	desc, err := remote.Get(parsed,
		remote.WithContext(ctx),
		// Reuse the standard Docker keychain so existing registry logins work.
		remote.WithAuthFromKeychain(authn.DefaultKeychain),
	)
	if err != nil {
		return "", fmt.Errorf("resolving %s in the registry: %w\n\nhint: has the image been pushed? check `buidl releases`", ref, err)
	}
	return desc.Digest.String(), nil
}

// Exists reports whether a reference is present in the registry. Used by deploy
// preflight so a missing image fails before any cluster change is made.
func Exists(ctx context.Context, ref string) (bool, error) {
	if _, err := Resolve(ctx, ref); err != nil {
		return false, err
	}
	return true, nil
}
