package kubernetes

import (
	"context"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/danecwalker/buidl/internal/config"
	"github.com/danecwalker/buidl/internal/deploy"
	"github.com/danecwalker/buidl/internal/release"
)

// namespaceWait is how long we wait for a namespace to finish terminating.
//
// Delete returns as soon as the API accepts the request; the namespace then
// sits in Terminating until every object (and every finalizer) is gone. Two
// minutes covers a normal drain. A stuck finalizer is reported rather than
// waited out to the command timeout, because the user has to intervene.
const namespaceWait = 2 * time.Minute

// Destroy implements deploy.Target.
func (t *Target) Destroy(ctx context.Context, req deploy.DestroyRequest) (*deploy.DestroyOutcome, error) {
	cfg := req.Config
	out := &deploy.DestroyOutcome{
		Environment: cfg.Environment,
		Namespace:   t.Namespace,
	}

	if req.StaleAfter > 0 {
		if !deploy.IsEphemeral(cfg) && !config.PreviewLike(cfg.Environment) {
			return nil, fmt.Errorf("--stale only applies to ephemeral preview environments, not %s", cfg.Environment)
		}
		return t.destroyStale(ctx, req, out)
	}

	decision := deploy.DecideDestroy(cfg, req.Slug)
	if decision.Scope == deploy.ScopeRefused {
		return nil, fmt.Errorf("%s", decision.Reason)
	}

	exists, err := t.namespaceExists(ctx)
	if err != nil {
		return nil, t.wrapClusterError(err, "checking namespace "+t.Namespace)
	}
	if !exists {
		out.Mode = deploy.DestroyModeNone
		t.log.Detail("namespace %s is already gone", t.Namespace)
		return out, nil
	}

	if decision.Scope == deploy.ScopeNamespace {
		return t.destroyNamespace(ctx, req, out)
	}
	return t.destroyObjects(ctx, req, out)
}

// destroyNamespace deletes the preview namespace wholesale.
func (t *Target) destroyNamespace(ctx context.Context, req deploy.DestroyRequest, out *deploy.DestroyOutcome) (*deploy.DestroyOutcome, error) {
	ns, err := t.clientset.CoreV1().Namespaces().Get(ctx, t.Namespace, metav1.GetOptions{})
	if err != nil {
		if isNotFound(err) {
			out.Mode = deploy.DestroyModeNone
			return out, nil
		}
		return nil, t.wrapClusterError(err, "reading namespace "+t.Namespace)
	}

	if err := assertOwnedNamespace(ns, req.Config); err != nil {
		return nil, err
	}

	change := deploy.Change{
		Action:  deploy.ActionDelete,
		Kind:    "Namespace",
		Name:    t.Namespace,
		Summary: fmt.Sprintf("delete namespace/%s", t.Namespace),
		Impact:  "removes the preview app",
	}
	out.Mode = deploy.DestroyModeNamespace
	out.Namespaces = []string{t.Namespace}

	if ns.Status.Phase == corev1.NamespaceTerminating {
		t.log.Detail("namespace %s is already terminating", t.Namespace)
		if req.DryRun {
			out.Changes = []deploy.Change{change}
			return out, nil
		}
		if err := t.waitNamespaceGone(ctx, t.Namespace); err != nil {
			return out, err
		}
		change.Applied = true
		out.Changes = []deploy.Change{change}
		return out, nil
	}

	if req.DryRun {
		out.Changes = []deploy.Change{change}
		return out, nil
	}

	t.log.Step("Deleting namespace")
	if err := t.clientset.CoreV1().Namespaces().Delete(ctx, t.Namespace, metav1.DeleteOptions{}); err != nil && !isNotFound(err) {
		return nil, fmt.Errorf("deleting namespace %s: %w", t.Namespace, err)
	}
	if err := t.waitNamespaceGone(ctx, t.Namespace); err != nil {
		return out, err
	}
	change.Applied = true
	out.Changes = []deploy.Change{change}
	t.log.Info("deleted namespace/%s", t.Namespace)
	return out, nil
}

