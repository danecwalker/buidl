// Package update finds and installs buidl releases from GitHub.
package update

import (
	"strconv"
	"strings"
)

// Version is a parsed release tag or `git describe` string.
type Version struct {
	Major, Minor, Patch int
	// Commits is the N in v0.1.6-3-gabc: builds after a tag. A clean tag is 0.
	Commits int
}

// Parseable reports whether s can be compared to a release tag.
//
// `dev` (go install without -ldflags) is not parseable, so a source build
// does not nag about "updates" it cannot identify.
func Parseable(s string) bool {
	_, ok := Parse(s)
	return ok
}

// Parse reads a release tag or git-describe string. The leading v is optional.
func Parse(s string) (Version, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	s = strings.TrimSuffix(s, "-dirty")
	if s == "" || s == "dev" {
		return Version{}, false
	}

	var commits int
	if i := strings.Index(s, "-"); i >= 0 {
		n, ok := gitDescribeCommits(s[i+1:])
		if !ok {
			return Version{}, false
		}
		commits = n
		s = s[:i]
	}

	parts := strings.Split(s, ".")
	if len(parts) < 2 || len(parts) > 3 {
		return Version{}, false
	}
	maj, err := strconv.Atoi(parts[0])
	if err != nil || maj < 0 {
		return Version{}, false
	}
	min, err := strconv.Atoi(parts[1])
	if err != nil || min < 0 {
		return Version{}, false
	}
	pat := 0
	if len(parts) == 3 {
		pat, err = strconv.Atoi(parts[2])
		if err != nil || pat < 0 {
			return Version{}, false
		}
	}
	return Version{Major: maj, Minor: min, Patch: pat, Commits: commits}, true
}

// gitDescribeCommits accepts the suffix of `git describe --tags`, e.g.
// "3-gabcdef". Other suffixes (rc.1) are not something we ship, so they
// stay incomparable rather than being guessed.
func gitDescribeCommits(rest string) (int, bool) {
	head, tail, ok := strings.Cut(rest, "-")
	if !ok || !strings.HasPrefix(tail, "g") {
		return 0, false
	}
	n, err := strconv.Atoi(head)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// Newer reports whether latest is a release tag after current.
//
// A git-describe build of the same X.Y.Z is not older than that tag: the
// developer is already running something at or past the published release.
func Newer(current, latest string) bool {
	c, okc := Parse(current)
	l, okl := Parse(latest)
	if !okc || !okl {
		return false
	}
	if l.Major != c.Major {
		return l.Major > c.Major
	}
	if l.Minor != c.Minor {
		return l.Minor > c.Minor
	}
	if l.Patch != c.Patch {
		return l.Patch > c.Patch
	}
	return false
}
