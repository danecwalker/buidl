package kubernetes

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/danecwalker/buidl/internal/config"
	"github.com/danecwalker/buidl/internal/deploy"
	"github.com/danecwalker/buidl/internal/release"
)

func TestDestroyDeletesEphemeralNamespace(t *testing.T) {
	nsName := "web-preview-pr-12"
	cfg := previewConfig(t, nsName, true)
	tgt := destroyTarget(cfg, ownedNamespace(nsName, "web", "preview"),
		ownedDeployment(nsName, "web", "preview"))

	out, err := tgt.Destroy(context.Background(), deploy.DestroyRequest{
		Config: cfg,
		Slug:   "pr-12",
	})
	if err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if out.Mode != deploy.DestroyModeNamespace {
		t.Fatalf("Mode = %s, want namespace", out.Mode)
	}
	_, err = tgt.clientset.CoreV1().Namespaces().Get(context.Background(), nsName, metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("namespace still present: %v", err)
	}
}

func TestDestroyIsIdempotentWhenAlreadyGone(t *testing.T) {
	cfg := previewConfig(t, "web-preview-pr-12", true)
	tgt := destroyTarget(cfg)

	out, err := tgt.Destroy(context.Background(), deploy.DestroyRequest{Config: cfg, Slug: "pr-12"})
	if err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if out.Mode != deploy.DestroyModeNone {
		t.Errorf("Mode = %s, want none", out.Mode)
	}
}

func TestDestroyRefusesUnownedNamespace(t *testing.T) {
	nsName := "web-preview-pr-12"
	cfg := previewConfig(t, nsName, true)
	tgt := destroyTarget(cfg, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}})

	_, err := tgt.Destroy(context.Background(), deploy.DestroyRequest{Config: cfg, Slug: "pr-12"})
	if err == nil {
		t.Fatal("expected refusal to delete an unowned namespace")
	}
	if want := "not managed by buidl"; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q, want %q", err, want)
	}
	// Still there.
	if _, err := tgt.clientset.CoreV1().Namespaces().Get(context.Background(), nsName, metav1.GetOptions{}); err != nil {
		t.Fatalf("unowned namespace was deleted: %v", err)
	}
}

func TestDestroyObjectsLeavesAccessories(t *testing.T) {
	nsName := "web-staging"
	cfg := testConfig(t, `
app: web
image: ghcr.io/acme/web
deploy:
  kubernetes:
    namespace: web-staging
    createNamespace: true
`)
	cfg.Environment = "staging"

	tgt := destroyTarget(cfg,
		ownedNamespace(nsName, "web", "staging"),
		ownedDeployment(nsName, "web", "staging"),
		ownedService(nsName, "web", "staging"),
		accessoryStatefulSet(nsName, "web", "staging"),
	)

	out, err := tgt.Destroy(context.Background(), deploy.DestroyRequest{Config: cfg})
	if err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if out.Mode != deploy.DestroyModeObjects {
		t.Fatalf("Mode = %s, want objects", out.Mode)
	}

	if _, err := tgt.clientset.CoreV1().Namespaces().Get(context.Background(), nsName, metav1.GetOptions{}); err != nil {
		t.Fatalf("staging namespace was deleted: %v", err)
	}
	if _, err := tgt.clientset.AppsV1().Deployments(nsName).Get(context.Background(), "web", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("app deployment still present: %v", err)
	}
	if _, err := tgt.clientset.AppsV1().StatefulSets(nsName).Get(context.Background(), "web-postgres", metav1.GetOptions{}); err != nil {
		t.Fatalf("accessory was deleted: %v", err)
	}
}

func TestDestroyDryRunDoesNotDelete(t *testing.T) {
	nsName := "web-preview-pr-12"
	cfg := previewConfig(t, nsName, true)
	tgt := destroyTarget(cfg, ownedNamespace(nsName, "web", "preview"))

	out, err := tgt.Destroy(context.Background(), deploy.DestroyRequest{
		Config: cfg, Slug: "pr-12", DryRun: true,
	})
	if err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if out.Mode != deploy.DestroyModeNamespace || len(out.Changes) != 1 {
		t.Fatalf("dry-run outcome = %+v", out)
	}
	if _, err := tgt.clientset.CoreV1().Namespaces().Get(context.Background(), nsName, metav1.GetOptions{}); err != nil {
		t.Fatalf("dry-run deleted the namespace: %v", err)
	}
}

