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

func writeExec(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func failingSudo(t *testing.T, binDir string) {
	t.Helper()
	writeExec(t, binDir, "sudo", "#!/bin/sh\necho 'sudo should not run' >&2\nexit 1\n")
}

// cooperatingSudo never prompts. It makes dest writable so the following
// command can succeed as the test user, then execs it.
func cooperatingSudo(t *testing.T, binDir string) {
	t.Helper()
	writeExec(t, binDir, "sudo", `#!/bin/sh
while [ $# -gt 0 ]; do
  case "$1" in
    -v|-n) shift ;;
    -p) shift 2 ;;
    --) shift; break ;;
    -*) shift ;;
    *) break ;;
  esac
done
if [ $# -eq 0 ]; then
  exit 0
fi
if [ -n "${SUDO_DEST_DIR:-}" ]; then
  if [ -d "$SUDO_DEST_DIR" ]; then
    chmod u+w "$SUDO_DEST_DIR" || true
  else
    parent=$(dirname "$SUDO_DEST_DIR")
    if [ -d "$parent" ]; then
      chmod u+w "$parent" || true
    fi
  fi
fi
exec "$@"
`)
}

func hideSudoPath(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	tools := []string{
		"curl", "awk", "uname", "mkdir", "cp", "chmod", "mv", "mktemp",
		"rm", "tr", "sed", "wc", "id", "dirname", "basename", "sleep",
		"tail", "cat", "head", "true", "ln",
	}
	if _, err := exec.LookPath("sha256sum"); err == nil {
		tools = append(tools, "sha256sum")
	} else {
		tools = append(tools, "shasum")
	}
	for _, name := range tools {
		src, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		if err := os.Symlink(src, filepath.Join(bin, name)); err != nil {
			t.Fatal(err)
		}
	}
	return bin
}

func installEnv(home, tmp, dest, baseURL, version, path string, extra ...string) []string {
	env := []string{
		"PATH=" + path,
		"HOME=" + home,
		"TMPDIR=" + tmp,
		"BUIDL_BASE_URL=" + baseURL,
		"BUIDL_INSTALL_DIR=" + dest,
		"BUIDL_VERSION=" + version,
		"NO_COLOR=1",
		"TERM=dumb",
	}
	return append(env, extra...)
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
	binDir := t.TempDir()
	failingSudo(t, binDir)

	cmd := exec.Command("bash", installScript(t))
	cmd.Env = installEnv(destDir, destDir, destDir, srv.URL, version, binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("install.sh: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "checksum") {
		t.Errorf("expected checksum confirmation:\n%s", out)
	}
	if strings.Contains(string(out), "needs sudo") {
		t.Errorf("writable dest should not mention sudo:\n%s", out)
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
	cmd.Env = installEnv(destDir, destDir, destDir, srv.URL, version, os.Getenv("PATH"))
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

func TestInstallScriptUsesSudoWhenDestNotWritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write a 0555 directory")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not available")
	}

	version := "v9.9.9"
	payload := []byte("#!/bin/sh\necho 'buidl version " + version + "'\n")
	srv := startReleaseServer(t, version, payload)

	root := t.TempDir()
	home := filepath.Join(root, "home")
	tmp := filepath.Join(root, "tmp")
	destDir := filepath.Join(root, "dest")
	for _, dir := range []string{home, tmp, destDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(destDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(destDir, 0o755) })

	binDir := t.TempDir()
	cooperatingSudo(t, binDir)

	cmd := exec.Command("bash", installScript(t))
	cmd.Env = installEnv(home, tmp, destDir, srv.URL, version, binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"SUDO_DEST_DIR="+destDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("install.sh: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "needs sudo to write") {
		t.Errorf("expected sudo notice:\n%s", out)
	}
	got, err := os.ReadFile(filepath.Join(destDir, "buidl"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Errorf("installed %q, want %q", got, payload)
	}
}

func TestInstallScriptFailsWithoutSudoWhenDestNotWritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write a 0555 directory")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not available")
	}

	version := "v9.9.9"
	payload := []byte("#!/bin/sh\necho 'buidl version " + version + "'\n")
	srv := startReleaseServer(t, version, payload)

	root := t.TempDir()
	home := filepath.Join(root, "home")
	tmp := filepath.Join(root, "tmp")
	destDir := filepath.Join(root, "dest")
	for _, dir := range []string{home, tmp, destDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(destDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(destDir, 0o755) })

	cmd := exec.Command("bash", installScript(t))
	cmd.Env = installEnv(home, tmp, destDir, srv.URL, version, hideSudoPath(t))
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected failure without sudo, got success:\n%s", out)
	}
	if !strings.Contains(string(out), "sudo is required") {
		t.Errorf("error should mention sudo:\n%s", out)
	}
	if !strings.Contains(string(out), "BUIDL_INSTALL_DIR") {
		t.Errorf("error should hint at BUIDL_INSTALL_DIR:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(destDir, "buidl")); !os.IsNotExist(err) {
		t.Errorf("failed install left a binary: %v", err)
	}
}

