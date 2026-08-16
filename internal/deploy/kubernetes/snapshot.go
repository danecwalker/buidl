package kubernetes

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/danecwalker/buidl/internal/config"
	"github.com/danecwalker/buidl/internal/release"
	"github.com/danecwalker/buidl/internal/watch"
)

var (
	podMetricsGVR = schema.GroupVersionResource{
		Group:    "metrics.k8s.io",
		Version:  "v1beta1",
		Resource: "pods",
	}
	nodeMetricsGVR = schema.GroupVersionResource{
		Group:    "metrics.k8s.io",
		Version:  "v1beta1",
		Resource: "nodes",
	}
)

// WatchSnapshot is one observation of every stack member plus the nodes.
//
// Status looks up a Deployment named after the app. Blue-green names that
// object <app>-<release>, so the lookup misses the live workload. Watch lists
// by label instead, and overlays metrics.k8s.io when it is there.
func (t *Target) WatchSnapshot(ctx context.Context, cfg *config.Config) (watch.Snapshot, error) {
	now := time.Now()
	snap := watch.Snapshot{
		Time:        now,
		Stack:       cfg.App,
		Environment: cfg.Environment,
		Namespace:   t.Namespace,
		Context:     t.contextName,
		Server:      t.serverHost,
		Metrics:     watch.MetricsMissing,
	}

	pods, err := t.clientset.CoreV1().Pods(t.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s,%s=%s", release.LabelManagedBy, release.ManagedBy, release.LabelEnv, cfg.Environment),
	})
	if err != nil {
		return snap, t.wrapClusterError(err, "listing instances")
	}

	deps, err := t.clientset.AppsV1().Deployments(t.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s,%s=%s", release.LabelManagedBy, release.ManagedBy, release.LabelEnv, cfg.Environment),
	})
	if err != nil {
		return snap, t.wrapClusterError(err, "listing apps")
	}

	sets, err := t.clientset.AppsV1().StatefulSets(t.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s,%s=%s,%s=%s",
			release.LabelManagedBy, release.ManagedBy,
			release.LabelEnv, cfg.Environment,
			release.LabelComponent, accessoryComponent),
	})
	if err != nil {
		return snap, t.wrapClusterError(err, "listing stateful apps")
	}

	depsByName := map[string][]appsv1.Deployment{}
	for _, d := range deps.Items {
		name := d.Labels[release.LabelName]
		depsByName[name] = append(depsByName[name], d)
	}
	setByName := map[string]appsv1.StatefulSet{}
	for _, s := range sets.Items {
		setByName[s.Name] = s
	}
	liveByApp := t.serviceReleases(ctx, cfg)

	usage, metrics := t.collectUsage(ctx)
	snap.Metrics = metrics.state
	snap.MetricsErr = metrics.err

	for _, name := range cfg.ProcessAppNames() {
		appCfg, err := cfg.ForProcessApp(name)
		if err != nil {
			return snap, err
		}
		app := t.watchProcess(now, name, depsByName[name], pods.Items, usage, liveByApp[name])
		app.URL = t.primaryURL(appCfg)
		snap.Apps = append(snap.Apps, app)
	}

	accNames := make([]string, 0, len(cfg.Accessories))
	for name := range cfg.Accessories {
		accNames = append(accNames, name)
	}
	sort.Strings(accNames)
	for _, name := range accNames {
		object := accessoryName(cfg, name)
		kind := config.NormalizeAccessoryType(cfg.Accessories[name].Type)
		if kind == "" {
			kind = "stateful"
		}
		set, ok := setByName[object]
		var ptr *appsv1.StatefulSet
		if ok {
			ptr = &set
		}
		snap.Apps = append(snap.Apps, t.watchStateful(now, name, kind, object, ptr, pods.Items, usage))
	}

	nodes, err := t.clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		// Apps still painted; a node list that 403s should not hide the stack.
		snap.Alerts = append(snap.Alerts, watch.Alert{Level: "warn", Text: "could not list nodes: " + oneLineErr(err)})
	} else {
		snap.Nodes = watchNodes(now, nodes.Items, usage.nodes)
	}

	snap.Alerts = append(deriveAlerts(snap), snap.Alerts...)
	return snap, nil
}

