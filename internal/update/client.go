package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// DefaultBaseURL is the GitHub repository whose Releases hold the binaries.
const DefaultBaseURL = "https://github.com/danecwalker/buidl"

const userAgentPrefix = "buidl"

// Client talks to a GitHub-style /releases tree.
type Client struct {
	// BaseURL is the repository root, e.g. https://github.com/danecwalker/buidl.
	// Tests point this at httptest.
	BaseURL string
	HTTP    *http.Client
	// CachePath stores the last successful latest-tag lookup. Empty uses
	// DefaultCachePath.
	CachePath string
	Current   string
	GOOS      string
	GOARCH    string
	Now       func() time.Time
	// Progress is invoked as bytes arrive. total is -1 when unknown.
	Progress func(done, total int64)
}

// New builds a client for the running binary's version and platform.
func New(current string) *Client {
	return &Client{
		BaseURL:   DefaultBaseURL,
		Current:   current,
		GOOS:      runtime.GOOS,
		GOARCH:    runtime.GOARCH,
		CachePath: DefaultCachePath(),
		Now:       time.Now,
	}
}

func (c *Client) base() string {
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	return DefaultBaseURL
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	// No client-level timeout: each call is bounded by its context. A 30s
	// Timeout here would kill a slow download of a 50MB binary.
	return &http.Client{}
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) goos() string {
	if c.GOOS != "" {
		return c.GOOS
	}
	return runtime.GOOS
}

func (c *Client) goarch() string {
	if c.GOARCH != "" {
		return c.GOARCH
	}
	return runtime.GOARCH
}

func (c *Client) ua() string {
	if c.Current != "" {
		return userAgentPrefix + "/" + c.Current
	}
	return userAgentPrefix
}

func (c *Client) get(ctx context.Context, rawURL string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.ua())
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("%s: HTTP %d", rawURL, resp.StatusCode)
	}
	return resp, nil
}

// Latest returns the tag of the most recent GitHub release by following
// /releases/latest. That avoids the API rate limit and needs no token.
func (c *Client) Latest(ctx context.Context) (string, error) {
	raw := c.base() + "/releases/latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", c.ua())
	resp, err := c.http().Do(req)
	if err != nil {
		return "", err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("%s: HTTP %d", raw, resp.StatusCode)
	}
	final := ""
	if resp.Request != nil && resp.Request.URL != nil {
		final = resp.Request.URL.String()
	}
	tag := tagFromURL(final)
	if tag == "" {
		return "", fmt.Errorf("could not determine the latest release tag from %s", final)
	}
	return tag, nil
}

func tagFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i := 0; i < len(parts)-1; i++ {
		if parts[i] == "tag" {
			return parts[i+1]
		}
	}
	return ""
}

// PlatformAsset is AssetName for this client's OS/arch, falling back to
// the running process when the fields are empty.
func (c *Client) PlatformAsset() string {
	return AssetName(c.goos(), c.goarch())
}

// AssetName is the file published by `make release` for this platform.
func AssetName(goos, goarch string) string {
	osName := strings.ToLower(goos)
	arch := strings.ToLower(goarch)
	switch arch {
	case "x86_64":
		arch = "amd64"
	case "aarch64":
		arch = "arm64"
	}
	return "buidl-" + osName + "-" + arch
}

// ParseChecksums reads the `sha256sum` / `shasum -a 256` file `make release`
// writes: bare filenames, two spaces (or " *") before the name.
func ParseChecksums(r io.Reader) (map[string]string, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		hexSum, name, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		name = strings.TrimPrefix(name, "*")
		if hexSum == "" || name == "" {
			continue
		}
		if _, err := hex.DecodeString(hexSum); err != nil {
			continue
		}
		out[name] = strings.ToLower(hexSum)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("checksums.txt contained no entries")
	}
	return out, nil
}

// Download fetches checksums.txt and the platform binary into destDir, and
// refuses to return a path whose digest does not match.
func (c *Client) Download(ctx context.Context, version, destDir string) (string, error) {
	asset := AssetName(c.goos(), c.goarch())
	sumsURL := fmt.Sprintf("%s/releases/download/%s/checksums.txt", c.base(), version)
	resp, err := c.get(ctx, sumsURL)
	if err != nil {
		return "", fmt.Errorf("fetching checksums: %w", err)
	}
	sums, err := ParseChecksums(resp.Body)
	resp.Body.Close()
	if err != nil {
		return "", err
	}
	want, ok := sums[asset]
	if !ok {
		return "", fmt.Errorf("checksums.txt has no entry for %s (have %s)", asset, joinNames(sums))
	}

	binURL := fmt.Sprintf("%s/releases/download/%s/%s", c.base(), version, asset)
	dest := filepath.Join(destDir, asset)
	if err := c.fetchVerified(ctx, binURL, dest, want); err != nil {
		return "", err
	}
	return dest, nil
}

func joinNames(m map[string]string) string {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	return strings.Join(names, ", ")
}

func (c *Client) fetchVerified(ctx context.Context, rawURL, dest, wantHex string) error {
	resp, err := c.get(ctx, rawURL)
	if err != nil {
		return fmt.Errorf("downloading %s: %w", filepath.Base(dest), err)
	}
	defer resp.Body.Close()

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}

	h := sha256.New()
	w := io.MultiWriter(f, h)
	total := resp.ContentLength
	var done int64
	buf := make([]byte, 32*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, err := w.Write(buf[:n]); err != nil {
				f.Close()
				os.Remove(dest)
				return err
			}
			done += int64(n)
			if c.Progress != nil {
				c.Progress(done, total)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			f.Close()
			os.Remove(dest)
			return readErr
		}
	}
	if err := f.Close(); err != nil {
		os.Remove(dest)
		return err
	}

	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, wantHex) {
		os.Remove(dest)
		return fmt.Errorf("checksum mismatch for %s: got %s, want %s", filepath.Base(dest), got, wantHex)
	}
	return nil
}

// Install downloads version and atomically replaces dest with it.
func (c *Client) Install(ctx context.Context, version, dest string) error {
	if dest == "" {
		return fmt.Errorf("install destination is empty")
	}
	dir := filepath.Dir(dest)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("cannot create %s: %w\n\nhint: install to a directory you can write, or re-run with permission to write there", dir, err)
	}
	// Same directory as dest so os.Rename cannot hit EXDEV and leave dest half
	// replaced. The leading dot keeps a failed run from looking like a binary.
	tmp, err := os.MkdirTemp(dir, ".buidl-update-")
	if err != nil {
		return fmt.Errorf("cannot write to %s: %w\n\nhint: re-run with sudo, or install to a directory you can write", dir, err)
	}
	defer os.RemoveAll(tmp)

	bin, err := c.Download(ctx, version, tmp)
	if err != nil {
		return err
	}
	return ReplaceFile(bin, dest)
}

// ReplaceFile moves src onto dest, keeping dest's mode when it already exists.
func ReplaceFile(src, dest string) error {
	mode := os.FileMode(0o755)
	if info, err := os.Stat(dest); err == nil {
		mode = info.Mode()
	}
	if err := os.Chmod(src, mode); err != nil {
		return err
	}
	if err := os.Rename(src, dest); err != nil {
		return fmt.Errorf("cannot replace %s: %w\n\nhint: re-run with sudo, or install to a directory you can write", dest, err)
	}
	return nil
}

// Executable is the real path of this process. Symlinks are resolved so a
// Homebrew-style shim is not overwritten with a full binary.
func Executable() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("cannot locate this buidl binary: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved, nil
	}
	return p, nil
}
