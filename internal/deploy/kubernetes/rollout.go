package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/danecwalker/buidl/internal/config"
	"github.com/danecwalker/buidl/internal/deploy"
	"github.com/danecwalker/buidl/internal/release"
)

// terminalPodReasons are container states that will never resolve on their own.
//
// Detecting these is the difference between a deploy that fails in eight seconds
// with "image not found" and one that fails in five minutes with "timed out".
var terminalPodReasons = map[string]string{
	"ErrImagePull":     "the image could not be pulled",
	"ImagePullBackOff": "the image could not be pulled",
	"InvalidImageName": "the image reference is invalid",
	// CreateContainerConfigError covers several distinct causes, so its
	// explanation is derived from the message rather than guessed at. Naming the
	// wrong cause is worse than naming none.
	"CreateContainerConfigError": "",
	"CreateContainerError":       "the container could not be created",
	"CrashLoopBackOff":           "the container is crashing on startup",
}

// explainWaiting turns a container's waiting state into a cause, consulting the
// message where the reason alone is ambiguous.
func explainWaiting(waiting *corev1.ContainerStateWaiting) string {
	message := strings.ToLower(waiting.Message)

	switch {
	case strings.Contains(message, "non-numeric user"):
		return "the image's USER is a name, not a numeric UID, so Kubernetes cannot verify it is non-root"
	case strings.Contains(message, "secret") && strings.Contains(message, "not found"):
		return "a referenced Secret is missing"
	case strings.Contains(message, "configmap") && strings.Contains(message, "not found"):
		return "a referenced ConfigMap is missing"
	}

	if explanation := terminalPodReasons[waiting.Reason]; explanation != "" {
		return explanation
	}
	return "the container could not be started"
}

// remedyFor suggests a fix for causes with a known one.
func remedyFor(waiting *corev1.ContainerStateWaiting, pod corev1.Pod) string {
	message := strings.ToLower(waiting.Message)

	if strings.Contains(message, "non-numeric user") {
		return "\n\nbuidl sets runAsNonRoot for defense in depth, which requires a numeric UID.\n" +
			"Change the Dockerfile's final USER to a number:\n" +
			"  USER 65532:65532        # distroless nonroot\n" +
			"  USER 1000:1000          # node\n" +
			"and rebuild. `id -u <name>` inside the image gives the right value."
	}

	if isUnauthorizedPull(waiting) && len(pod.Spec.ImagePullSecrets) == 0 {
		return "\n\nThe image was pushed successfully, so this is a *pull* credential problem:\n" +
			"the cluster has no imagePullSecret and cannot authenticate to the registry.\n\n" +
			"hint: log in so buidl can copy the credential into the cluster:\n" +
			"  docker login <registry>\n" +
			"then redeploy. Or set them in the file:\n" +
			"  registry:\n    username: your-username\n    password: ${REGISTRY_TOKEN}\n" +
			"or reference a secret you already maintain:\n" +
			"  registry:\n    pullSecret: my-registry-creds\n" +
			"If you set registry.createPullSecret: false, turn it back on."
	}

	return ""
}

// rolloutError describes a failed rollout with the diagnosis attached.
type rolloutError struct {
	message string
	// Pods carries the failing instances so the caller can render detail.
	Pods []deploy.PodStatus
	// Logs holds tail output from a crashing container, which is almost always
	// the actual answer to "why did my deploy fail".
	Logs string
}

func (e *rolloutError) Error() string { return e.message }

