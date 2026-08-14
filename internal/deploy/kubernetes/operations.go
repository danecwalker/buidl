package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"

	"github.com/danecwalker/buidl/internal/build"
	"github.com/danecwalker/buidl/internal/config"
	"github.com/danecwalker/buidl/internal/deploy"
	"github.com/danecwalker/buidl/internal/release"
)

// Preflight validates that a deploy can succeed before mutating anything.
//
// Every check here is one that would otherwise fail partway through a rollout,
// when the cluster is already in a mixed state and the fix is more disruptive.
func (t *Target) Preflight(ctx context.Context, req deploy.Request) error {
	cfg := req.Config

	// 1. Is the cluster reachable, and which one is it?
	ver, err := t.discovery.ServerVersion()
	if err != nil {
		return t.wrapClusterError(err, "checking cluster connectivity")
	}
	t.log.Detail("cluster %s (kubernetes %s) namespace %s", t.contextName, ver.GitVersion, t.Namespace)

	// 2. Does the namespace exist, or are we allowed to create it?
	exists, err := t.namespaceExists(ctx)
	if err != nil {
		return fmt.Errorf("checking namespace %s: %w", t.Namespace, err)
	}
	if !exists && !cfg.Deploy.Kubernetes.CreateNamespace {
		return fmt.Errorf("namespace %q does not exist\n\nhint: set deploy.kubernetes.createNamespace: true, or run `kubectl create namespace %s`", t.Namespace, t.Namespace)
	}

	// 3. Do we have permission to write what we are about to write? Failing here
	//    is far better than failing after the Secret is applied but before the
	//    Deployment is.
	if err := t.checkPermissions(ctx, req); err != nil {
		return err
	}

	// 4. Are all declared secrets actually present? A missing secret otherwise
	//    manifests as CreateContainerConfigError minutes later. Accessory
	//    names (POSTGRES_PASSWORD) are required even when they are not on
	//    the app's env.secret.
	var missing []string
	for _, name := range cfg.SecretNames() {
		if _, ok := req.Secrets[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("required secret(s) not set in the environment: %s\n\nhint: export them, or add them to .buidl/secrets", strings.Join(missing, ", "))
	}

	// 5. Does the image exist in the registry? The kubelet's failure mode for a
	//    missing image is a slow ImagePullBackOff.
	if req.Release.Pinned() {
		if _, err := build.Resolve(ctx, req.Release.Ref()); err != nil {
			return fmt.Errorf("image %s is not available: %w", req.Release.Ref(), err)
		}
		t.log.Detail("image %s verified in registry", req.Release.ShortDigest())
	}

	// 6. Would this deploy replace an existing Deployment whose selector differs?
	//    A Deployment's selector is immutable, so that apply would be rejected.
	if err := t.checkSelectorCompatibility(ctx, req); err != nil {
		return err
	}

	return nil
}

// checkPermissions uses SelfSubjectAccessReview to verify write access up front.
func (t *Target) checkPermissions(ctx context.Context, req deploy.Request) error {
	objs, err := t.Render(req)
	if err != nil {
		return err
	}

	type check struct {
		group    string
		resource string
		kind     string
	}
	seen := map[check]bool{}
	for _, obj := range objs {
		gvr, _, err := t.resourceFor(obj.GVK)
		if err != nil {
			return err
		}
		seen[check{gvr.Group, gvr.Resource, obj.Kind}] = true
	}

	var denied []string
	for c := range seen {
		namespace := t.Namespace
		if c.kind == "Namespace" {
			namespace = ""
		}
		review, err := t.clientset.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, &authorizationv1.SelfSubjectAccessReview{
			Spec: authorizationv1.SelfSubjectAccessReviewSpec{
				ResourceAttributes: &authorizationv1.ResourceAttributes{
					Namespace: namespace,
					Verb:      "patch", // server-side apply is a PATCH
					Group:     c.group,
					Resource:  c.resource,
				},
			},
		}, metav1.CreateOptions{})
		if err != nil {
			// Some clusters restrict SSAR itself. That is not a reason to block a
			// deploy; the apply will report any real permission problem.
			t.log.Detail("could not check permissions for %s: %v", c.resource, err)
			return nil
		}
		if !review.Status.Allowed {
			denied = append(denied, c.kind)
		}
	}

	if len(denied) > 0 {
		sort.Strings(denied)
		return fmt.Errorf("the current credentials cannot write: %s in namespace %s\n\nhint: check the RBAC role bound to this service account or user", strings.Join(denied, ", "), t.Namespace)
	}
	return nil
}