type usageIndex struct {
	pods  map[string]watch.Usage
	nodes map[string]watch.Usage
}

type metricsResult struct {
	state watch.MetricsState
	err   string
}

func (t *Target) collectUsage(ctx context.Context) (usageIndex, metricsResult) {
	idx := usageIndex{pods: map[string]watch.Usage{}, nodes: map[string]watch.Usage{}}
	if t.dynamic == nil {
		return idx, metricsResult{state: watch.MetricsMissing}
	}

	podList, err := t.dynamic.Resource(podMetricsGVR).Namespace(t.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return idx, classifyMetrics(err)
	}
	for i := range podList.Items {
		name, u, ok := parseMetricsObject(&podList.Items[i])
		if ok {
			idx.pods[name] = u
		}
	}

	nodeList, err := t.dynamic.Resource(nodeMetricsGVR).List(ctx, metav1.ListOptions{})
	if err != nil && !isMetricsAbsent(err) {
		// Pods already loaded; node metrics are optional.
		return idx, metricsResult{state: watch.MetricsOK, err: oneLineErr(err)}
	}
	if err == nil {
		for i := range nodeList.Items {
			name, u, ok := parseMetricsObject(&nodeList.Items[i])
			if ok {
				idx.nodes[name] = u
			}
		}
	}
	return idx, metricsResult{state: watch.MetricsOK}
}

func classifyMetrics(err error) metricsResult {
	if isMetricsAbsent(err) {
		return metricsResult{state: watch.MetricsMissing}
	}
	if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) {
		return metricsResult{state: watch.MetricsDenied, err: oneLineErr(err)}
	}
	return metricsResult{state: watch.MetricsError, err: oneLineErr(err)}
}

func isMetricsAbsent(err error) bool {
	if err == nil {
		return false
	}
	if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "metrics.k8s.io") &&
		(strings.Contains(msg, "could not find") ||
			strings.Contains(msg, "no matches") ||
			strings.Contains(msg, "the server could not find"))
}

func parseMetricsObject(u *unstructured.Unstructured) (string, watch.Usage, bool) {
	name := u.GetName()
	if name == "" {
		return "", watch.Usage{}, false
	}
	containers, found, err := unstructured.NestedSlice(u.Object, "containers")
	if err != nil || !found {
		// NodeMetrics uses usage at the top level, not per container.
		if usage, ok := quantityMap(u.Object, "usage"); ok {
			return name, usage, true
		}
		return "", watch.Usage{}, false
	}
	var out watch.Usage
	for _, c := range containers {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if usage, ok := quantityMap(cm, "usage"); ok {
			out = watch.AddUsage(out, usage)
		}
	}
	if !out.Known {
		return "", watch.Usage{}, false
	}
	return name, out, true
}

func quantityMap(obj map[string]any, field string) (watch.Usage, bool) {
	raw, found, err := unstructured.NestedMap(obj, field)
	if err != nil || !found {
		return watch.Usage{}, false
	}
	var cpu, mem resource.Quantity
	have := false
	if s, ok := raw["cpu"].(string); ok && s != "" {
		if q, err := resource.ParseQuantity(s); err == nil {
			cpu = q
			have = true
		}
	}
	if s, ok := raw["memory"].(string); ok && s != "" {
		if q, err := resource.ParseQuantity(s); err == nil {
			mem = q
			have = true
		}
	}
	if !have {
		return watch.Usage{}, false
	}
	return watch.Usage{Known: true, CPUMilli: cpu.MilliValue(), Memory: mem.Value()}, true
}

func (t *Target) watchProcess(now time.Time, name string, deps []appsv1.Deployment, pods []corev1.Pod, usage usageIndex, liveRelease string) watch.App {
	app := watch.App{Name: name, Type: "app"}
	if len(deps) == 0 {
		app.Health = watch.HealthMissing
		return app
	}

	live := pickLiveDeployment(deps, liveRelease)
	if live != nil {
		app.Release = live.Annotations[release.AnnotationRelease]
		app.GitSHA = live.Annotations[release.AnnotationGitSHA]
		if live.Spec.Replicas != nil {
			app.Desired = *live.Spec.Replicas
		}
		app.Ready = live.Status.ReadyReplicas
		if len(live.Spec.Template.Spec.Containers) > 0 {
			app.Image = live.Spec.Template.Spec.Containers[0].Image
		}
		if app.Release == "" {
			app.Release = live.Labels[release.LabelRelease]
		}
	}

	app.Instances = instancesFor(now, pods, func(p corev1.Pod) bool {
		return p.Labels[release.LabelName] == name && p.Labels[release.LabelComponent] != accessoryComponent
	}, usage.pods)
	summarizeApp(&app, now, deps)
	return app
}