// waitForRollout blocks until the Deployment reports the new release healthy, or
// fails fast on a terminal condition.
func (t *Target) waitForRollout(ctx context.Context, name string, rel release.Release, timeout time.Duration) error {
	// Conditions carried over from a previous rollout are ignored by comparing
	// against this; see rolloutProgress.
	startedAt := time.Now()
	deadline := startedAt.Add(timeout)
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	var lastMessage string
	for {
		dep, err := t.clientset.AppsV1().Deployments(t.Namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if isNotFound(err) {
				return fmt.Errorf("deployment %s disappeared during rollout", name)
			}
			// A transient API error should not abort a rollout that may well be
			// succeeding; retry until the deadline.
			t.log.Detail("polling %s: %v", name, err)
		} else {
			done, msg, err := t.rolloutProgress(ctx, dep, rel, startedAt)
			if err != nil {
				return err
			}
			if done {
				return nil
			}
			if msg != "" && msg != lastMessage {
				t.log.Info("%s", msg)
				lastMessage = msg
			}
		}

		select {
		case <-ctx.Done():
			// Distinguish our deadline from a user interrupt.
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return t.timeoutError(name, rel, timeout)
			}
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// rolloutProgress evaluates one poll of the Deployment.
func (t *Target) rolloutProgress(ctx context.Context, dep *appsv1.Deployment, rel release.Release, startedAt time.Time) (done bool, message string, err error) {
	// A terminal pod failure is checked first so we fail fast rather than
	// reporting slow progress toward an outcome that will never arrive.
	if fail, err := t.checkTerminalFailure(ctx, rel); err != nil {
		return false, "", err
	} else if fail != nil {
		return false, "", fail
	}

	// generation vs observedGeneration tells us whether the controller has even
	// seen our update yet. This must come first: until the controller has caught
	// up, every field in status — including the conditions below — describes the
	// *previous* rollout.
	if dep.Generation > dep.Status.ObservedGeneration {
		return false, "waiting for the deployment controller to observe the update", nil
	}

	// ProgressDeadlineExceeded is the controller's own verdict that the rollout is
	// stuck — but only if it refers to this rollout.
	//
	// The condition persists after a failed rollout until a new one makes
	// progress, so a rollback started moments later sees the *previous* failure
	// and aborts immediately. That breaks recovery at the exact moment it
	// matters: the deploy failed, and the rollback then refuses to run because
	// the deploy failed. Ignoring conditions last updated before this wait began
	// scopes the verdict to the rollout actually in progress.
	for _, cond := range dep.Status.Conditions {
		if cond.Type != appsv1.DeploymentProgressing || cond.Reason != "ProgressDeadlineExceeded" {
			continue
		}
		if cond.LastUpdateTime.Time.Before(startedAt) {
			continue
		}
		return false, "", &rolloutError{
			message: fmt.Sprintf("rollout of %s exceeded its progress deadline: %s", dep.Name, cond.Message),
		}
	}

	desired := int32(1)
	if dep.Spec.Replicas != nil {
		desired = *dep.Spec.Replicas
	}

	// Naming the individual instances turns "2/3 ready" into something
	// actionable: the user can see which pod is lagging and go look at it.
	detail := t.instanceDetail(ctx, rel)

	switch {
	case dep.Status.UpdatedReplicas < desired:
		return false, withDetail(fmt.Sprintf("rolling out: %d/%d new instances created", dep.Status.UpdatedReplicas, desired), detail), nil
	case dep.Status.Replicas > dep.Status.UpdatedReplicas:
		return false, withDetail(fmt.Sprintf("draining %d old instance(s)", dep.Status.Replicas-dep.Status.UpdatedReplicas), detail), nil
	case dep.Status.AvailableReplicas < desired:
		return false, withDetail(fmt.Sprintf("waiting for health checks: %d/%d instances ready", dep.Status.AvailableReplicas, desired), detail), nil
	}

	// Zero desired replicas is a legitimate scaled-to-zero state.
	if desired == 0 {
		return true, "", nil
	}

	return true, "", nil
}

// isUnauthorizedPull reports whether a waiting state is an authentication failure
// against the registry, as opposed to a missing tag or an unreachable host.
func isUnauthorizedPull(waiting *corev1.ContainerStateWaiting) bool {
	if waiting.Reason != "ErrImagePull" && waiting.Reason != "ImagePullBackOff" {
		return false
	}
	message := strings.ToLower(waiting.Message)
	return strings.Contains(message, "unauthorized") ||
		strings.Contains(message, "authentication required") ||
		strings.Contains(message, "denied")
}

// instanceDetail summarizes the release's pods as "name=state" pairs.
//
// Returns "" when pods cannot be listed; this is progress reporting, so it must
// never be the reason a rollout fails.
func (t *Target) instanceDetail(ctx context.Context, rel release.Release) string {
	pods, err := t.releasePods(ctx, rel.ID)
	if err != nil || len(pods.Items) == 0 {
		return ""
	}

	items := make([]string, 0, len(pods.Items))
	for _, pod := range pods.Items {
		state := string(pod.Status.Phase)
		switch {
		case podReady(pod):
			state = "ready"
		default:
			// A waiting reason (ContainerCreating, ImagePullBackOff) says far more
			// than the phase does.
			for _, cs := range pod.Status.ContainerStatuses {
				if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
					state = cs.State.Waiting.Reason
					break
				}
			}
		}
		items = append(items, shortPodName(pod.Name)+"="+state)
	}
	sort.Strings(items)
	return strings.Join(items, " ")
}

// withDetail appends instance detail to a progress message.
func withDetail(message, detail string) string {
	if detail == "" {
		return message
	}
	return message + "  [" + detail + "]"
}

// shortPodName trims the deployment and replicaset prefix, which is identical on
// every pod and only consumes width.
func shortPodName(name string) string {
	if i := strings.LastIndex(name, "-"); i >= 0 && i < len(name)-1 {
		return name[i+1:]
	}
	return name
}

// checkTerminalFailure inspects the release's pods for unrecoverable states.
func (t *Target) checkTerminalFailure(ctx context.Context, rel release.Release) (error, error) {
	pods, err := t.releasePods(ctx, rel.ID)
	if err != nil {
		// Pod listing is diagnostic only; never fail a rollout because of it.
		return nil, nil
	}

	for _, pod := range pods.Items {
		for _, cs := range append(pod.Status.ContainerStatuses, pod.Status.InitContainerStatuses...) {
			waiting := cs.State.Waiting
			if waiting == nil {
				continue
			}
			if _, terminal := terminalPodReasons[waiting.Reason]; !terminal {
				continue
			}
			explanation := explainWaiting(waiting)

			// A container that crashes after starting may simply be slow to
			// stabilize on its first restart; only treat repeated crashes as
			// terminal so a single restart does not abort a healthy deploy.
			if waiting.Reason == "CrashLoopBackOff" && cs.RestartCount < 2 {
				continue
			}

			message := fmt.Sprintf("instance %s failed: %s (%s: %s)",
				pod.Name, explanation, waiting.Reason, waiting.Message)
			// Several of these causes have a specific, known fix; stating it beats
			// leaving the user to search for the Kubernetes error text.
			message += remedyFor(waiting, pod)

			re := &rolloutError{
				message: message,
				Pods:    []deploy.PodStatus{podStatus(pod)},
			}
			// Crash loops are explained by the container's own output.
			if waiting.Reason == "CrashLoopBackOff" {
				re.Logs = t.tailLogs(ctx, pod.Name, cs.Name, 30)
			}
			return re, nil
		}
	}

	return nil, nil
}

// timeoutError builds a diagnostic error when the deploy timeout elapses.
func (t *Target) timeoutError(name string, rel release.Release, timeout time.Duration) error {
	// Naming the gate is the useful part: a timeout almost always means the
	// readiness endpoint never returned success, not that the deploy was slow.
	hint := probeTimeoutHint(t.cfg)

	re := &rolloutError{
		message: fmt.Sprintf("deploy timed out after %s waiting for %s to become healthy%s", timeout, name, hint),
	}

	// Attach whatever we can learn, since the user's next question is always
	// "why".
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if pods, err := t.releasePods(ctx, rel.ID); err == nil {
		for _, pod := range pods.Items {
			ps := podStatus(pod)
			if !ps.Ready {
				re.Pods = append(re.Pods, ps)
			}
		}
		// Show the first unready pod's logs, which usually contain the failing
		// health check or a startup error.
		for _, pod := range pods.Items {
			if !podReady(pod) && len(pod.Spec.Containers) > 0 {
				re.Logs = t.tailLogs(ctx, pod.Name, pod.Spec.Containers[0].Name, 30)
				break
			}
		}
	}

	return re
}

// probeTimeoutHint names the configured probe paths so a timed-out deploy
// tells the user which endpoints to implement, instead of guessing another.
func probeTimeoutHint(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	hc := cfg.Deploy.Healthcheck
	if len(hc.Command) > 0 {
		return fmt.Sprintf("\n\nThe rollout is gated on the exec probe %s.\n"+
			"An instance that stays unready means that command is not exiting 0.",
			strings.Join(hc.Command, " "))
	}
	ready := hc.Readiness
	if ready == "" {
		return ""
	}
	if hc.Liveness == ready && hc.Startup == ready {
		return fmt.Sprintf("\n\nThe rollout is gated on GET %s.\n"+
			"That path must return 2xx once the app can serve traffic.\n"+
			"Implement it, or set deploy.healthcheck.path to an endpoint you already serve.",
			ready)
	}
	return fmt.Sprintf("\n\nThe rollout is gated on GET %s (startup %s, liveness %s).\n"+
		"Those paths must return 2xx. Implement them, or set deploy.healthcheck.path\n"+
		"to an endpoint you already serve.",
		ready, hc.Startup, hc.Liveness)
}

// releasePods lists the pods belonging to one release.
func (t *Target) releasePods(ctx context.Context, releaseID string) (*corev1.PodList, error) {
	return t.clientset.CoreV1().Pods(t.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", release.LabelRelease, releaseID),
	})
}

