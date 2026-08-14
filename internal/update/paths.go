package update

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// UserBinDir is where a user-owned buidl lives. Updates write here so they
// do not need sudo. Matches install.sh's default dest.
func UserBinDir() (string, error) {
	if d := strings.TrimSpace(os.Getenv("BUIDL_INSTALL_DIR")); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}
	return filepath.Join(home, ".local", "bin"), nil
}

// UserInstallPath is a writable dest that is not current. BUIDL_INSTALL_DIR
// is tried first, then ~/.local/bin, so an env pointing at the unwritable
// current path still relocates.
func UserInstallPath(current string) (string, error) {
	var dirs []string
	if d := strings.TrimSpace(os.Getenv("BUIDL_INSTALL_DIR")); d != "" {
		dirs = append(dirs, d)
	}
	home, err := os.UserHomeDir()
	if err != nil && len(dirs) == 0 {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}
	if err == nil {
		dirs = append(dirs, filepath.Join(home, ".local", "bin"))
	}

	seen := map[string]bool{}
	for _, dir := range dirs {
		dest := filepath.Join(dir, "buidl")
		if current != "" && sameFile(current, dest) {
			continue
		}
		if seen[dest] {
			continue
		}
		seen[dest] = true
		if CanReplace(dest) {
			return dest, nil
		}
	}
	return "", fmt.Errorf("no writable install directory besides %s", current)
}

// CanReplace reports whether dest can be created or atomically replaced
// by this user. On Unix that is write access to the directory, not the file.
func CanReplace(dest string) bool {
	if dest == "" {
		return false
	}
	dir := filepath.Dir(dest)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false
	}
	f, err := os.CreateTemp(dir, ".buidl-write-")
	if err != nil {
		return false
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return true
}

func sameFile(a, b string) bool {
	aa, err := filepath.Abs(a)
	if err != nil {
		return a == b
	}
	bb, err := filepath.Abs(b)
	if err != nil {
		return a == b
	}
	return aa == bb
}