func (t *Target) watchStateful(now time.Time, name, kind, object string, set *appsv1.StatefulSet, pods []corev1.Pod, usage usageIndex) watch.App {
	app := watch.App{Name: name, Type: kind}
	if set == nil {
		app.Health = watch.HealthMissing
		return app
	}
	if set.Spec.Replicas != nil {
		app.Desired = *set.Spec.Replicas
	} else {
		app.Desired = 1
	}
	app.Ready = set.Status.ReadyReplicas
	if len(set.Spec.Template.Spec.Containers) > 0 {
		app.Image = set.Spec.Template.Spec.Containers[0].Image
	}
	app.Instances = instancesFor(now, pods, func(p corev1.Pod) bool {
		return p.Labels[release.LabelName] == object
	}, usage.pods)
	summarizeApp(&app, now, nil)
	return app
}

func (t *Target) serviceReleases(ctx context.Context, cfg *config.Config) map[string]string {
	out := map[string]string{}
	list, err := t.clientset.CoreV1().Services(t.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s,%s=%s", release.LabelManagedBy, release.ManagedBy, release.LabelEnv, cfg.Environment),
	})
	if err != nil {
		return out
	}
	for _, svc := range list.Items {
		name := svc.Labels[release.LabelName]
		if name == "" {
			continue
		}
		if rel := svc.Spec.Selector[release.LabelRelease]; rel != "" {
			out[name] = rel
		}
	}
	return out
}

func pickLiveDeployment(deps []appsv1.Deployment, liveRelease string) *appsv1.Deployment {
	if liveRelease != "" {
		for i := range deps {
			if deps[i].Annotations[release.AnnotationRelease] == liveRelease || deps[i].Labels[release.LabelRelease] == liveRelease {
				return &deps[i]
			}
		}
	}
	var best *appsv1.Deployment
	for i := range deps {
		d := &deps[i]
		if best == nil || d.Status.ReadyReplicas > best.Status.ReadyReplicas {
			best = d
		}
	}
	return best
}