// appPods lists every pod for the app in this environment, across releases.
func (t *Target) appPods(ctx context.Context, app, env string) (*corev1.PodList, error) {
	return t.clientset.CoreV1().Pods(t.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s,%s=%s", release.LabelName, app, release.LabelEnv, env),
	})
}

// tailLogs fetches the last n lines from a container, best-effort.
func (t *Target) tailLogs(ctx context.Context, pod, container string, lines int64) string {
	req := t.clientset.CoreV1().Pods(t.Namespace).GetLogs(pod, &corev1.PodLogOptions{
		Container: container,
		TailLines: &lines,
	})
	stream, err := req.Stream(ctx)
	if err != nil {
		// A crashed container's logs may only be available as "previous".
		prev := t.clientset.CoreV1().Pods(t.Namespace).GetLogs(pod, &corev1.PodLogOptions{
			Container: container,
			TailLines: &lines,
			Previous:  true,
		})
		stream, err = prev.Stream(ctx)
		if err != nil {
			return ""
		}
	}
	defer stream.Close()

	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := stream.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
		// Cap what we buffer; this is a diagnostic tail, not a log viewer.
		if b.Len() > 64*1024 {
			break
		}
	}
	return b.String()
}

// podStatus converts a Pod into the backend-agnostic status type.
func podStatus(pod corev1.Pod) deploy.PodStatus {
	ps := deploy.PodStatus{
		Name:    pod.Name,
		Phase:   string(pod.Status.Phase),
		Ready:   podReady(pod),
		Node:    pod.Spec.NodeName,
		Release: pod.Labels[release.LabelRelease],
	}
	if !pod.CreationTimestamp.IsZero() {
		ps.Age = time.Since(pod.CreationTimestamp.Time)
	}
	for _, cs := range pod.Status.ContainerStatuses {
		ps.Restarts += cs.RestartCount
		if cs.State.Waiting != nil && ps.Message == "" {
			ps.Message = cs.State.Waiting.Reason
			if cs.State.Waiting.Message != "" {
				ps.Message += ": " + cs.State.Waiting.Message
			}
		}
		if cs.State.Terminated != nil && ps.Message == "" {
			ps.Message = fmt.Sprintf("terminated (exit %d): %s", cs.State.Terminated.ExitCode, cs.State.Terminated.Reason)
		}
	}
	// A pending pod is usually unschedulable; that reason lives in conditions.
	if ps.Message == "" && pod.Status.Phase == corev1.PodPending {
		for _, cond := range pod.Status.Conditions {
			if cond.Type == corev1.PodScheduled && cond.Status == corev1.ConditionFalse {
				ps.Message = cond.Reason
				if cond.Message != "" {
					ps.Message += ": " + cond.Message
				}
			}
		}
	}

	// A container that is running but not ready has no waiting or terminated
	// state, so without this the message would be a bare "Running" — which reads
	// as healthy and says nothing about why the rollout is stuck. In practice this
	// always means the readiness probe is not passing.
	if ps.Message == "" && pod.Status.Phase == corev1.PodRunning && !ps.Ready {
		ps.Message = "running but not ready: readiness probe has not passed"
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.Started != nil && !*cs.Started {
				ps.Message = "running but not started: startup probe has not passed"
				break
			}
		}
	}

	return ps
}

