package update

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// CheckTTL is how long a cached latest-tag is trusted. Hitting GitHub on
// every `buidl status` would add latency and burn the unauthenticated
// rate limit; a day is enough to notice a release.
const CheckTTL = 24 * time.Hour

// Result is one comparison of the running binary against the latest release.
type Result struct {
	Current string
	Latest  string
	Newer   bool
}

// DefaultCachePath is the file that remembers the last successful lookup.
func DefaultCachePath() string {
	if p := os.Getenv("BUIDL_UPDATE_CACHE"); p != "" {
		return p
	}
	dir, err := os.UserCacheDir()
	if err != nil || dir == "" {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "buidl", "update-check")
}

func (c *Client) cachePath() string {
	if c.CachePath != "" {
		return c.CachePath
	}
	return DefaultCachePath()
}

// Check returns whether latest is newer than current. A fresh cache entry
// skips the network so a command is never blocked on GitHub.
func (c *Client) Check(ctx context.Context) (Result, error) {
	res := Result{Current: c.Current}
	if latest, at, ok := c.readCache(); ok && c.now().Sub(at) < CheckTTL {
		res.Latest = latest
		res.Newer = Newer(c.Current, latest)
		return res, nil
	}
	latest, err := c.Latest(ctx)
	if err != nil {
		return res, err
	}
	_ = c.writeCache(latest)
	res.Latest = latest
	res.Newer = Newer(c.Current, latest)
	return res, nil
}

func (c *Client) readCache() (latest string, at time.Time, ok bool) {
	data, err := os.ReadFile(c.cachePath())
	if err != nil {
		return "", time.Time{}, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		key, val, cut := strings.Cut(line, "=")
		if !cut {
			continue
		}
		switch strings.TrimSpace(key) {
		case "latest":
			latest = strings.TrimSpace(val)
		case "checked_at":
			t, err := time.Parse(time.RFC3339, strings.TrimSpace(val))
			if err == nil {
				at = t
			}
		}
	}
	if latest == "" || at.IsZero() {
		return "", time.Time{}, false
	}
	return latest, at, true
}

func (c *Client) writeCache(latest string) error {
	path := c.cachePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	body := fmt.Sprintf("latest=%s\nchecked_at=%s\n", latest, c.now().UTC().Format(time.RFC3339))
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(body), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