func TestInstallScriptDefaultsToHomeLocalBin(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not available")
	}

	version := "v9.9.9"
	payload := []byte("#!/bin/sh\necho 'buidl version " + version + "'\n")
	srv := startReleaseServer(t, version, payload)

	home := t.TempDir()
	userBin := filepath.Join(home, ".local", "bin")
	binDir := t.TempDir()
	failingSudo(t, binDir)

	// userBin is on PATH so the script must not try to sudo a /usr/local/bin link.
	cmd := exec.Command("bash", installScript(t))
	cmd.Env = []string{
		"PATH=" + userBin + string(os.PathListSeparator) + binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + home,
		"TMPDIR=" + t.TempDir(),
		"BUIDL_BASE_URL=" + srv.URL,
		"BUIDL_VERSION=" + version,
		"NO_COLOR=1",
		"TERM=dumb",
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("install.sh: %v\n%s", err, out)
	}
	if strings.Contains(string(out), "needs sudo") {
		t.Errorf("default dest is user-owned; should not sudo:\n%s", out)
	}
	got, err := os.ReadFile(filepath.Join(userBin, "buidl"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Errorf("installed %q, want %q", got, payload)
	}
}

func TestInstallScriptLinksWhenNotOnPath(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not available")
	}

	version := "v9.9.9"
	payload := []byte("#!/bin/sh\necho 'buidl version " + version + "'\n")
	srv := startReleaseServer(t, version, payload)

	home := t.TempDir()
	linkDir := t.TempDir()
	binDir := t.TempDir()
	failingSudo(t, binDir)

	cmd := exec.Command("bash", installScript(t))
	cmd.Env = []string{
		"PATH=" + linkDir + string(os.PathListSeparator) + binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + home,
		"TMPDIR=" + t.TempDir(),
		"BUIDL_BASE_URL=" + srv.URL,
		"BUIDL_VERSION=" + version,
		"BUIDL_PATH_LINK_DIR=" + linkDir,
		"NO_COLOR=1",
		"TERM=dumb",
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("install.sh: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "linked") {
		t.Errorf("expected a PATH symlink:\n%s", out)
	}
	link := filepath.Join(linkDir, "buidl")
	got, err := os.Readlink(link)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".local", "bin", "buidl")
	if got != want {
		t.Errorf("symlink = %q, want %q", got, want)
	}
	body, err := os.ReadFile(link)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != string(payload) {
		t.Errorf("linked binary = %q, want payload", body)
	}
}

func TestInstallScriptPathHintWhenLinkFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write a 0555 directory")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not available")
	}

	version := "v9.9.9"
	payload := []byte("#!/bin/sh\necho 'buidl version " + version + "'\n")
	srv := startReleaseServer(t, version, payload)

	home := t.TempDir()
	linkDir := t.TempDir()
	if err := os.Chmod(linkDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(linkDir, 0o755) })

	cmd := exec.Command("bash", installScript(t))
	cmd.Env = []string{
		"PATH=" + linkDir + string(os.PathListSeparator) + hideSudoPath(t) + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + home,
		"TMPDIR=" + t.TempDir(),
		"BUIDL_BASE_URL=" + srv.URL,
		"BUIDL_VERSION=" + version,
		"BUIDL_PATH_LINK_DIR=" + linkDir,
		"NO_COLOR=1",
		"TERM=dumb",
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("install should succeed without the link: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "add ") || !strings.Contains(string(out), "PATH") {
		t.Errorf("expected a PATH hint when the link cannot be created:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(home, ".local", "bin", "buidl")); err != nil {
		t.Fatalf("real binary missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(linkDir, "buidl")); !os.IsNotExist(err) {
		t.Errorf("failed link left a file: %v", err)
	}
}