// checkSelectorCompatibility catches the immutable-selector failure before apply.
func (t *Target) checkSelectorCompatibility(ctx context.Context, req deploy.Request) error {
	name := t.workloadName(req.Config, req.Release)
	live, err := t.clientset.AppsV1().Deployments(t.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil // absent, or unreadable; apply will report the latter
	}
	if live.Spec.Selector == nil {
		return nil
	}

	desired := selectorLabels(req.Config, req.Release)
	for k, v := range desired {
		if live.Spec.Selector.MatchLabels[k] != v {
			return fmt.Errorf(
				"existing Deployment %s has an incompatible selector (%s=%q, want %q)\n\n"+
					"A Deployment's selector cannot be changed after creation. Delete it first:\n"+
					"  kubectl delete deployment %s -n %s",
				name, k, live.Spec.Selector.MatchLabels[k], v, name, t.Namespace)
		}
	}
	return nil
}

// Deploy applies the release and waits for it to become healthy.
func (t *Target) Deploy(ctx context.Context, req deploy.Request) (*deploy.Outcome, error) {
	start := time.Now()

	t.resolveScale(ctx)

	// Capture what is live now, so a failed rollout can be reverted and so the
	// outcome can report what was replaced.
	previous, _ := t.liveRelease(ctx, req.Config)

	objs, err := t.Render(req)
	if err != nil {
		return nil, err
	}

	t.log.Step("Applying manifests")
	changes, err := t.applyAll(ctx, objs, replicas(req.Config))
	if err != nil {
		// Return the partial outcome with the error so the caller can report which
		// objects landed before the failure.
		return &deploy.Outcome{
			Release:         req.Release,
			PreviousRelease: previous,
			Changes:         changes,
			URL:             t.primaryURL(req.Config),
			Duration:        time.Since(start),
			Partial:         true,
		}, err
	}

	outcome := &deploy.Outcome{
		Release:         req.Release,
		PreviousRelease: previous,
		Changes:         changes,
		URL:             t.primaryURL(req.Config),
	}

	applied, unchanged := 0, 0
	for _, c := range changes {
		if c.Action == deploy.ActionUnchanged {
			unchanged++
		} else {
			applied++
		}
	}
	t.log.StepDetail("%d changed, %d unchanged", applied, unchanged)

	if !req.Wait {
		outcome.Duration = time.Since(start)
		return outcome, nil
	}

	name := t.workloadName(req.Config, req.Release)
	timeout := req.Config.Deploy.DeployTimeout.Or(defaultDeploy)

	t.log.Step("Waiting for health checks")
	if err := t.waitForRollout(ctx, name, req.Release, timeout); err != nil {
		t.reportRolloutFailure(err)

		if !req.AutoRollback || previous == "" {
			return outcome, err
		}

		// The new release never became healthy, so reverting restores a known
		// good state rather than leaving a half-rolled-out app.
		t.log.Warn("rollout failed; rolling back to %s", previous)
		if _, rbErr := t.Rollback(ctx, deploy.RollbackRequest{
			Config: req.Config,
			Root:   req.Root,
			To:     previous,
			Wait:   true,
		}); rbErr != nil {
			return outcome, fmt.Errorf("%w\n\nthe automatic rollback also failed: %v\n\nthe app may be in a degraded state; run `buidl status`", err, rbErr)
		}
		outcome.RolledBack = true
		outcome.Duration = time.Since(start)
		return outcome, fmt.Errorf("%w (rolled back to %s)", err, previous)
	}

	// Blue-green cuts over only after the new release is fully healthy. Until
	// this apply, the Service still pointed at the old release.
	if req.Config.Deploy.Strategy.Type == config.StrategyBlueGreen {
		t.log.Step("Switching traffic")
		if err := t.cutover(ctx, req); err != nil {
			return outcome, err
		}
		if err := t.reapOldBlueGreen(ctx, req); err != nil {
			// Leftover old Deployments cost resources but do not break the
			// deploy, so this is a warning rather than a failure.
			t.log.Warn("could not clean up superseded releases: %v", err)
		}
	}

	// Report what is actually serving, not merely that the apply succeeded.
	if pods, err := t.releasePods(ctx, req.Release.ID); err == nil {
		ready := 0
		for _, pod := range pods.Items {
			status := podStatus(pod)
			if status.Ready {
				ready++
			}
			outcome.Instances = append(outcome.Instances, status)
		}
		sort.Slice(outcome.Instances, func(i, j int) bool {
			return outcome.Instances[i].Name < outcome.Instances[j].Name
		})
		t.log.StepDetail("%d/%d instances ready", ready, len(outcome.Instances))
	}

	outcome.Duration = time.Since(start)
	return outcome, nil
}