// destroyObjects deletes buidl-managed app objects and leaves accessories
// and the namespace alone.
func (t *Target) destroyObjects(ctx context.Context, req deploy.DestroyRequest, out *deploy.DestroyOutcome) (*deploy.DestroyOutcome, error) {
	out.Mode = deploy.DestroyModeObjects
	selector := appSelector(req.Config)

	listed, err := t.listManaged(ctx, selector)
	if err != nil {
		return nil, err
	}
	if len(listed) == 0 {
		out.Mode = deploy.DestroyModeNone
		t.log.Detail("no managed objects in namespace %s", t.Namespace)
		return out, nil
	}

	for _, obj := range listed {
		change := deploy.Change{
			Action:  deploy.ActionDelete,
			Kind:    obj.kind,
			Name:    obj.name,
			Summary: fmt.Sprintf("delete %s/%s", obj.kind, obj.name),
			Impact:  "stops serving",
		}
		if req.DryRun {
			out.Changes = append(out.Changes, change)
			continue
		}
		if err := obj.del(ctx); err != nil && !isNotFound(err) {
			change.Err = err
			out.Changes = append(out.Changes, change)
			return out, fmt.Errorf("deleting %s/%s: %w", obj.kind, obj.name, err)
		}
		change.Applied = true
		out.Changes = append(out.Changes, change)
		t.log.Info("deleted %s/%s", obj.kind, obj.name)
	}
	return out, nil
}

// destroyStale deletes ephemeral namespaces older than the threshold.
func (t *Target) destroyStale(ctx context.Context, req deploy.DestroyRequest, out *deploy.DestroyOutcome) (*deploy.DestroyOutcome, error) {
	out.Mode = deploy.DestroyModeStale
	selector := fmt.Sprintf("%s=%s,%s=%s,%s=%s",
		release.LabelManagedBy, release.ManagedBy,
		release.LabelName, req.Config.App,
		release.LabelEnv, req.Config.Environment,
	)

	list, err := t.clientset.CoreV1().Namespaces().List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, t.wrapClusterError(err, "listing preview namespaces")
	}

	cutoff := time.Now().Add(-req.StaleAfter)
	var stale []corev1.Namespace
	for _, ns := range list.Items {
		if config.ProtectedNamespace(ns.Name) {
			continue
		}
		if err := assertOwnedNamespace(&ns, req.Config); err != nil {
			t.log.Detail("skipping %s: %v", ns.Name, err)
			continue
		}
		if ns.CreationTimestamp.Time.After(cutoff) {
			continue
		}
		stale = append(stale, ns)
	}
	sort.Slice(stale, func(i, j int) bool {
		return stale[i].Name < stale[j].Name
	})

	if len(stale) == 0 {
		out.Mode = deploy.DestroyModeNone
		t.log.Detail("no preview namespaces older than %s", req.StaleAfter)
		return out, nil
	}

	for _, ns := range stale {
		change := deploy.Change{
			Action:  deploy.ActionDelete,
			Kind:    "Namespace",
			Name:    ns.Name,
			Summary: fmt.Sprintf("delete namespace/%s", ns.Name),
			Impact:  "removes a stale preview",
		}
		out.Namespaces = append(out.Namespaces, ns.Name)
		if req.DryRun {
			out.Changes = append(out.Changes, change)
			continue
		}
		if err := t.clientset.CoreV1().Namespaces().Delete(ctx, ns.Name, metav1.DeleteOptions{}); err != nil && !isNotFound(err) {
			change.Err = err
			out.Changes = append(out.Changes, change)
			return out, fmt.Errorf("deleting namespace %s: %w", ns.Name, err)
		}
		if err := t.waitNamespaceGone(ctx, ns.Name); err != nil {
			return out, err
		}
		change.Applied = true
		out.Changes = append(out.Changes, change)
		t.log.Info("deleted namespace/%s", ns.Name)
	}
	return out, nil
}

// assertOwnedNamespace refuses to delete a namespace buidl did not create.
//
// A name collision with a hand-made namespace (or one belonging to a different
// app) would otherwise take someone else's workloads with it.
func assertOwnedNamespace(ns *corev1.Namespace, cfg *config.Config) error {
	labels := ns.Labels
	if labels[release.LabelManagedBy] != release.ManagedBy {
		return fmt.Errorf("namespace %q exists but is not managed by buidl\n\nhint: refusing to delete a namespace we did not create", ns.Name)
	}
	if name := labels[release.LabelName]; name != "" && name != cfg.App {
		return fmt.Errorf("namespace %q is managed by buidl but belongs to app %q, not %q", ns.Name, name, cfg.App)
	}
	if env := labels[release.LabelEnv]; env != "" && env != cfg.Environment {
		return fmt.Errorf("namespace %q is for environment %q, not %q", ns.Name, env, cfg.Environment)
	}
	return nil
}

func appSelector(cfg *config.Config) string {
	return fmt.Sprintf("%s=%s,%s=%s,%s=%s",
		release.LabelManagedBy, release.ManagedBy,
		release.LabelName, cfg.App,
		release.LabelEnv, cfg.Environment,
	)
}