func TestDestroyStaleSkipsFreshNamespaces(t *testing.T) {
	cfg := previewConfig(t, "web-preview-pr-99", true)
	old := ownedNamespace("web-preview-pr-1", "web", "preview")
	old.CreationTimestamp = metav1.NewTime(time.Now().Add(-10 * 24 * time.Hour))
	fresh := ownedNamespace("web-preview-pr-2", "web", "preview")
	fresh.CreationTimestamp = metav1.NewTime(time.Now().Add(-1 * time.Hour))

	tgt := destroyTarget(cfg, old, fresh)
	// The current slug's namespace is unused for a stale sweep.
	tgt.Namespace = "web-preview-pr-99"

	out, err := tgt.Destroy(context.Background(), deploy.DestroyRequest{
		Config:     cfg,
		StaleAfter: 7 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if out.Mode != deploy.DestroyModeStale {
		t.Fatalf("Mode = %s, want stale", out.Mode)
	}
	if len(out.Namespaces) != 1 || out.Namespaces[0] != "web-preview-pr-1" {
		t.Fatalf("Namespaces = %v, want [web-preview-pr-1]", out.Namespaces)
	}
	if _, err := tgt.clientset.CoreV1().Namespaces().Get(context.Background(), "web-preview-pr-1", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("stale namespace still present: %v", err)
	}
	if _, err := tgt.clientset.CoreV1().Namespaces().Get(context.Background(), "web-preview-pr-2", metav1.GetOptions{}); err != nil {
		t.Fatalf("fresh namespace was deleted: %v", err)
	}
}

func TestDestroyStaleRefusedOnStaging(t *testing.T) {
	cfg := testConfig(t, `
app: web
image: ghcr.io/acme/web
`)
	cfg.Environment = "staging"
	tgt := destroyTarget(cfg)

	_, err := tgt.Destroy(context.Background(), deploy.DestroyRequest{
		Config:     cfg,
		StaleAfter: 7 * 24 * time.Hour,
	})
	if err == nil {
		t.Fatal("expected --stale on staging to fail")
	}
}

func TestEphemeralNamespaceGetsLabel(t *testing.T) {
	target, req := testRequest(t, `
app: web
image: ghcr.io/acme/web
deploy:
  kubernetes:
    namespace: web-preview-pr-12
    createNamespace: true
    ephemeral: true
`)
	req.Config.Environment = "preview"
	req.Release.Environment = "preview"
	objs, err := target.Render(req)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	ns := findObject(objs, "Namespace")
	if ns == nil {
		t.Fatal("expected Namespace")
	}
	labels := ns.Object.(*corev1.Namespace).Labels
	if labels[release.LabelEphemeral] != "true" {
		t.Errorf("ephemeral label = %q", labels[release.LabelEphemeral])
	}
}

func previewConfig(t *testing.T, ns string, ephemeral bool) *config.Config {
	t.Helper()
	flag := "false"
	if ephemeral {
		flag = "true"
	}
	cfg := testConfig(t, `
app: web
image: ghcr.io/acme/web
deploy:
  kubernetes:
    namespace: `+ns+`
    createNamespace: true
    ephemeral: `+flag+`
`)
	cfg.Environment = "preview"
	return cfg
}

func destroyTarget(cfg *config.Config, objects ...runtime.Object) *Target {
	return &Target{
		cfg:       cfg,
		log:       testLogger{},
		Namespace: cfg.Deploy.Kubernetes.Namespace,
		clientset: fake.NewSimpleClientset(objects...),
	}
}

func ownedNamespace(name, app, env string) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				release.LabelManagedBy: release.ManagedBy,
				release.LabelName:      app,
				release.LabelEnv:       env,
			},
		},
	}
}

func ownedDeployment(ns, app, env string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      app,
			Namespace: ns,
			Labels: map[string]string{
				release.LabelManagedBy: release.ManagedBy,
				release.LabelName:      app,
				release.LabelEnv:       env,
			},
		},
	}
}

func ownedService(ns, app, env string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      app,
			Namespace: ns,
			Labels: map[string]string{
				release.LabelManagedBy: release.ManagedBy,
				release.LabelName:      app,
				release.LabelEnv:       env,
			},
		},
	}
}

func accessoryStatefulSet(ns, app, env string) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      app + "-postgres",
			Namespace: ns,
			Labels: map[string]string{
				release.LabelManagedBy: release.ManagedBy,
				release.LabelName:      app + "-postgres",
				release.LabelEnv:       env,
				release.LabelComponent: "accessory",
			},
		},
	}
}
