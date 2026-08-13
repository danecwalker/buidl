package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danecwalker/buidl/internal/ui"
)

// TestManifestNeedsNoClusterCredentials covers the documented GitOps use,
// `buidl manifest -e production | kubectl apply -f -`, whose whole point is not
// needing cluster access. Rendering is pure, but the command used to construct a
// deploy target first, so it failed on any machine without a kubeconfig — and on
// one with the wrong kubeconfig it failed for a reason that had nothing to do
// with the output it was asked for.
func TestManifestNeedsNoClusterCredentials(t *testing.T) {
	path := writeTempConfig(t, `
app: web
image: ghcr.io/acme/web
environments:
  production: {}
`)

	// No kubeconfig anywhere: neither KUBECONFIG nor the home-directory default
	// resolves to a file.
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "absent.yaml"))
	t.Setenv("HOME", t.TempDir())

	app, _ := newTestApp(t, ui.ModePlain)
	app.opts.configPath = path
	app.opts.environment = "production"

	out := &bytes.Buffer{}
	cmd := newManifestCmd(app)
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetArgs(nil)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("manifest: %v", err)
	}

	got := out.String()
	// The rendered stream must be complete, not a stub: this output is what gets
	// committed or piped to kubectl.
	for _, want := range []string{"kind: Deployment", "kind: Service", "name: web", "namespace: web"} {
		if !strings.Contains(got, want) {
			t.Errorf("manifest output is missing %q:\n%s", want, got)
		}
	}
	// The placeholder digest keeps the command usable before any image exists.
	if !strings.Contains(got, "ghcr.io/acme/web@sha256:") {
		t.Errorf("manifest output does not pin the image by digest:\n%s", got)
	}
}
