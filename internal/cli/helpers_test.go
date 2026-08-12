package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeTempConfig writes a buidl.yaml into a temp dir and returns its path.
func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "buidl.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	return path
}

// secondsDuration converts seconds to a Duration, keeping table tests readable.
func secondsDuration(s int) time.Duration {
	return time.Duration(s) * time.Second
}
