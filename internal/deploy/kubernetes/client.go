// Package kubernetes implements the deploy backend for Kubernetes clusters.
//
// Design decisions worth knowing before reading further:
//
//   - All writes go through server-side apply with a fixed field manager. buidl
//     therefore owns exactly the fields it sets and will not clobber fields set
//     by an HPA, a service mesh injector, or a human running kubectl. A
//     conflicting change surfaces as a real error rather than a silent revert.
//   - Objects are rendered as typed structs, converted to unstructured, and
//     applied through the dynamic client. One code path covers every kind.
//   - Nothing is applied until preflight passes and (for `plan`) a server-side
//     dry run has produced the diff.
package kubernetes

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/klog/v2"

	"github.com/danecwalker/buidl/internal/config"
	"github.com/danecwalker/buidl/internal/deploy"
)

// FieldManager identifies buidl's ownership in server-side apply. Changing this
// string would orphan every field buidl currently owns, so it is a constant and
// not configurable.
const FieldManager = "buidl"

func init() {
	deploy.Register("kubernetes", func(cfg *config.Config, log deploy.Logger) (deploy.Target, error) {
		return New(cfg, log)
	})

	// client-go logs transport failures through klog straight to stderr, which
	// interleaves "Unhandled Error" noise with buidl's own output and duplicates
	// errors that are already reported with actionable context. Route it to a
	// discard logger so buidl owns everything the user sees.
	klog.SetLogger(logr.Discard())
}

// Target is the Kubernetes deploy backend.
type Target struct {
	cfg *config.Config
	log deploy.Logger

	// Namespace is the resolved target namespace.
	Namespace string

	restConfig *rest.Config
	clientset  *kubernetes.Clientset
	dynamic    dynamic.Interface
	mapper     meta.RESTMapper
	discovery  discovery.DiscoveryInterface

	// contextName is the kubeconfig context in use, shown in output so a user
	// can never be unsure which cluster they just deployed to.
	contextName string
	// serverHost is the API server address, shown for the same reason.
	serverHost string
}

// New builds a Target and loads cluster credentials.
func New(cfg *config.Config, log deploy.Logger) (*Target, error) {
	t := &Target{
		cfg:       cfg,
		log:       log,
		Namespace: cfg.Deploy.Kubernetes.Namespace,
	}
	if err := t.connect(); err != nil {
		return nil, err
	}
	return t, nil
}

// NewRenderer builds a Target that can render manifests but cannot talk to a
// cluster.
//
// Rendering is pure — it reads the config and the release and touches no API
// server — so `buidl manifest | kubectl apply -f -`, whose whole point is not
// needing cluster access, must work on a machine with no kubeconfig. Making
// that an explicit constructor rather than a hand-built struct literal means
// the "no client needed" contract is stated here, next to the fields, instead
// of being an assumption the CLI silently depends on.
//
// Every client field is nil. Calling anything that reaches the API server on
// the result will panic, which is the correct outcome: it is a programming
// error, not a condition to handle at runtime.
func NewRenderer(cfg *config.Config, log deploy.Logger) *Target {
	ns := cfg.Deploy.Kubernetes.Namespace
	if ns == "" {
		// buildClients applies the same fallback for a connected Target; without
		// it a rendered manifest would carry an empty namespace.
		ns = "default"
	}
	return &Target{cfg: cfg, log: log, Namespace: ns}
}

// Name implements deploy.Target.
func (t *Target) Name() string { return "kubernetes" }

