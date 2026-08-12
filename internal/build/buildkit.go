package build

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/docker/cli/cli/config"
	bkclient "github.com/moby/buildkit/client"

	// BuildKit's address schemes are pluggable: each connection helper registers
	// itself from its own package's init. Without these imports the client can
	// only dial unix:// and tcp://, so every address form buidl documents —
	// docker-container:// for a local builder, kube-pod:// for the in-cluster
	// buildkit addon — would fail at dial time with an opaque error.
	_ "github.com/moby/buildkit/client/connhelper/dockercontainer"
	_ "github.com/moby/buildkit/client/connhelper/kubepod"
	_ "github.com/moby/buildkit/client/connhelper/nerdctlcontainer"
	_ "github.com/moby/buildkit/client/connhelper/podmancontainer"
	_ "github.com/moby/buildkit/client/connhelper/ssh"

	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/auth/authprovider"
	"github.com/moby/buildkit/session/secrets/secretsprovider"
	"github.com/moby/buildkit/util/progress/progressui"
	"golang.org/x/sync/errgroup"

	buidlconfig "github.com/danewalker/buidl/internal/config"
)

// exporterDigestKey is where BuildKit reports the pushed manifest digest.
const exporterDigestKey = "containerimage.digest"

// BuildKit builds images by talking to a buildkitd over gRPC.
type BuildKit struct {
	cfg *buidlconfig.Config
	log Logger

	mu     sync.Mutex
	client *bkclient.Client
	addr   string
}

// NewBuildKit constructs a BuildKit builder. No connection is made until
// Available or Build is called.
func NewBuildKit(cfg *buidlconfig.Config, log Logger) *BuildKit {
	return &BuildKit{cfg: cfg, log: log}
}

// Name implements Builder.
func (b *BuildKit) Name() string { return "buildkit" }

// Available connects to a buildkitd and verifies it responds.
func (b *BuildKit) Available(ctx context.Context) error {
	c, err := b.connect(ctx)
	if err != nil {
		return err
	}
	// ListWorkers is the cheapest round trip that proves the daemon is healthy
	// and tells us which platforms it can build for.
	workers, err := c.ListWorkers(ctx)
	if err != nil {
		return fmt.Errorf("buildkit at %s is not responding: %w", b.addr, err)
	}
	if len(workers) == 0 {
		return fmt.Errorf("buildkit at %s has no workers", b.addr)
	}
	b.log.Detail("buildkit %s: %d worker(s)", b.addr, len(workers))
	return nil
}

// connect dials buildkitd, discovering an endpoint if none is configured.
func (b *BuildKit) connect(ctx context.Context) (*bkclient.Client, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.client != nil {
		return b.client, nil
	}

	addr, err := b.resolveAddr()
	if err != nil {
		return nil, err
	}
	c, err := bkclient.New(ctx, addr)
	if err != nil {
		return nil, fmt.Errorf("connecting to buildkit at %s: %w\n\n%s", addr, err, buildkitHint())
	}
	b.client = c
	b.addr = addr
	return c, nil
}

// resolveAddr finds a buildkitd endpoint, preferring explicit configuration.
//
// The search order exists so the common cases need no configuration: a
// developer with rootless buildkitd running, and a CI job that sets
// BUILDKIT_HOST (as the setup-buildx action does).
func (b *BuildKit) resolveAddr() (string, error) {
	if b.cfg.Build.Addr != "" {
		return b.cfg.Build.Addr, nil
	}
	if env := strings.TrimSpace(os.Getenv("BUILDKIT_HOST")); env != "" {
		return env, nil
	}

	// Rootless buildkitd, per the upstream default.
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		sock := filepath.Join(dir, "buildkit", "buildkitd.sock")
		if _, err := os.Stat(sock); err == nil {
			return "unix://" + sock, nil
		}
	}
	// System buildkitd.
	if _, err := os.Stat("/run/buildkit/buildkitd.sock"); err == nil {
		return "unix:///run/buildkit/buildkitd.sock", nil
	}

	return "", errors.New("no buildkit endpoint found\n\n" + buildkitHint())
}