// cutover re-applies the Service so its selector points at the new release.
func (t *Target) cutover(ctx context.Context, req deploy.Request) error {
	svc := t.service(req.Config, req.Release)
	if _, err := t.apply(ctx, svc, false); err != nil {
		return fmt.Errorf("switching traffic to %s: %w", req.Release.ID, err)
	}
	t.log.Success("traffic now served by %s", req.Release.ID)
	return nil
}

// reapOldBlueGreen deletes superseded per-release Deployments beyond the
// retention limit.
func (t *Target) reapOldBlueGreen(ctx context.Context, req deploy.Request) error {
	deps, err := t.clientset.AppsV1().Deployments(t.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s,%s=%s", release.LabelName, req.Config.App, release.LabelEnv, req.Release.Environment),
	})
	if err != nil {
		return err
	}

	var old []appsv1.Deployment
	for _, dep := range deps.Items {
		if dep.Annotations[release.AnnotationRelease] != req.Release.ID {
			old = append(old, dep)
		}
	}
	// Newest first, so the ones we keep are the most recent.
	sort.Slice(old, func(i, j int) bool {
		return old[i].CreationTimestamp.After(old[j].CreationTimestamp.Time)
	})

	// Keep RetainReleases-1 superseded releases so rollback stays instant.
	keep := req.Config.RetainReleases - 1
	if keep < 1 {
		keep = 1
	}
	for i, dep := range old {
		if i < keep {
			// Scale down but keep the object, so a rollback is a scale-up rather
			// than a fresh image pull.
			if err := t.scale(ctx, dep.Name, 0); err != nil {
				t.log.Detail("scaling down %s: %v", dep.Name, err)
			}
			continue
		}
		t.log.Detail("deleting superseded deployment %s", dep.Name)
		if err := t.clientset.AppsV1().Deployments(t.Namespace).Delete(ctx, dep.Name, metav1.DeleteOptions{}); err != nil && !isNotFound(err) {
			return err
		}
	}
	return nil
}