func podReady(pod corev1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

// reportRolloutFailure prints the diagnosis attached to a rollout error.
func (t *Target) reportRolloutFailure(err error) {
	var re *rolloutError
	if !errors.As(err, &re) {
		return
	}
	for _, pod := range re.Pods {
		msg := pod.Message
		if msg == "" {
			msg = pod.Phase
		}
		t.log.Warn("instance %s: %s (restarts: %d)", pod.Name, msg, pod.Restarts)
	}
	if re.Logs != "" {
		t.log.Warn("last log output from the failing instance:")
		for _, line := range strings.Split(strings.TrimRight(re.Logs, "\n"), "\n") {
			t.log.Info("  | %s", line)
		}
	}
}

// replicaSetsFor returns the ReplicaSets owned by a Deployment, newest revision
// first.
//
// ReplicaSet history is buidl's release history: each deploy produces one, and
// each carries the release annotations we stamped. That means rollback needs no
// external state store — the cluster is the source of truth.
func (t *Target) replicaSetsFor(ctx context.Context, dep *appsv1.Deployment) ([]appsv1.ReplicaSet, error) {
	selector, err := metav1.LabelSelectorAsSelector(dep.Spec.Selector)
	if err != nil {
		return nil, err
	}
	list, err := t.clientset.AppsV1().ReplicaSets(t.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector.String(),
	})
	if err != nil {
		return nil, err
	}

	owned := make([]appsv1.ReplicaSet, 0, len(list.Items))
	for _, rs := range list.Items {
		for _, ref := range rs.OwnerReferences {
			if ref.UID == dep.UID {
				owned = append(owned, rs)
				break
			}
		}
	}

	sort.Slice(owned, func(i, j int) bool {
		return revisionOf(owned[i]) > revisionOf(owned[j])
	})
	return owned, nil
}

// revisionOf reads the Deployment controller's revision annotation.
func revisionOf(rs appsv1.ReplicaSet) int64 {
	var n int64
	if v := rs.Annotations["deployment.kubernetes.io/revision"]; v != "" {
		fmt.Sscanf(v, "%d", &n)
	}
	return n
}