// connect loads a rest.Config from the environment.
//
// Resolution order is in-cluster first, then kubeconfig. In-cluster must win: a
// buidl running as a Pod (for example, a deploy job or an in-cluster agent) has
// a service account token that is more trustworthy than a stray kubeconfig in
// the image.
func (t *Target) connect() error {
	if cfg, err := rest.InClusterConfig(); err == nil {
		t.restConfig = cfg
		t.contextName = "in-cluster"
		t.serverHost = cfg.Host
		return t.buildClients()
	}

	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if path := os.Getenv("KUBECONFIG"); path == "" {
		if home, err := os.UserHomeDir(); err == nil {
			candidate := filepath.Join(home, ".kube", "config")
			if _, err := os.Stat(candidate); err == nil {
				rules.ExplicitPath = candidate
			}
		}
	}

	overrides := &clientcmd.ConfigOverrides{}
	// An explicit context in buidl.yaml is a guardrail: it makes deploying
	// production from a laptop pointed at the wrong cluster impossible.
	if t.cfg.Deploy.Kubernetes.Context != "" {
		overrides.CurrentContext = t.cfg.Deploy.Kubernetes.Context
	}

	clientConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides)

	raw, err := clientConfig.RawConfig()
	if err == nil {
		t.contextName = overrides.CurrentContext
		if t.contextName == "" {
			t.contextName = raw.CurrentContext
		}
		if t.contextName == "" {
			return fmt.Errorf("no kubeconfig context selected; set deploy.kubernetes.context in buidl.yaml or run `kubectl config use-context`")
		}
		if _, ok := raw.Contexts[t.contextName]; !ok {
			return fmt.Errorf("kubeconfig context %q not found (available: %s)", t.contextName, contextNames(raw.Contexts))
		}
	}

	restCfg, err := clientConfig.ClientConfig()
	if err != nil {
		return fmt.Errorf("loading kubernetes credentials: %w\n\nhint: is a kubeconfig present, or KUBECONFIG set?", err)
	}
	t.restConfig = restCfg
	t.serverHost = restCfg.Host

	// If the namespace was not set in config or derived from the app name, fall
	// back to the one the kubeconfig context selects.
	if t.Namespace == "" {
		if ns, _, err := clientConfig.Namespace(); err == nil {
			t.Namespace = ns
		}
	}

	return t.buildClients()
}

func (t *Target) buildClients() error {
	// A deploy makes many small API calls; the defaults are throttled low enough
	// to add noticeable latency to a rollout wait.
	t.restConfig.QPS = 50
	t.restConfig.Burst = 100
	t.restConfig.UserAgent = "buidl/" + version()
	if t.restConfig.Timeout == 0 {
		t.restConfig.Timeout = 60 * time.Second
	}

	cs, err := kubernetes.NewForConfig(t.restConfig)
	if err != nil {
		return fmt.Errorf("building kubernetes client: %w", err)
	}
	t.clientset = cs

	dyn, err := dynamic.NewForConfig(t.restConfig)
	if err != nil {
		return fmt.Errorf("building dynamic client: %w", err)
	}
	t.dynamic = dyn

	// The discovery cache matters: without it, every object we apply triggers a
	// fresh round of API discovery, which dominates deploy time.
	t.discovery = cs.Discovery()
	t.mapper = restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(cs.Discovery()))

	if t.Namespace == "" {
		t.Namespace = "default"
	}
	return nil
}

// Close implements deploy.Target. The clients hold no long-lived resources that
// require explicit release.
func (t *Target) Close() error { return nil }

// ContextName reports the cluster context in use.
func (t *Target) ContextName() string { return t.contextName }

// ServerHost reports the API server address.
func (t *Target) ServerHost() string { return t.serverHost }

// resourceFor maps a typed object's GroupVersionKind to the REST resource needed
// by the dynamic client.
func (t *Target) resourceFor(gvk schema.GroupVersionKind) (schema.GroupVersionResource, bool, error) {
	mapping, err := t.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		// This is the first call that touches the API server, so a cluster that is
		// down surfaces here. Wrapping keeps the message actionable instead of
		// reporting a REST-mapping failure for a well-known built-in kind.
		return schema.GroupVersionResource{}, false, t.wrapClusterError(err, fmt.Sprintf("resolving the API resource for %s", gvk.Kind))
	}
	namespaced := mapping.Scope.Name() == meta.RESTScopeNameNamespace
	return mapping.Resource, namespaced, nil
}

