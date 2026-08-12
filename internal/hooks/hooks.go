// Package hooks runs user-supplied executables at points in the deploy lifecycle.
//
// The motivating case is database migrations. A migration must run after the new
// image exists but before the new release starts serving, and it needs a
// credential the application itself should not hold. A pre-deploy hook is the
// natural place: it receives the release's identity and its resolved secrets in
// its environment, so it can run `migrate` against an owner-role URL that the app
// never sees.
//
// Hooks are plain executables in .buidl/hooks, named for the lifecycle point. A
// missing hook is not an error — most projects need none.
package hooks

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Point is a lifecycle position at which a hook may run.
type Point string

const (
	// PreBuild runs before the image is built. Use for code generation or asset
	// compilation that must not live in the Dockerfile.
	PreBuild Point = "pre-build"
	// PostBuild runs after the image is pushed, so BUIDL_DIGEST is available.
	PostBuild Point = "post-build"
	// PreDeploy runs after preflight passes but before anything is applied. This
	// is where migrations belong: the image exists, the cluster is reachable, and
	// nothing is serving the new release yet.
	//
	// A non-zero exit aborts the deploy, which is the point — a failed migration
	// must not be followed by code that assumes it succeeded.
	PreDeploy Point = "pre-deploy"
	// PostDeploy runs after the release is healthy and serving.
	PostDeploy Point = "post-deploy"
	// DeployFailed runs when a deploy fails, for notifications. Its own failure is
	// reported but does not change the outcome, which is already a failure.
	DeployFailed Point = "deploy-failed"
)

// Points lists every hook point in lifecycle order.
func Points() []Point {
	return []Point{PreBuild, PostBuild, PreDeploy, PostDeploy, DeployFailed}
}

// Description explains when a hook point fires, for scaffolding and docs.
func (p Point) Description() string {
	switch p {
	case PreBuild:
		return "before the image is built"
	case PostBuild:
		return "after the image is pushed (BUIDL_DIGEST is set)"
	case PreDeploy:
		return "after preflight, before anything is applied — run migrations here"
	case PostDeploy:
		return "after the release is healthy and serving"
	case DeployFailed:
		return "after a failed deploy, for notifications"
	default:
		return ""
	}
}

// Aborts reports whether a non-zero exit from this hook should stop the deploy.
func (p Point) Aborts() bool {
	// A post-deploy or failure notification cannot un-deploy anything, so failing
	// the command on its account would misrepresent what happened.
	return p != PostDeploy && p != DeployFailed
}

// Context carries the values a hook receives in its environment.
type Context struct {
	App         string
	Environment string
	Release     string
	Digest      string
	Image       string
	Namespace   string
	GitSHA      string
	GitBranch   string
	Actor       string
	URL         string
	// Version is the buidl version, so a hook can assert on it.
	Version string

	// Secrets are the resolved values for env.secret. Hooks need these for the
	// migration case, where the credential is deliberately not given to the app.
	Secrets map[string]string
}

// Logger is the output surface this package needs.
type Logger interface {
	Info(format string, args ...any)
	Detail(format string, args ...any)
	Warn(format string, args ...any)
}

// Runner discovers and executes hooks.
type Runner struct {
	// Dir is the absolute hooks directory.
	Dir string
	log Logger
	// Timeout bounds a single hook. Migrations can be slow, so this is generous,
	// but unbounded would let a hung hook hold a deploy open indefinitely.
	Timeout time.Duration
}

// NewRunner builds a Runner for a project root and configured hooks path.
func NewRunner(root, hooksPath string, log Logger) *Runner {
	dir := hooksPath
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(root, hooksPath)
	}
	return &Runner{Dir: dir, log: log, Timeout: 15 * time.Minute}
}

// Result describes one hook execution.
type Result struct {
	Point    Point
	Path     string
	Ran      bool
	Duration time.Duration
	Err      error
}

// Available reports the hook points that have an executable present.
func (r *Runner) Available() []Point {
	var out []Point
	for _, point := range Points() {
		if path, ok := r.lookup(point); ok {
			_ = path
			out = append(out, point)
		}
	}
	return out
}

// lookup finds an executable hook for a point.
func (r *Runner) lookup(point Point) (string, bool) {
	path := filepath.Join(r.Dir, string(point))
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return "", false
	}
	// A hook that is present but not executable is almost always a mistake — a
	// forgotten chmod after checkout — so it is reported rather than ignored.
	if info.Mode().Perm()&0o111 == 0 {
		r.log.Warn("hook %s is not executable and will be skipped; run `chmod +x %s`", point, path)
		return "", false
	}
	return path, true
}

