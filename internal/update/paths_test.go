package update

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUserBinDirDefault(t *testing.T) {
	t.Setenv("BUIDL_INSTALL_DIR", "")
	t.Setenv("HOME", "/tmp/buidl-home")
	// UserHomeDir on Unix reads HOME.
	got, err := UserBinDir()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join("/tmp/buidl-home", ".local", "bin")
	if got != want {
		t.Errorf("UserBinDir = %q, want %q", got, want)
	}
}

func TestUserBinDirHonoursInstallDir(t *testing.T) {
	t.Setenv("BUIDL_INSTALL_DIR", "/opt/buidl/bin")
	got, err := UserBinDir()
	if err != nil {
		t.Fatal(err)
	}
	if got != "/opt/buidl/bin" {
		t.Errorf("UserBinDir = %q, want /opt/buidl/bin", got)
	}
}

func TestCanReplace(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "buidl")
	if !CanReplace(dest) {
		t.Fatal("expected a temp dir to be writable")
	}
}

func TestCanReplaceDenied(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write a 0555 directory")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if CanReplace(filepath.Join(dir, "buidl")) {
		t.Fatal("0555 directory must not look writable")
	}
}

func TestUserInstallPathSkipsCurrent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BUIDL_INSTALL_DIR", "")
	current := filepath.Join(home, ".local", "bin", "buidl")
	if _, err := UserInstallPath(current); err == nil {
		t.Fatal("expected an error when the user dest is the current dest")
	}
}

func TestUserInstallPathFindsHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BUIDL_INSTALL_DIR", "")
	got, err := UserInstallPath("/usr/local/bin/buidl")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".local", "bin", "buidl")
	if got != want {
		t.Errorf("UserInstallPath = %q, want %q", got, want)
	}
}

func TestUserInstallPathSkipsUnwritableEnv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BUIDL_INSTALL_DIR", "/usr/local/bin")
	got, err := UserInstallPath("/usr/local/bin/buidl")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".local", "bin", "buidl")
	if got != want {
		t.Errorf("UserInstallPath = %q, want %q", got, want)
	}
}
