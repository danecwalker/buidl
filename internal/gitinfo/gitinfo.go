// Package gitinfo reads repository provenance for a deploy.
//
// Every release records where it came from. This is what makes
// `buidl status --history` useful during an incident: you can see which
// commit is live, whether it was built from a dirty tree, and who shipped it.
package gitinfo

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

// Info describes the state of the working tree a build came from.
type Info struct {
	SHA      string
	ShortSHA string
	Branch   string
	Tag      string
	Subject  string
	Author   string
	Remote   string
	// Dirty reports uncommitted changes. Deploying a dirty tree is allowed but
	// warned about, and blocked in CI-strict mode, because the release is then
	// not reproducible from any commit.
	Dirty bool
	// Available is false when the directory is not a git repository. buidl still
	// works (falling back to timestamp-based release IDs); it just records less.
	//
	// Available with an empty SHA means a repository that exists but has no
	// commits yet. That is a legitimate state for every command except the ones
	// that mint a release; see RequireCommit.
	Available bool
}

// Load gathers provenance for dir. It never returns an error for "not a git
// repo" — that is a supported mode, reported via Info.Available.
func Load(ctx context.Context, dir string) (Info, error) {
	info := Info{}

	if _, err := exec.LookPath("git"); err != nil {
		return info, nil
	}
	if out, err := run(ctx, dir, "rev-parse", "--is-inside-work-tree"); err != nil || strings.TrimSpace(out) != "true" {
		return info, nil
	}
	info.Available = true

	// CI systems often check out a detached HEAD, so prefer the branch name the
	// CI provider tells us over `git branch --show-current`, which is empty then.
	info.SHA, _ = run(ctx, dir, "rev-parse", "HEAD")
	info.ShortSHA, _ = run(ctx, dir, "rev-parse", "--short=12", "HEAD")
	info.Branch, _ = run(ctx, dir, "rev-parse", "--abbrev-ref", "HEAD")
	if info.Branch == "HEAD" || info.Branch == "" {
		info.Branch = branchFromCI()
	}
	info.Subject, _ = run(ctx, dir, "log", "-1", "--pretty=%s")
	info.Author, _ = run(ctx, dir, "log", "-1", "--pretty=%an")
	info.Remote, _ = run(ctx, dir, "config", "--get", "remote.origin.url")
	// --exact-match fails on untagged commits, which is not an error for us.
	info.Tag, _ = run(ctx, dir, "describe", "--tags", "--exact-match")

	status, err := run(ctx, dir, "status", "--porcelain")
	if err != nil {
		return info, err
	}
	info.Dirty = strings.TrimSpace(status) != ""

	return info, nil
}

// RequireCommit reports whether there is a commit to attribute a release to.
//
// An empty repository is not an error in itself. `config validate`, `manifest`
// and `init` all work fine without a commit, and failing them was wrong: a
// freshly scaffolded project is exactly when someone runs validate, and a lint
// command that demands a commit first is a lint command nobody can use. Only
// the commands that mint a release need provenance, so they ask for it here.
func (i Info) RequireCommit() error {
	if i.Available && i.SHA == "" {
		return errors.New("repository has no commits; commit before deploying so the release records where it came from")
	}
	return nil
}

// branchFromCI recovers the branch name on providers that check out detached
// HEADs. Ordered from most to least specific.
func branchFromCI() string {
	// On GitHub pull_request events GITHUB_REF is refs/pull/N/merge, so the head
	// ref is the meaningful name for a preview environment.
	for _, key := range []string{
		"GITHUB_HEAD_REF",  // GitHub Actions, PR events
		"CI_COMMIT_BRANCH", // GitLab
		"BUILDKITE_BRANCH",
		"CIRCLE_BRANCH",
		"BRANCH_NAME", // Jenkins multibranch
	} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
	}
	// GITHUB_REF_NAME is "N/merge" on PR events; only trust it otherwise.
	if v := strings.TrimSpace(os.Getenv("GITHUB_REF_NAME")); v != "" && !strings.Contains(v, "/") {
		return v
	}
	return ""
}

func run(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	// Keep git from prompting for credentials in a non-interactive deploy.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

var slugUnsafe = regexp.MustCompile(`[^a-z0-9]+`)

// Slug converts a branch name into a DNS-label-safe token suitable for
// hostnames, namespaces and object names.
//
//	"feature/Add-OAuth!" -> "feature-add-oauth"
//
// The result is truncated to 40 characters to leave room for the prefixes and
// suffixes buidl adds while staying under Kubernetes' 63-character label limit.
func Slug(branch string) string {
	s := slugUnsafe.ReplaceAllString(strings.ToLower(branch), "-")
	s = strings.Trim(s, "-")
	if len(s) > 40 {
		s = strings.Trim(s[:40], "-")
	}
	if s == "" {
		return "unknown"
	}
	return s
}