// buildkitHint tells the user how to get a builder, since "no buildkit" is the
// single most likely first-run failure.
func buildkitHint() string {
	return `buidl builds without a Docker daemon, so it needs a BuildKit endpoint. Pick one:

  local (docker available):
    docker run -d --name buildkitd --privileged moby/buildkit:latest
    export BUILDKIT_HOST=docker-container://buildkitd

  rootless (no docker, no root):
    https://github.com/moby/buildkit/blob/master/docs/rootless.md

  in-cluster (recommended for CI — no privileged runner needed):
    enable infra.addons.buildkit, then:
    build:
      addr: kube-pod://buildkitd?namespace=buidl-system

  Supported address schemes: unix://, tcp://, docker-container://,
  kube-pod://, podman-container://, nerdctl-container://, ssh://`
}

// Close releases the connection.
func (b *BuildKit) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.client == nil {
		return nil
	}
	err := b.client.Close()
	b.client = nil
	return err
}

// Build performs the solve and returns the resolved digest.
func (b *BuildKit) Build(ctx context.Context, req Request) (Result, error) {
	start := time.Now()

	c, err := b.connect(ctx)
	if err != nil {
		return Result{}, err
	}

	contextDir := filepath.Join(req.Root, req.Config.Build.Context)
	dockerfilePath := filepath.Join(contextDir, req.Config.Build.Dockerfile)
	if _, err := os.Stat(dockerfilePath); err != nil {
		return Result{}, fmt.Errorf("dockerfile %s not found: %w\n\nhint: run `buidl init` to generate one", dockerfilePath, err)
	}

	platforms := req.Platforms
	if len(platforms) == 0 {
		platforms = req.Config.Build.Platforms
	}

	solveOpt, err := b.solveOpt(req, contextDir, dockerfilePath, platforms)
	if err != nil {
		return Result{}, err
	}

	// BuildKit streams status on a channel that must be drained concurrently
	// with the solve, or the solve blocks. Both run in an errgroup so a failure
	// in either cancels the other.
	ch := make(chan *bkclient.SolveStatus)
	eg, ctx := errgroup.WithContext(ctx)

	var resp *bkclient.SolveResponse
	eg.Go(func() error {
		var err error
		resp, err = c.Solve(ctx, nil, *solveOpt, ch)
		if err != nil {
			return fmt.Errorf("build failed: %w", err)
		}
		return nil
	})

	eg.Go(func() error {
		mode := progressui.TtyMode
		if req.Plain {
			mode = progressui.PlainMode
		}
		// Build logs belong on stderr so that `buidl build --output json` can
		// keep stdout a clean machine-readable stream.
		display, err := progressui.NewDisplay(os.Stderr, mode)
		if err != nil {
			// A display that cannot initialize (no TTY) must not fail the build.
			display, err = progressui.NewDisplay(os.Stderr, progressui.PlainMode)
			if err != nil {
				return err
			}
		}
		_, err = display.UpdateFrom(ctx, ch)
		return err
	})

	if err := eg.Wait(); err != nil {
		return Result{}, err
	}

	digest := resp.ExporterResponse[exporterDigestKey]
	if digest == "" && req.Push {
		return Result{}, errors.New("build succeeded but the registry returned no image digest; cannot pin this release")
	}

	return Result{
		Digest:    digest,
		Ref:       req.Config.Image + "@" + digest,
		Tag:       req.Release.Tag,
		Platforms: platforms,
		Duration:  time.Since(start),
		Pushed:    req.Push,
	}, nil
}