func (t *Target) waitNamespaceGone(ctx context.Context, name string) error {
	err := wait.PollUntilContextTimeout(ctx, time.Second, namespaceWait, true, func(ctx context.Context) (bool, error) {
		_, err := t.clientset.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
		if isNotFound(err) {
			return true, nil
		}
		return false, err
	})
	if err == nil {
		return nil
	}
	// The parent context dying (Ctrl+C, --timeout) is not a stuck finalizer.
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if wait.Interrupted(err) {
		return fmt.Errorf("namespace %s is still terminating after %s\n\nhint: a finalizer may be stuck; inspect with `kubectl get ns %s -o yaml`", name, namespaceWait, name)
	}
	return fmt.Errorf("waiting for namespace %s to go away: %w", name, err)
}

// managedObject is one namespaced object we are willing to delete.
type managedObject struct {
	kind string
	name string
	del  func(context.Context) error
}

func (t *Target) listManaged(ctx context.Context, selector string) ([]managedObject, error) {
	var out []managedObject
	ns := t.Namespace

	add := func(kind string, names []string, del func(string) func(context.Context) error) {
		sort.Strings(names)
		for _, name := range names {
			out = append(out, managedObject{kind: kind, name: name, del: del(name)})
		}
	}

	ings, err := t.clientset.NetworkingV1().Ingresses(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("listing Ingresses: %w", err)
	}
	add("Ingress", metaNames(len(ings.Items), func(i int) string { return ings.Items[i].Name }), func(name string) func(context.Context) error {
		return func(ctx context.Context) error {
			return t.clientset.NetworkingV1().Ingresses(ns).Delete(ctx, name, metav1.DeleteOptions{})
		}
	})

	hpas, err := t.clientset.AutoscalingV2().HorizontalPodAutoscalers(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("listing HorizontalPodAutoscalers: %w", err)
	}
	add("HorizontalPodAutoscaler", metaNames(len(hpas.Items), func(i int) string { return hpas.Items[i].Name }), func(name string) func(context.Context) error {
		return func(ctx context.Context) error {
			return t.clientset.AutoscalingV2().HorizontalPodAutoscalers(ns).Delete(ctx, name, metav1.DeleteOptions{})
		}
	})

	pdbs, err := t.clientset.PolicyV1().PodDisruptionBudgets(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("listing PodDisruptionBudgets: %w", err)
	}
	add("PodDisruptionBudget", metaNames(len(pdbs.Items), func(i int) string { return pdbs.Items[i].Name }), func(name string) func(context.Context) error {
		return func(ctx context.Context) error {
			return t.clientset.PolicyV1().PodDisruptionBudgets(ns).Delete(ctx, name, metav1.DeleteOptions{})
		}
	})

	deps, err := t.clientset.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("listing Deployments: %w", err)
	}
	add("Deployment", metaNames(len(deps.Items), func(i int) string { return deps.Items[i].Name }), func(name string) func(context.Context) error {
		return func(ctx context.Context) error {
			return t.clientset.AppsV1().Deployments(ns).Delete(ctx, name, metav1.DeleteOptions{})
		}
	})

	svcs, err := t.clientset.CoreV1().Services(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("listing Services: %w", err)
	}
	add("Service", metaNames(len(svcs.Items), func(i int) string { return svcs.Items[i].Name }), func(name string) func(context.Context) error {
		return func(ctx context.Context) error {
			return t.clientset.CoreV1().Services(ns).Delete(ctx, name, metav1.DeleteOptions{})
		}
	})

	secs, err := t.clientset.CoreV1().Secrets(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("listing Secrets: %w", err)
	}
	add("Secret", metaNames(len(secs.Items), func(i int) string { return secs.Items[i].Name }), func(name string) func(context.Context) error {
		return func(ctx context.Context) error {
			return t.clientset.CoreV1().Secrets(ns).Delete(ctx, name, metav1.DeleteOptions{})
		}
	})

	sas, err := t.clientset.CoreV1().ServiceAccounts(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("listing ServiceAccounts: %w", err)
	}
	add("ServiceAccount", metaNames(len(sas.Items), func(i int) string { return sas.Items[i].Name }), func(name string) func(context.Context) error {
		return func(ctx context.Context) error {
			return t.clientset.CoreV1().ServiceAccounts(ns).Delete(ctx, name, metav1.DeleteOptions{})
		}
	})

	return out, nil
}

func metaNames(n int, name func(int) string) []string {
	out := make([]string, n)
	for i := 0; i < n; i++ {
		out[i] = name(i)
	}
	return out
}