func instancesFor(now time.Time, pods []corev1.Pod, match func(corev1.Pod) bool, usage map[string]watch.Usage) []watch.Instance {
	var out []watch.Instance
	for _, p := range pods {
		if !match(p) {
			continue
		}
		out = append(out, instanceFromPod(now, p, usage[p.Name]))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func instanceFromPod(now time.Time, pod corev1.Pod, usage watch.Usage) watch.Instance {
	ps := podStatus(pod)
	inst := watch.Instance{
		Name:     ps.Name,
		Phase:    ps.Phase,
		Ready:    ps.Ready,
		Restarts: ps.Restarts,
		Age:      ps.Age,
		Node:     ps.Node,
		Release:  ps.Release,
		Message:  ps.Message,
		Usage:    usage,
	}
	if start := containerStartedAt(pod); !start.IsZero() {
		inst.StartedAt = start
		inst.Uptime = now.Sub(start)
	}
	return inst
}

func containerStartedAt(pod corev1.Pod) time.Time {
	var earliest time.Time
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Running == nil || cs.State.Running.StartedAt.IsZero() {
			continue
		}
		t := cs.State.Running.StartedAt.Time
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest
}

func summarizeApp(app *watch.App, now time.Time, deps []appsv1.Deployment) {
	releases := map[string]struct{}{}
	var (
		restarts int32
		usage    watch.Usage
		oldest   time.Time
	)
	for _, inst := range app.Instances {
		restarts += inst.Restarts
		usage = watch.AddUsage(usage, inst.Usage)
		if inst.Release != "" {
			releases[inst.Release] = struct{}{}
		}
		if inst.Ready && !inst.StartedAt.IsZero() && (oldest.IsZero() || inst.StartedAt.Before(oldest)) {
			oldest = inst.StartedAt
		}
	}
	if oldest.IsZero() {
		for _, inst := range app.Instances {
			if !inst.StartedAt.IsZero() && (oldest.IsZero() || inst.StartedAt.Before(oldest)) {
				oldest = inst.StartedAt
			}
		}
	}
	app.Restarts = restarts
	app.Usage = usage
	app.Releases = len(releases)
	if !oldest.IsZero() {
		app.StartedAt = oldest
		app.Uptime = now.Sub(oldest)
	}
	if app.Health == watch.HealthMissing {
		return
	}
	if app.Desired == 0 {
		app.Health = watch.HealthStopped
		return
	}
	if app.Ready < app.Desired {
		app.Health = watch.HealthDegraded
		return
	}
	if len(releases) > 1 || extraDeployment(deps) {
		app.Health = watch.HealthRollout
		return
	}
	app.Health = watch.HealthHealthy
}

func extraDeployment(deps []appsv1.Deployment) bool {
	live := 0
	for _, d := range deps {
		if d.Status.Replicas > 0 {
			live++
		}
	}
	return live > 1
}

func watchNodes(now time.Time, nodes []corev1.Node, usage map[string]watch.Usage) []watch.Node {
	out := make([]watch.Node, 0, len(nodes))
	for _, n := range nodes {
		row := watch.Node{
			Name:        n.Name,
			Schedulable: !n.Spec.Unschedulable,
			Version:     n.Status.NodeInfo.KubeletVersion,
			Roles:       nodeRoles(n),
			Usage:       usage[n.Name],
		}
		if !n.CreationTimestamp.IsZero() {
			row.Age = now.Sub(n.CreationTimestamp.Time)
		}
		for _, cond := range n.Status.Conditions {
			if cond.Type == corev1.NodeReady {
				row.Ready = cond.Status == corev1.ConditionTrue
				if !row.Ready && cond.Message != "" {
					row.Message = cond.Message
				}
			}
		}
		if cpu := n.Status.Allocatable.Cpu(); cpu != nil {
			row.CPUAlloc = cpu.MilliValue()
		}
		if mem := n.Status.Allocatable.Memory(); mem != nil {
			row.MemAlloc = mem.Value()
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func nodeRoles(n corev1.Node) string {
	var roles []string
	for k, v := range n.Labels {
		if k == "kubernetes.io/role" && v != "" {
			roles = append(roles, v)
			continue
		}
		const prefix = "node-role.kubernetes.io/"
		if strings.HasPrefix(k, prefix) {
			role := strings.TrimPrefix(k, prefix)
			if role != "" {
				roles = append(roles, role)
			}
		}
	}
	sort.Strings(roles)
	return strings.Join(roles, ",")
}

func deriveAlerts(snap watch.Snapshot) []watch.Alert {
	var out []watch.Alert
	for _, app := range snap.Apps {
		switch app.Health {
		case watch.HealthDegraded:
			msg := fmt.Sprintf("%s is degraded (%s ready)", app.Name, watch.FormatReady(app.Ready, app.Desired))
			for _, inst := range app.Instances {
				if !inst.Ready && inst.Message != "" {
					msg += " — " + inst.Message
					break
				}
			}
			out = append(out, watch.Alert{Level: "crit", Text: msg})
		case watch.HealthMissing:
			out = append(out, watch.Alert{Level: "warn", Text: app.Name + " is not deployed"})
		case watch.HealthRollout:
			out = append(out, watch.Alert{Level: "warn", Text: app.Name + " is rolling out"})
		}
		if app.Restarts >= 5 {
			out = append(out, watch.Alert{Level: "warn", Text: fmt.Sprintf("%s has %d restarts", app.Name, app.Restarts)})
		}
	}
	for _, n := range snap.Nodes {
		if !n.Ready {
			msg := n.Name + " is not ready"
			if n.Message != "" {
				msg += " — " + n.Message
			}
			out = append(out, watch.Alert{Level: "crit", Text: msg})
		}
	}
	return out
}

func oneLineErr(err error) string {
	if err == nil {
		return ""
	}
	s := strings.ReplaceAll(err.Error(), "\n", " ")
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 120 {
		return s[:117] + "..."
	}
	return s
}