// Run executes the hook for a point, if one exists.
//
// Returns a Result with Ran=false when no hook is present, which callers treat as
// success: hooks are optional.
func (r *Runner) Run(ctx context.Context, point Point, hookCtx Context) Result {
	result := Result{Point: point}

	path, ok := r.lookup(point)
	if !ok {
		return result
	}
	result.Path = path
	result.Ran = true

	r.log.Info("running %s hook", point)
	start := time.Now()

	ctx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, path)
	cmd.Dir = filepath.Dir(r.Dir) // the project root, so relative paths work
	cmd.Env = r.environment(hookCtx)
	// Stream straight through: a migration's output is exactly what a user wants
	// to see, and buffering it would hide progress on a slow one.
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	err := cmd.Run()
	result.Duration = time.Since(start)

	if err != nil {
		var exitErr *exec.ExitError
		switch {
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			result.Err = fmt.Errorf("%s hook timed out after %s", point, r.Timeout)
		case errors.As(err, &exitErr):
			result.Err = fmt.Errorf("%s hook failed (exit %d): %s", point, exitErr.ExitCode(), path)
		default:
			result.Err = fmt.Errorf("%s hook could not run: %w", point, err)
		}
		return result
	}

	r.log.Detail("%s hook succeeded in %s", point, result.Duration.Round(time.Millisecond))
	return result
}

// environment builds the hook's environment.
//
// The parent environment is inherited so a hook can use PATH and the tools it
// expects, then buidl's own variables and the resolved secrets are layered on.
func (r *Runner) environment(hookCtx Context) []string {
	env := os.Environ()

	add := func(name, value string) {
		if value != "" {
			env = append(env, name+"="+value)
		}
	}

	add("BUIDL_APP", hookCtx.App)
	add("BUIDL_ENV", hookCtx.Environment)
	add("BUIDL_RELEASE", hookCtx.Release)
	add("BUIDL_DIGEST", hookCtx.Digest)
	add("BUIDL_IMAGE", hookCtx.Image)
	add("BUIDL_NAMESPACE", hookCtx.Namespace)
	add("BUIDL_GIT_SHA", hookCtx.GitSHA)
	add("BUIDL_GIT_BRANCH", hookCtx.GitBranch)
	add("BUIDL_ACTOR", hookCtx.Actor)
	add("BUIDL_URL", hookCtx.URL)
	add("BUIDL_VERSION", hookCtx.Version)
	// Marks the process as running inside a hook, so a script can refuse to run
	// standalone if that would be unsafe.
	add("BUIDL_HOOK", "1")

	// Secrets last so they cannot be shadowed by buidl's own names.
	names := make([]string, 0, len(hookCtx.Secrets))
	for name := range hookCtx.Secrets {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		env = append(env, name+"="+hookCtx.Secrets[name])
	}

	return env
}

// SampleHook returns the contents of a scaffolded example hook.
func SampleHook(point Point) string {
	var b strings.Builder

	fmt.Fprintf(&b, `#!/usr/bin/env bash
# buidl %s hook — runs %s.
#
# Make it executable to enable it:
#   chmod +x .buidl/hooks/%s
#
# Available in the environment:
#   BUIDL_APP BUIDL_ENV BUIDL_RELEASE BUIDL_DIGEST BUIDL_IMAGE
#   BUIDL_NAMESPACE BUIDL_GIT_SHA BUIDL_GIT_BRANCH BUIDL_ACTOR BUIDL_URL
# plus every secret listed under env.secret in buidl.yaml.
`, point, point.Description(), point)

	// A non-zero exit from an aborting hook stops the deploy, so say so.
	if point.Aborts() {
		fmt.Fprintf(&b, "#\n# Exiting non-zero ABORTS the deploy.\n")
	} else {
		fmt.Fprintf(&b, "#\n# Exiting non-zero is reported but does not fail the deploy.\n")
	}

	b.WriteString("\nset -euo pipefail\n\n")

	switch point {
	case PreDeploy:
		b.WriteString(`echo "would migrate $BUIDL_APP ($BUIDL_ENV) to release $BUIDL_RELEASE"

# Run migrations here, before the new release serves traffic. Use a credential
# the application itself does not get, so the app cannot alter its own schema:
#
#   migrate -database "$MIGRATIONS_DATABASE_URL" -path ./migrations up
`)
	case PostDeploy:
		b.WriteString(`echo "deployed $BUIDL_APP ($BUIDL_ENV) release $BUIDL_RELEASE"

# Notify, warm a cache, or purge a CDN here.
#
#   curl -fsS -X POST "$SLACK_WEBHOOK" \
#     -d "{\"text\":\"deployed $BUIDL_APP $BUIDL_RELEASE to $BUIDL_ENV\"}"
`)
	case DeployFailed:
		b.WriteString(`echo "deploy of $BUIDL_APP ($BUIDL_ENV) failed at release $BUIDL_RELEASE" >&2

# Page someone, or record the failure.
`)
	default:
		fmt.Fprintf(&b, "echo \"%s: $BUIDL_APP ($BUIDL_ENV) release $BUIDL_RELEASE\"\n", point)
	}

	return b.String()
}
