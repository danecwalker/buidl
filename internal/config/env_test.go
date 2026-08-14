package config

import (
	"strings"
	"testing"
)

func TestProductionLike(t *testing.T) {
	for _, env := range []string{"production", "prod", "live", "main", "Production"} {
		if !ProductionLike(env) {
			t.Errorf("%q should be production-like", env)
		}
	}
	for _, env := range []string{"staging", "preview", "dev", "test"} {
		if ProductionLike(env) {
			t.Errorf("%q should not be production-like", env)
		}
	}
}

func TestPreviewLike(t *testing.T) {
	for _, env := range []string{"preview", "review", "pr"} {
		if !PreviewLike(env) {
			t.Errorf("%q should be preview-like", env)
		}
	}
	if PreviewLike("staging") {
		t.Error("staging is not a preview environment")
	}
}

func TestProtectedNamespace(t *testing.T) {
	for _, ns := range []string{"default", "kube-system", "cert-manager", "buidl-system"} {
		if !ProtectedNamespace(ns) {
			t.Errorf("%q should be protected", ns)
		}
	}
	if ProtectedNamespace("web-preview-pr-12") {
		t.Error("a preview namespace is not protected")
	}
}

func TestEphemeralRejectedOnProduction(t *testing.T) {
	_, err := Load(LoadOptions{
		Path: write(t, `
app: web
image: ghcr.io/acme/web
environments:
  production:
    deploy:
      kubernetes:
        ephemeral: true
`),
		Environment: "production",
		Strict:      true,
	})
	if err == nil {
		t.Fatal("expected ephemeral production to be rejected")
	}
	if want := "ephemeral"; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q, want it to mention %q", err, want)
	}
}

func TestEphemeralRejectedOnProtectedNamespace(t *testing.T) {
	_, err := Load(LoadOptions{
		Path: write(t, `
app: web
image: ghcr.io/acme/web
deploy:
  kubernetes:
    namespace: default
    ephemeral: true
`),
		Strict: true,
	})
	if err == nil {
		t.Fatal("expected ephemeral default namespace to be rejected")
	}
	if want := "protected namespace"; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q, want it to mention %q", err, want)
	}
}
