package update

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func installScript(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		candidate := filepath.Join(dir, "install.sh")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("install.sh not found")
		}
		dir = parent
	}
}

func TestInstallScript(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not available")
	}

	version := "v9.9.9"
	payload := []byte("#!/bin/sh\necho 'buidl version " + version + "'\n")
	srv := startReleaseServer(t, version, payload)
	destDir := t.TempDir()

	cmd := exec.Command("bash", installScript(t))
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + destDir,
		"TMPDIR=" + destDir,
		"BUIDL_BASE_URL=" + srv.URL,
		"BUIDL_INSTALL_DIR=" + destDir,
		"BUIDL_VERSION=" + version,
		"NO_COLOR=1",
		"TERM=dumb",
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("install.sh: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "checksum") {
		t.Errorf("expected checksum confirmation:\n%s", out)
	}

	dest := filepath.Join(destDir, "buidl")
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Errorf("installed %q, want %q", got, payload)
	}

	ver, err := exec.Command(dest, "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("installed binary: %v\n%s", err, ver)
	}
	if !strings.Contains(string(ver), version) {
		t.Errorf("--version = %q, want %s", ver, version)
	}
}

func TestInstallScriptRejectsBadChecksum(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not available")
	}

	version := "v9.9.9"
	payload := []byte("tampered")
	asset := AssetName(runtime.GOOS, runtime.GOARCH)
	mux := http.NewServeMux()
	mux.HandleFunc("/releases/download/"+version+"/checksums.txt", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s  %s\n", strings.Repeat("ab", 32), asset)
	})
	mux.HandleFunc("/releases/download/"+version+"/"+asset, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	destDir := t.TempDir()
	cmd := exec.Command("bash", installScript(t))
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + destDir,
		"TMPDIR=" + destDir,
		"BUIDL_BASE_URL=" + srv.URL,
		"BUIDL_INSTALL_DIR=" + destDir,
		"BUIDL_VERSION=" + version,
		"NO_COLOR=1",
		"TERM=dumb",
	}
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected checksum failure, got success:\n%s", out)
	}
	if !strings.Contains(string(out), "checksum mismatch") {
		t.Errorf("error should name the mismatch:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(destDir, "buidl")); !os.IsNotExist(err) {
		t.Errorf("failed install left a binary: %v", err)
	}
}
