package kubernetes

import (
	"os"
	"path/filepath"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// writeConfig creates a buidl.yaml in a temp dir for rendering tests.
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "buidl.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	return path
}

// unstructuredNestedFieldNoCopy wraps the upstream helper so tests read clearly.
func unstructuredNestedFieldNoCopy(obj map[string]any, fields ...string) (any, bool, error) {
	return unstructured.NestedFieldNoCopy(obj, fields...)
}