// scale sets a Deployment's replica count.
func (t *Target) scale(ctx context.Context, name string, replicas int32) error {
	s, err := t.clientset.AppsV1().Deployments(t.Namespace).GetScale(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	s.Spec.Replicas = replicas
	_, err = t.clientset.AppsV1().Deployments(t.Namespace).UpdateScale(ctx, name, s, metav1.UpdateOptions{})
	return err
}

// liveRelease returns the release ID currently serving traffic.
func (t *Target) liveRelease(ctx context.Context, cfg *config.Config) (string, error) {
	// For blue-green the Service selector is authoritative about what is live.
	if cfg.Deploy.Strategy.Type == config.StrategyBlueGreen {
		svc, err := t.clientset.CoreV1().Services(t.Namespace).Get(ctx, release.ObjectName(cfg.App), metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		return svc.Spec.Selector[release.LabelRelease], nil
	}

	dep, err := t.clientset.AppsV1().Deployments(t.Namespace).Get(ctx, release.ObjectName(cfg.App), metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	return dep.Annotations[release.AnnotationRelease], nil
}

// Rollback reverts to a previous release.
//
// Rollback never rebuilds and never re-resolves a tag. It reuses the exact pod
// template from a prior ReplicaSet, which is why it is both fast and safe: the
// bytes that ran before are the bytes that run again.
func (t *Target) Rollback(ctx context.Context, req deploy.RollbackRequest) (*deploy.Outcome, error) {
	start := time.Now()
	cfg := req.Config

	name := release.ObjectName(cfg.App)
	dep, err := t.clientset.AppsV1().Deployments(t.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("nothing to roll back: no deployment %s in namespace %s", name, t.Namespace)
		}
		return nil, err
	}

	current := dep.Annotations[release.AnnotationRelease]

	sets, err := t.replicaSetsFor(ctx, dep)
	if err != nil {
		return nil, err
	}
	if len(sets) < 2 && req.To == "" {
		return nil, errors.New("no previous release to roll back to")
	}

	target, err := selectRollbackTarget(sets, req.To, current)
	if err != nil {
		return nil, err
	}

	targetRelease := target.Annotations[release.AnnotationRelease]
	t.log.Info("rolling back %s to %s (revision %d)", cfg.App, orUnknown(targetRelease), revisionOf(*target))

	// Copy the historical pod template back onto the Deployment. This is exactly
	// what `kubectl rollout undo` does, and it means we do not need to reconstruct
	// the old configuration from anything outside the cluster.
	// Re-read and write under conflict retry.
	//
	// A rollback almost always runs while the Deployment controller is actively
	// writing status — that is the situation that caused the rollback. Any object
	// read even a moment earlier is therefore likely to be stale by the time it is
	// written back, and Kubernetes rejects the write with a conflict. Without this
	// retry an automatic rollback fails exactly when it is needed most, leaving a
	// broken release in place and reporting that the rollback also failed.
	var updated *appsv1.Deployment
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest, err := t.clientset.AppsV1().Deployments(t.Namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}

		patched := latest.DeepCopy()
		patched.Spec.Template = *target.Spec.Template.DeepCopy()
		// The template's selector-irrelevant labels come along; restore the release
		// annotations to match so status reporting stays truthful.
		if patched.Annotations == nil {
			patched.Annotations = map[string]string{}
		}
		for _, key := range []string{
			release.AnnotationRelease,
			release.AnnotationDigest,
			release.AnnotationGitSHA,
			release.AnnotationGitBranch,
		} {
			if v, ok := target.Annotations[key]; ok {
				patched.Annotations[key] = v
			} else {
				delete(patched.Annotations, key)
			}
		}
		patched.Annotations[release.AnnotationTime] = time.Now().UTC().Format(time.RFC3339)

		updated, err = t.clientset.AppsV1().Deployments(t.Namespace).Update(ctx, patched, metav1.UpdateOptions{
			FieldManager: FieldManager,
		})
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("applying rollback: %w", err)
	}

	outcome := &deploy.Outcome{
		PreviousRelease: current,
		URL:             t.primaryURL(cfg),
		RolledBack:      true,
	}
	outcome.Release.ID = targetRelease
	outcome.Release.Environment = cfg.Environment
	outcome.Release.Digest = target.Annotations[release.AnnotationDigest]

	if req.Wait {
		rel := release.Release{ID: targetRelease, Environment: cfg.Environment}
		if err := t.waitForRollout(ctx, updated.Name, rel, cfg.Deploy.DeployTimeout.Or(defaultDeploy)); err != nil {
			t.reportRolloutFailure(err)
			return outcome, fmt.Errorf("rollback did not become healthy: %w", err)
		}
	}

	outcome.Duration = time.Since(start)
	return outcome, nil
}

// selectRollbackTarget picks the ReplicaSet to restore.
func selectRollbackTarget(sets []appsv1.ReplicaSet, to, current string) (*appsv1.ReplicaSet, error) {
	if to == "" {
		// Default to the most recent set that is not the one running now.
		for i := range sets {
			if sets[i].Annotations[release.AnnotationRelease] != current {
				return &sets[i], nil
			}
		}
		return nil, errors.New("no previous release to roll back to")
	}

	for i := range sets {
		rs := &sets[i]
		if rs.Annotations[release.AnnotationRelease] == to {
			return rs, nil
		}
		// Also accept a revision number, matching kubectl's --to-revision.
		if fmt.Sprintf("%d", revisionOf(*rs)) == to {
			return rs, nil
		}
	}

	available := make([]string, 0, len(sets))
	for _, rs := range sets {
		if id := rs.Annotations[release.AnnotationRelease]; id != "" {
			available = append(available, id)
		}
	}
	return nil, fmt.Errorf("release %q not found in history (available: %s)", to, strings.Join(available, ", "))
}

// Status reports what is currently live.
func (t *Target) Status(ctx context.Context, req deploy.Request) (*deploy.Status, error) {
	cfg := req.Config
	name := release.ObjectName(cfg.App)

	dep, err := t.clientset.AppsV1().Deployments(t.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("%s is not deployed to %s (namespace %s)", cfg.App, cfg.Environment, t.Namespace)
		}
		return nil, t.wrapClusterError(err, "reading deployment status")
	}

	st := &deploy.Status{
		Environment: cfg.Environment,
		Release:     dep.Annotations[release.AnnotationRelease],
		Digest:      dep.Annotations[release.AnnotationDigest],
		GitSHA:      dep.Annotations[release.AnnotationGitSHA],
		DeployedBy:  dep.Annotations[release.AnnotationActor],
		Ready:       dep.Status.ReadyReplicas,
		Updated:     dep.Status.UpdatedReplicas,
		URL:         t.primaryURL(cfg),
	}
	if dep.Spec.Replicas != nil {
		st.Desired = *dep.Spec.Replicas
	}
	if len(dep.Spec.Template.Spec.Containers) > 0 {
		st.Image = dep.Spec.Template.Spec.Containers[0].Image
	}
	if ts := dep.Annotations[release.AnnotationTime]; ts != "" {
		if parsed, err := time.Parse(time.RFC3339, ts); err == nil {
			st.DeployedAt = parsed
		}
	}

	for _, cond := range dep.Status.Conditions {
		if cond.Type == appsv1.DeploymentAvailable {
			st.Available = cond.Status == corev1.ConditionTrue
		}
		if cond.Status != corev1.ConditionTrue && cond.Message != "" {
			st.Conditions = append(st.Conditions, fmt.Sprintf("%s: %s", cond.Reason, cond.Message))
		}
	}

	if pods, err := t.appPods(ctx, cfg.App, cfg.Environment); err == nil {
		for _, pod := range pods.Items {
			st.Pods = append(st.Pods, podStatus(pod))
		}
		sort.Slice(st.Pods, func(i, j int) bool { return st.Pods[i].Name < st.Pods[j].Name })
	}

	return st, nil
}

