package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/danecwalker/buidl/internal/ui"
	"github.com/danecwalker/buidl/internal/update"
)

func TestUpdateCheckReportsNewer(t *testing.T) {
	srv := startCLIReleaseServer(t, "v9.9.9", []byte("payload"))
	app, out := newTestApp(t, ui.ModePlain)
	app.opts.timeout = time.Minute
	app.updater = releaseClient(t, srv, "v0.1.6")

	cmd := newUpdateCmd(app)
	cmd.SetArgs([]string{"--check"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "v9.9.9") {
		t.Errorf("missing latest:\n%s", got)
	}
	if !strings.Contains(got, "buidl update") {
		t.Errorf("should tell the user how to install:\n%s", got)
	}
}

func TestUpdateIgnoresNoticeCache(t *testing.T) {
	// The notice cache can still say v0.1.6 while GitHub already has a
	// newer tag. `update` must not trust that file.
	srv := startCLIReleaseServer(t, "v9.9.9", []byte("payload"))
	app, out := newTestApp(t, ui.ModePlain)
	app.opts.timeout = time.Minute
	app.updater = releaseClient(t, srv, "v0.1.6")
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	if err := os.WriteFile(app.updater.CachePath, []byte("latest=v0.1.6\nchecked_at="+now.Format(time.RFC3339)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	app.updater.Now = func() time.Time { return now.Add(time.Minute) }

	cmd := newUpdateCmd(app)
	cmd.SetArgs([]string{"--check"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "v9.9.9") {
		t.Errorf("update --check used the cache instead of GitHub:\n%s", out)
	}
}

func TestUpdateCheckAlreadyCurrent(t *testing.T) {
	srv := startCLIReleaseServer(t, "v0.1.6", []byte("payload"))
	app, out := newTestApp(t, ui.ModePlain)
	app.opts.timeout = time.Minute
	app.updater = releaseClient(t, srv, "v0.1.6")

	cmd := newUpdateCmd(app)
	cmd.SetArgs([]string{"--check"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "already up to date") {
		t.Errorf("got:\n%s", out)
	}
}

func TestUpdateInstallsAndReplaces(t *testing.T) {
	payload := []byte("new-buidl-bytes")
	srv := startCLIReleaseServer(t, "v9.9.9", payload)
	dest := filepath.Join(t.TempDir(), "buidl")
	if err := os.WriteFile(dest, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}

	app, out := newTestApp(t, ui.ModePlain)
	app.opts.timeout = time.Minute
	app.updater = releaseClient(t, srv, "v0.1.6")
	app.updateDest = dest

	cmd := newUpdateCmd(app)
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Errorf("dest = %q, want %q", got, payload)
	}
	if !strings.Contains(out.String(), "installed v9.9.9") {
		t.Errorf("missing success:\n%s", out)
	}
}

func TestUpdateRelocatesWhenDestNotWritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write a 0555 directory")
	}
	payload := []byte("relocated-buidl")
	srv := startCLIReleaseServer(t, "v9.9.9", payload)

	locked := t.TempDir()
	dest := filepath.Join(locked, "buidl")
	if err := os.WriteFile(dest, []byte("old-system"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BUIDL_INSTALL_DIR", "")

	app, out := newTestApp(t, ui.ModePlain)
	app.opts.timeout = time.Minute
	app.updater = releaseClient(t, srv, "v0.1.6")
	app.updateDest = dest

	cmd := newUpdateCmd(app)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	// The system copy must stay put — we cannot write it.
	old, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(old) != "old-system" {
		t.Errorf("unwritable dest was overwritten: %q", old)
	}

	got, err := os.ReadFile(filepath.Join(home, ".local", "bin", "buidl"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Errorf("user dest = %q, want %q", got, payload)
	}
	text := out.String()
	if !strings.Contains(text, "is not writable") {
		t.Errorf("should say why it relocated:\n%s", text)
	}
	if !strings.Contains(text, "sudo ln -s") {
		t.Errorf("should tell the user how to point PATH at the new binary:\n%s", text)
	}
}

func TestUpdateRelocatesWhenAlreadyCurrent(t *testing.T) {
	// A first `sudo buidl update` leaves the new version in /usr/local/bin.
	// The next run without sudo must still move it, or sudo never goes away.
	if os.Geteuid() == 0 {
		t.Skip("root can write a 0555 directory")
	}
	payload := []byte("already-current-relocated")
	srv := startCLIReleaseServer(t, "v0.1.6", payload)

	locked := t.TempDir()
	dest := filepath.Join(locked, "buidl")
	if err := os.WriteFile(dest, []byte("old-system"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BUIDL_INSTALL_DIR", "")

	app, out := newTestApp(t, ui.ModePlain)
	app.opts.timeout = time.Minute
	app.updater = releaseClient(t, srv, "v0.1.6")
	app.updateDest = dest

	cmd := newUpdateCmd(app)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	old, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(old) != "old-system" {
		t.Errorf("unwritable dest was overwritten: %q", old)
	}

	got, err := os.ReadFile(filepath.Join(home, ".local", "bin", "buidl"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Errorf("user dest = %q, want %q", got, payload)
	}
	text := out.String()
	if !strings.Contains(text, "is not writable") {
		t.Errorf("should say why it relocated:\n%s", text)
	}
	if !strings.Contains(text, "sudo ln -s") {
		t.Errorf("should tell the user how to point PATH at the new binary:\n%s", text)
	}
}

func TestUpdateSkipsWhenCurrent(t *testing.T) {
	srv := startCLIReleaseServer(t, "v0.1.6", []byte("new"))
	dest := filepath.Join(t.TempDir(), "buidl")
	if err := os.WriteFile(dest, []byte("keep-me"), 0o755); err != nil {
		t.Fatal(err)
	}

	app, _ := newTestApp(t, ui.ModePlain)
	app.opts.timeout = time.Minute
	app.updater = releaseClient(t, srv, "v0.1.6")
	app.updateDest = dest

	cmd := newUpdateCmd(app)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "keep-me" {
		t.Errorf("replaced a current binary: %q", got)
	}
}

func TestUpdateNotice(t *testing.T) {
	app, out := newTestApp(t, ui.ModePlain)
	app.updateResult = make(chan update.Result, 1)
	app.updateResult <- update.Result{Current: "v0.1.6", Latest: "v0.1.7", Newer: true}
	app.maybeNotifyUpdate()

	got := out.String()
	if !strings.Contains(got, "v0.1.7") {
		t.Errorf("missing latest:\n%s", got)
	}
	if !strings.Contains(got, "buidl update") {
		t.Errorf("missing command:\n%s", got)
	}
	if !strings.Contains(got, "v0.1.6") {
		t.Errorf("missing current:\n%s", got)
	}
}

func TestUpdateNoticeSkippedForUpdateCommand(t *testing.T) {
	app, out := newTestApp(t, ui.ModePlain)
	app.skipUpdateNotice = true
	app.updateResult = make(chan update.Result, 1)
	app.updateResult <- update.Result{Current: "v0.1.6", Latest: "v0.1.7", Newer: true}
	app.maybeNotifyUpdate()
	if out.Len() != 0 {
		t.Errorf("update command should not nag:\n%s", out)
	}
}

func TestUpdateNoticeSkippedInJSON(t *testing.T) {
	app, out := newTestApp(t, ui.ModeJSON)
	app.updateResult = make(chan update.Result, 1)
	app.updateResult <- update.Result{Current: "v0.1.6", Latest: "v0.1.7", Newer: true}
	app.maybeNotifyUpdate()
	if strings.Contains(out.String(), "buidl update") {
		t.Errorf("JSON mode must not print a notice:\n%s", out)
	}
}

func TestUpdateNoticeSkippedWhenCurrent(t *testing.T) {
	app, out := newTestApp(t, ui.ModePlain)
	app.updateResult = make(chan update.Result, 1)
	app.updateResult <- update.Result{Current: "v0.1.6", Latest: "v0.1.6", Newer: false}
	app.maybeNotifyUpdate()
	if strings.Contains(out.String(), "buidl update") {
		t.Errorf("should stay quiet when current:\n%s", out)
	}
}

func TestStartUpdateCheckUsesCache(t *testing.T) {
	t.Setenv("BUIDL_NO_UPDATE_CHECK", "")
	t.Setenv("CI", "")
	t.Setenv("GITHUB_ACTIONS", "")

	old := Version
	Version = "v0.1.6"
	t.Cleanup(func() { Version = old })

	cache := filepath.Join(t.TempDir(), "update-check")
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	if err := os.WriteFile(cache, []byte("latest=v0.1.7\nchecked_at="+now.Format(time.RFC3339)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	app, out := newTestApp(t, ui.ModePlain)
	// newTestApp sets BUIDL_NO_UPDATE_CHECK; turn the check back on.
	t.Setenv("BUIDL_NO_UPDATE_CHECK", "")
	app.updater = &update.Client{
		BaseURL:   "http://127.0.0.1:1",
		Current:   "v0.1.6",
		CachePath: cache,
		Now:       func() time.Time { return now.Add(time.Hour) },
	}
	app.startUpdateCheck()
	app.maybeNotifyUpdate()

	if !strings.Contains(out.String(), "buidl update") {
		t.Errorf("cached newer tag should notify:\n%s", out)
	}
}

func TestUpdateCheckDisabledInCI(t *testing.T) {
	t.Setenv("BUIDL_NO_UPDATE_CHECK", "")
	t.Setenv("CI", "true")
	if !updateCheckDisabled() {
		t.Error("CI should disable the background check")
	}
}

func TestUpdateCheckDisabledByEnv(t *testing.T) {
	t.Setenv("CI", "")
	t.Setenv("GITHUB_ACTIONS", "")
	t.Setenv("BUIDL_NO_UPDATE_CHECK", "1")
	if !updateCheckDisabled() {
		t.Error("BUIDL_NO_UPDATE_CHECK=1 should disable the background check")
	}
}

func startCLIReleaseServer(t *testing.T, version string, payload []byte) string {
	t.Helper()
	if payload == nil {
		payload = []byte("payload")
	}
	asset := update.AssetName(runtime.GOOS, runtime.GOARCH)
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
	return srv.URL
}

func releaseClient(t *testing.T, base, current string) *update.Client {
	t.Helper()
	return &update.Client{
		BaseURL:   base,
		Current:   current,
		GOOS:      runtime.GOOS,
		GOARCH:    runtime.GOARCH,
		CachePath: filepath.Join(t.TempDir(), "update-check"),
		Now:       time.Now,
	}
}