// solveOpt translates buidl config into BuildKit's solve request.
func (b *BuildKit) solveOpt(req Request, contextDir, dockerfilePath string, platforms []string) (*bkclient.SolveOpt, error) {
	cfg := req.Config

	frontendAttrs := map[string]string{
		"filename": filepath.Base(dockerfilePath),
		"platform": strings.Join(platforms, ","),
	}
	if cfg.Build.Target != "" {
		frontendAttrs["target"] = cfg.Build.Target
	}
	if req.NoCache {
		frontendAttrs["no-cache"] = ""
	}
	for k, v := range cfg.Build.Args {
		frontendAttrs["build-arg:"+k] = v
	}
	// Stamp OCI provenance labels into the image config.
	for k, v := range imageLabels(cfg, req.Release) {
		frontendAttrs["label:"+k] = v
	}

	// Push both the immutable digest and a readable tag. The tag is a
	// convenience; nothing in buidl resolves it after the build.
	exportAttrs := map[string]string{
		"name": strings.Join([]string{req.Release.TagRef(), cfg.Image + ":" + cfg.Environment}, ","),
		"push": boolStr(req.Push),
		// Preserve both platforms' manifests under one index for multi-arch.
		"oci-mediatypes": "true",
	}
	if len(platforms) > 1 {
		exportAttrs["annotation-index.org.opencontainers.image.created"] = req.Release.CreatedAt.UTC().Format(time.RFC3339)
	}

	attachable, err := b.sessionAttachables(req, contextDir)
	if err != nil {
		return nil, err
	}

	opt := &bkclient.SolveOpt{
		Frontend:      "dockerfile.v0",
		FrontendAttrs: frontendAttrs,
		LocalDirs: map[string]string{
			"context":    contextDir,
			"dockerfile": filepath.Dir(dockerfilePath),
		},
		Exports: []bkclient.ExportEntry{{
			Type:  bkclient.ExporterImage,
			Attrs: exportAttrs,
		}},
		Session: attachable,
	}

	if !req.NoCache {
		opt.CacheImports, opt.CacheExports = b.cacheOpts(cfg)
	}

	return opt, nil
}

// cacheOpts configures the layer cache.
//
// Registry-backed cache is the default because it is the only kind that
// survives an ephemeral CI runner: the cache lives next to the image, so a
// fresh runner gets warm-cache build times.
func (b *BuildKit) cacheOpts(cfg *buidlconfig.Config) (imports, exports []bkclient.CacheOptionsEntry) {
	switch cfg.Build.Cache {
	case "none":
		return nil, nil

	case "inline":
		// Inline cache ships cache metadata inside the image itself. Zero extra
		// storage, but it can only cache the final stage.
		return []bkclient.CacheOptionsEntry{{
				Type:  "registry",
				Attrs: map[string]string{"ref": cfg.Image + ":" + cfg.Environment},
			}},
			[]bkclient.CacheOptionsEntry{{Type: "inline"}}

	default: // "registry"
		ref := cfg.Build.CacheRef
		return []bkclient.CacheOptionsEntry{{
				Type: "registry",
				Attrs: map[string]string{
					"ref": ref,
					// Tolerate a missing cache tag on the very first build.
					"ignore-error": "true",
				},
			}},
			[]bkclient.CacheOptionsEntry{{
				Type: "registry",
				Attrs: map[string]string{
					"ref": ref,
					// mode=max caches intermediate stages too, which is what
					// makes multi-stage builds actually fast on a cold runner.
					"mode":         "max",
					"ignore-error": "true",
				},
			}}
	}
}

// sessionAttachables wires up registry auth and build secrets.
func (b *BuildKit) sessionAttachables(req Request, contextDir string) ([]session.Attachable, error) {
	var attachable []session.Attachable

	// Registry credentials come from the standard Docker config, so `docker
	// login`, `gcloud auth configure-docker` and the docker/login-action all
	// work with no buidl-specific setup.
	dockerConfig := config.LoadDefaultConfigFile(os.Stderr)
	attachable = append(attachable, authprovider.NewDockerAuthProvider(authprovider.DockerAuthProviderConfig{
		ConfigFile: dockerConfig,
	}))

	if len(req.Config.Build.Secrets) > 0 {
		sources := make([]secretsprovider.Source, 0, len(req.Config.Build.Secrets))
		for id, path := range req.Config.Build.Secrets {
			// A bare "env:NAME" value sources from the environment instead of a
			// file, which is what CI needs (no secret ever touches disk).
			if name, ok := strings.CutPrefix(path, "env:"); ok {
				sources = append(sources, secretsprovider.Source{ID: id, Env: name})
				continue
			}
			abs := path
			if !filepath.IsAbs(abs) {
				abs = filepath.Join(contextDir, path)
			}
			if _, err := os.Stat(abs); err != nil {
				return nil, fmt.Errorf("build secret %q: %w", id, err)
			}
			sources = append(sources, secretsprovider.Source{ID: id, FilePath: abs})
		}
		store, err := secretsprovider.NewStore(sources)
		if err != nil {
			return nil, fmt.Errorf("configuring build secrets: %w", err)
		}
		attachable = append(attachable, secretsprovider.NewSecretProvider(store))
	}

	return attachable, nil
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