// Releases lists deploy history from ReplicaSet revisions, newest first.
func (t *Target) Releases(ctx context.Context, req deploy.Request) ([]deploy.ReleaseInfo, error) {
	cfg := req.Config
	name := release.ObjectName(cfg.App)

	dep, err := t.clientset.AppsV1().Deployments(t.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("%s is not deployed to %s (namespace %s)", cfg.App, cfg.Environment, t.Namespace)
		}
		return nil, t.wrapClusterError(err, "reading release history")
	}

	live := dep.Annotations[release.AnnotationRelease]

	sets, err := t.replicaSetsFor(ctx, dep)
	if err != nil {
		return nil, err
	}

	out := make([]deploy.ReleaseInfo, 0, len(sets))
	for _, rs := range sets {
		info := deploy.ReleaseInfo{
			ID:         rs.Annotations[release.AnnotationRelease],
			Digest:     rs.Annotations[release.AnnotationDigest],
			GitSHA:     rs.Annotations[release.AnnotationGitSHA],
			GitBranch:  rs.Annotations[release.AnnotationGitBranch],
			DeployedBy: rs.Annotations[release.AnnotationActor],
			Revision:   revisionOf(rs),
			CreatedAt:  rs.CreationTimestamp.Time,
			Replicas:   rs.Status.Replicas,
		}
		// A ReplicaSet with running pods is the live one; the annotation alone can
		// be stale mid-rollout.
		info.Live = info.ID == live && rs.Status.Replicas > 0
		if info.ID == "" {
			info.ID = fmt.Sprintf("revision-%d", info.Revision)
		}
		out = append(out, info)
	}

	return out, nil
}

