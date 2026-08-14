package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

type releaseServer struct {
	URL     string
	Asset   string
	Payload []byte
	close   func()
}

func startReleaseServer(t *testing.T, version string, payload []byte) *releaseServer {
	t.Helper()
	if payload == nil {
		payload = []byte("#!/bin/sh\necho 'buidl version " + version + "'\n")
	}
	asset := AssetName(runtime.GOOS, runtime.GOARCH)
	sum := sha256.Sum256(payload)
	checksums := hex.EncodeToString(sum[:]) + "  " + asset + "\n"

	mux := http.NewServeMux()
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/releases/tag/"+version, http.StatusFound)
	})
	mux.HandleFunc("/releases/tag/"+version, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/releases/download/"+version+"/checksums.txt", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(checksums))
	})
	mux.HandleFunc("/releases/download/"+version+"/"+asset, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &releaseServer{URL: srv.URL, Asset: asset, Payload: payload, close: srv.Close}
}

func TestLatestFollowsRedirect(t *testing.T) {
	srv := startReleaseServer(t, "v9.9.9", nil)
	c := New("v0.1.6")
	c.BaseURL = srv.URL

	got, err := c.Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != "v9.9.9" {
		t.Errorf("Latest = %q, want v9.9.9", got)
	}
}

func TestAssetName(t *testing.T) {
	tests := []struct{ os, arch, want string }{
		{"linux", "amd64", "buidl-linux-amd64"},
		{"linux", "x86_64", "buidl-linux-amd64"},
		{"darwin", "arm64", "buidl-darwin-arm64"},
		{"darwin", "aarch64", "buidl-darwin-arm64"},
		{"Darwin", "ARM64", "buidl-darwin-arm64"},
	}
	for _, tt := range tests {
		if got := AssetName(tt.os, tt.arch); got != tt.want {
			t.Errorf("AssetName(%q, %q) = %q, want %q", tt.os, tt.arch, got, tt.want)
		}
	}
}

func TestParseChecksums(t *testing.T) {
	in := "abc123def456  buidl-linux-amd64\n" +
		"deadbeef  *buidl-darwin-arm64\r\n" +
		"# comment\n" +
		"\n"
	got, err := ParseChecksums(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if got["buidl-linux-amd64"] != "abc123def456" {
		t.Errorf("linux = %q", got["buidl-linux-amd64"])
	}
	if got["buidl-darwin-arm64"] != "deadbeef" {
		t.Errorf("darwin = %q", got["buidl-darwin-arm64"])
	}
}

func TestDownloadVerifiesChecksum(t *testing.T) {
	payload := []byte("the-real-bytes")
	srv := startReleaseServer(t, "v9.9.9", payload)
	c := New("v0.1.6")
	c.BaseURL = srv.URL

	dir := t.TempDir()
	path, err := c.Download(context.Background(), "v9.9.9", dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Errorf("downloaded %q, want %q", got, payload)
	}
}

func TestDownloadRejectsMismatch(t *testing.T) {
	payload := []byte("honest")
	srv := startReleaseServer(t, "v9.9.9", payload)

	// Rewrite checksums.txt to a wrong digest after the server is up by wrapping.
	mux := http.NewServeMux()
	asset := AssetName(runtime.GOOS, runtime.GOARCH)
	mux.HandleFunc("/releases/download/v9.9.9/checksums.txt", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s  %s\n", strings.Repeat("ab", 32), asset)
	})
	mux.HandleFunc("/releases/download/v9.9.9/"+asset, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	})
	bad := httptest.NewServer(mux)
	t.Cleanup(bad.Close)

	c := New("v0.1.6")
	c.BaseURL = bad.URL
	dir := t.TempDir()
	_, err := c.Download(context.Background(), "v9.9.9", dir)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("error = %v, want checksum mismatch", err)
	}
	// A failed verify must not leave a file that looks installable.
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("dest dir not empty after mismatch: %v", entries)
	}
	_ = srv
}

func TestInstallReplacesDest(t *testing.T) {
	payload := []byte("new-binary")
	srv := startReleaseServer(t, "v9.9.9", payload)
	c := New("v0.1.6")
	c.BaseURL = srv.URL

	dest := filepath.Join(t.TempDir(), "bin", "buidl")
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := c.Install(context.Background(), "v9.9.9", dest); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Errorf("dest = %q, want %q", got, payload)
	}
}

func TestCheckUsesCache(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "update-check")
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	body := "latest=v0.1.7\nchecked_at=" + now.Format(time.RFC3339) + "\n"
	if err := os.WriteFile(cache, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	// A client whose BaseURL would fail if the network were used.
	c := New("v0.1.6")
	c.BaseURL = "http://127.0.0.1:1"
	c.CachePath = cache
	c.Now = func() time.Time { return now.Add(time.Hour) }

	res, err := c.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Latest != "v0.1.7" || !res.Newer {
		t.Errorf("Check = %+v, want latest v0.1.7 newer", res)
	}
}

func TestCheckRefetchesStaleCache(t *testing.T) {
	srv := startReleaseServer(t, "v0.2.0", nil)
	cache := filepath.Join(t.TempDir(), "update-check")
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.WriteFile(cache, []byte("latest=v0.1.0\nchecked_at="+old.Format(time.RFC3339)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := New("v0.1.6")
	c.BaseURL = srv.URL
	c.CachePath = cache
	c.Now = func() time.Time { return old.Add(CheckTTL + time.Hour) }

	res, err := c.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Latest != "v0.2.0" {
		t.Errorf("Latest = %q, want v0.2.0 after stale cache", res.Latest)
	}
}

func TestCheckWritesCache(t *testing.T) {
	srv := startReleaseServer(t, "v0.2.0", nil)
	c := New("v0.1.6")
	c.BaseURL = srv.URL
	c.CachePath = filepath.Join(t.TempDir(), "nested", "update-check")
	c.Now = func() time.Time { return time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC) }

	if _, err := c.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(c.CachePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "latest=v0.2.0") {
		t.Errorf("cache = %q, want latest=v0.2.0", data)
	}
}

func TestLatestHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	c := New("v0.1.6")
	c.BaseURL = srv.URL
	if _, err := c.Latest(context.Background()); err == nil {
		t.Fatal("expected HTTP error")
	}
}