// toUnstructured converts a typed object for the dynamic client.
//
// The GVK must be set explicitly: typed objects from client-go have an empty
// TypeMeta, and server-side apply requires apiVersion and kind in the payload.
func toUnstructured(obj runtime.Object, gvk schema.GroupVersionKind) (*unstructured.Unstructured, error) {
	data, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return nil, fmt.Errorf("converting %s: %w", gvk.Kind, err)
	}
	u := &unstructured.Unstructured{Object: data}
	u.SetGroupVersionKind(gvk)

	// creationTimestamp serializes as "null" from a zero time.Time, which the
	// apply endpoint rejects as an invalid value.
	unstructured.RemoveNestedField(u.Object, "metadata", "creationTimestamp")
	unstructured.RemoveNestedField(u.Object, "spec", "template", "metadata", "creationTimestamp")
	unstructured.RemoveNestedField(u.Object, "status")
	stripVolumeClaimTemplateNoise(u)

	return u, nil
}

// stripVolumeClaimTemplateNoise removes the same server-owned fields from a
// StatefulSet's volumeClaimTemplates.
//
// Each template is a full PersistentVolumeClaim, so it carries its own null
// creationTimestamp and an empty status — the identical problem the top-level
// strip above solves, one level down where the top-level strip cannot reach.
func stripVolumeClaimTemplateNoise(u *unstructured.Unstructured) {
	templates, found, err := unstructured.NestedSlice(u.Object, "spec", "volumeClaimTemplates")
	if err != nil || !found {
		return
	}
	for _, entry := range templates {
		claim, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		unstructured.RemoveNestedField(claim, "metadata", "creationTimestamp")
		unstructured.RemoveNestedField(claim, "status")
	}
	_ = unstructured.SetNestedSlice(u.Object, templates, "spec", "volumeClaimTemplates")
}

func contextNames(contexts map[string]*clientcmdapi.Context) string {
	names := make([]string, 0, len(contexts))
	for name := range contexts {
		names = append(names, name)
	}
	return strings.Join(names, ", ")
}

// applyOptions returns the options used for every write.
func applyOptions(dryRun bool) metav1.ApplyOptions {
	opts := metav1.ApplyOptions{
		FieldManager: FieldManager,
		// Force resolves conflicts in buidl's favor for fields buidl declares.
		// Without it, any field previously set by kubectl apply would block the
		// deploy with a conflict the user cannot easily resolve.
		Force: true,
	}
	if dryRun {
		opts.DryRun = []string{metav1.DryRunAll}
	}
	return opts
}

// version reports the buidl version for the user agent. Overridden at link time.
var buildVersion = "dev"

func version() string { return buildVersion }

// wrapClusterError adds actionable context to API errors.
//
// A bare "connection refused" tells the user nothing about which cluster was
// tried or what to do next, and it is the single most common failure when a
// kubeconfig points at a stopped local cluster.
func (t *Target) wrapClusterError(err error, doing string) error {
	if err == nil {
		return nil
	}
	if isNotFound(err) || isConflict(err) || apierrors.IsForbidden(err) {
		// These already carry a precise, actionable message from the server.
		return err
	}

	msg := err.Error()
	switch {
	case strings.Contains(msg, "connection refused"),
		strings.Contains(msg, "no such host"),
		strings.Contains(msg, "i/o timeout"),
		strings.Contains(msg, "TLS handshake timeout"):
		return fmt.Errorf("cannot reach the cluster while %s\n\n  context: %s\n  server:  %s\n\nhint: is the cluster running, and is the right context selected? try `kubectl config get-contexts`",
			doing, t.contextName, t.serverHost)
	case apierrors.IsUnauthorized(err):
		return fmt.Errorf("not authorized to reach the cluster while %s (context %s)\n\nhint: your credentials may have expired; re-authenticate and retry", doing, t.contextName)
	}
	return fmt.Errorf("%s: %w", doing, err)
}

// namespaceExists reports whether the target namespace is present.
func (t *Target) namespaceExists(ctx context.Context) (bool, error) {
	_, err := t.clientset.CoreV1().Namespaces().Get(ctx, t.Namespace, metav1.GetOptions{})
	if err == nil {
		return true, nil
	}
	if isNotFound(err) {
		return false, nil
	}
	return false, err
}