// Logs streams container logs for the app.
func (t *Target) Logs(ctx context.Context, req deploy.LogRequest) error {
	cfg := req.Config

	selector := fmt.Sprintf("%s=%s,%s=%s", release.LabelName, cfg.App, release.LabelEnv, cfg.Environment)
	if req.Release != "" {
		selector = fmt.Sprintf("%s=%s", release.LabelRelease, req.Release)
	}

	pods, err := t.clientset.CoreV1().Pods(t.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return t.wrapClusterError(err, "listing instances")
	}
	if len(pods.Items) == 0 {
		return fmt.Errorf("no running instances found for %s in %s", cfg.App, cfg.Environment)
	}

	opts := &corev1.PodLogOptions{
		Container: cfg.App,
		Follow:    req.Follow,
	}
	if req.Tail >= 0 {
		opts.TailLines = &req.Tail
	}
	if req.Since > 0 {
		secs := int64(req.Since.Seconds())
		opts.SinceSeconds = &secs
	}

	// Interleave every instance's stream, prefixing each line with its pod so a
	// multi-replica log is readable.
	errs := make(chan error, len(pods.Items))
	for _, pod := range pods.Items {
		go func(podName string) {
			errs <- t.streamPodLogs(ctx, podName, opts, req.Out, len(pods.Items) > 1)
		}(pod.Name)
	}

	var firstErr error
	for range pods.Items {
		if err := <-errs; err != nil && firstErr == nil {
			// A pod that terminates mid-stream is normal during a rollout.
			if !errors.Is(err, context.Canceled) && !errors.Is(err, io.EOF) {
				firstErr = err
			}
		}
	}
	return firstErr
}

// streamPodLogs copies one pod's log stream to out.
func (t *Target) streamPodLogs(ctx context.Context, pod string, opts *corev1.PodLogOptions, out io.Writer, prefix bool) error {
	stream, err := t.clientset.CoreV1().Pods(t.Namespace).GetLogs(pod, opts).Stream(ctx)
	if err != nil {
		return fmt.Errorf("streaming logs from %s: %w", pod, err)
	}
	defer stream.Close()

	if !prefix {
		_, err = io.Copy(out, stream)
		return err
	}

	// Shorten the pod name to its unique suffix; the app and replicaset prefix is
	// identical on every line and just wastes width.
	short := pod
	if i := strings.LastIndex(pod, "-"); i >= 0 && i < len(pod)-1 {
		short = pod[i+1:]
	}

	return copyWithPrefix(out, stream, short+" | ")
}

// primaryURL returns the app's external address, if a hostname is configured.
func (t *Target) primaryURL(cfg *config.Config) string {
	hosts := hostList(cfg)
	if len(hosts) == 0 {
		return ""
	}
	scheme := "http"
	if cfg.Proxy.SSL {
		scheme = "https"
	}
	return scheme + "://" + hosts[0]
}

func orUnknown(s string) string {
	if s == "" {
		return "(unknown release)"
	}
	return s
}
