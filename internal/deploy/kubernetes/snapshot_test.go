package kubernetes

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/danecwalker/buidl/internal/config"
	"github.com/danecwalker/buidl/internal/release"
	"github.com/danecwalker/buidl/internal/watch"
)

func TestWatchSnapshotProcessAndAccessory(t *testing.T) {
	cfg := watchTestConfig(t, `
app: web
image: ghcr.io/acme/web
accessories:
  postgres:
    type: postgres
    env:
      secret: [POSTGRES_PASSWORD]
`)
	started := metav1.NewTime(time.Now().Add(-3 * time.Hour))
	replicas := int32(2)
	one := int32(1)

	objects := []runtime.Object{
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "web",
				Namespace: "web",
				Labels: map[string]string{
					release.LabelManagedBy: release.ManagedBy,
					release.LabelName:      "web",
					release.LabelEnv:       "default",
					release.LabelRelease:   "rel-1",
				},
				Annotations: map[string]string{
					release.AnnotationRelease: "rel-1",
					release.AnnotationGitSHA:  "abc1234",
				},
			},
			Spec: appsv1.DeploymentSpec{
				Replicas: &replicas,
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "web", Image: "ghcr.io/acme/web@sha256:abc"}}},
				},
			},
			Status: appsv1.DeploymentStatus{ReadyReplicas: 2, Replicas: 2},
		},
		&appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "web-postgres",
				Namespace: "web",
				Labels: map[string]string{
					release.LabelManagedBy: release.ManagedBy,
					release.LabelName:      "web-postgres",
					release.LabelEnv:       "default",
					release.LabelComponent: accessoryComponent,
				},
			},
			Spec: appsv1.StatefulSetSpec{
				Replicas: &one,
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "postgres", Image: "postgres:17"}}},
				},
			},
			Status: appsv1.StatefulSetStatus{ReadyReplicas: 1},
		},
		watchPod("web", "web-aaaa", "rel-1", started, true, 0),
		watchPod("web", "web-bbbb", "rel-1", started, true, 0),
		watchAccessoryPod("web-postgres", "web-postgres-0", started),
		readyCapNode("node-1"),
	}

	metrics := []runtime.Object{
		podMetrics("web", "web-aaaa", "22m", "64Mi"),
		podMetrics("web", "web-bbbb", "23m", "64Mi"),
		podMetrics("web", "web-postgres-0", "8m", "256Mi"),
		nodeMetrics("node-1", "500m", "1200Mi"),
	}

	tgt := watchTarget(t, cfg, objects, metrics...)
	snap, err := tgt.WatchSnapshot(context.Background(), cfg)
	if err != nil {
		t.Fatalf("WatchSnapshot: %v", err)
	}
	if snap.Metrics != watch.MetricsOK {
		t.Errorf("metrics = %s (%s), want ok", snap.Metrics, snap.MetricsErr)
	}
	if len(snap.Apps) != 2 {
		t.Fatalf("apps = %d, want 2: %+v", len(snap.Apps), snap.Apps)
	}

	web := snap.Apps[0]
	if web.Name != "web" || web.Health != watch.HealthHealthy || web.Ready != 2 || web.Desired != 2 {
		t.Errorf("web = %+v", web)
	}
	if !web.Usage.Known || web.Usage.CPUMilli != 45 {
		t.Errorf("web cpu = %+v, want 45m", web.Usage)
	}
	if web.Usage.Memory != 128*1024*1024 {
		t.Errorf("web mem = %d, want 128Mi", web.Usage.Memory)
	}
	if web.StartedAt.IsZero() || web.Uptime < 2*time.Hour {
		t.Errorf("web uptime = %s started %v", web.Uptime, web.StartedAt)
	}
	if len(web.Instances) != 2 {
		t.Errorf("web instances = %d", len(web.Instances))
	}

	pg := snap.Apps[1]
	if pg.Name != "postgres" || pg.Type != "postgres" || pg.Health != watch.HealthHealthy {
		t.Errorf("postgres = %+v", pg)
	}
	if !pg.Usage.Known || pg.Usage.Memory != 256*1024*1024 {
		t.Errorf("postgres mem = %+v", pg.Usage)
	}

	if len(snap.Nodes) != 1 || !snap.Nodes[0].Ready {
		t.Errorf("nodes = %+v", snap.Nodes)
	}
	if !snap.Nodes[0].Usage.Known || snap.Nodes[0].Usage.CPUMilli != 500 {
		t.Errorf("node usage = %+v", snap.Nodes[0].Usage)
	}
}

func TestWatchSnapshotMissingAppAndMetrics(t *testing.T) {
	cfg := watchTestConfig(t, `
app: web
image: ghcr.io/acme/web
`)
	tgt := watchTarget(t, cfg, nil)
	snap, err := tgt.WatchSnapshot(context.Background(), cfg)
	if err != nil {
		t.Fatalf("WatchSnapshot: %v", err)
	}
	if snap.Metrics != watch.MetricsMissing {
		t.Errorf("metrics = %s, want missing", snap.Metrics)
	}
	if len(snap.Apps) != 1 || snap.Apps[0].Health != watch.HealthMissing {
		t.Errorf("apps = %+v, want web missing", snap.Apps)
	}
	var saw bool
	for _, a := range snap.Alerts {
		if strings.Contains(a.Text, "not deployed") {
			saw = true
		}
	}
	if !saw {
		t.Errorf("alerts = %+v, want not deployed", snap.Alerts)
	}
}

func TestWatchSnapshotBlueGreenPicksLiveService(t *testing.T) {
	cfg := watchTestConfig(t, `
app: web
image: ghcr.io/acme/web
deploy:
  strategy:
    type: bluegreen
`)
	one := int32(1)
	objects := []runtime.Object{
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "web-old",
				Namespace: "web",
				Labels: map[string]string{
					release.LabelManagedBy: release.ManagedBy,
					release.LabelName:      "web",
					release.LabelEnv:       "default",
					release.LabelRelease:   "old",
				},
				Annotations: map[string]string{release.AnnotationRelease: "old"},
			},
			Spec:   appsv1.DeploymentSpec{Replicas: &one},
			Status: appsv1.DeploymentStatus{ReadyReplicas: 1, Replicas: 1},
		},
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "web-new",
				Namespace: "web",
				Labels: map[string]string{
					release.LabelManagedBy: release.ManagedBy,
					release.LabelName:      "web",
					release.LabelEnv:       "default",
					release.LabelRelease:   "new",
				},
				Annotations: map[string]string{release.AnnotationRelease: "new"},
			},
			Spec:   appsv1.DeploymentSpec{Replicas: &one},
			Status: appsv1.DeploymentStatus{ReadyReplicas: 1, Replicas: 1},
		},
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "web",
				Namespace: "web",
				Labels: map[string]string{
					release.LabelManagedBy: release.ManagedBy,
					release.LabelName:      "web",
					release.LabelEnv:       "default",
				},
			},
			Spec: corev1.ServiceSpec{Selector: map[string]string{release.LabelRelease: "new"}},
		},
		watchPod("web", "web-old-0", "old", metav1.NewTime(time.Now().Add(-time.Hour)), true, 0),
		watchPod("web", "web-new-0", "new", metav1.NewTime(time.Now().Add(-time.Minute)), true, 0),
	}

	tgt := watchTarget(t, cfg, objects)
	snap, err := tgt.WatchSnapshot(context.Background(), cfg)
	if err != nil {
		t.Fatalf("WatchSnapshot: %v", err)
	}
	if snap.Apps[0].Release != "new" {
		t.Errorf("live release = %q, want new", snap.Apps[0].Release)
	}
	if snap.Apps[0].Health != watch.HealthRollout {
		t.Errorf("health = %q, want rollout with two releases", snap.Apps[0].Health)
	}
	if snap.Apps[0].Releases != 2 {
		t.Errorf("releases = %d, want 2", snap.Apps[0].Releases)
	}
}

func TestParseMetricsObject(t *testing.T) {
	name, usage, ok := parseMetricsObject(podMetrics("web", "web-aaaa", "22m", "64Mi"))
	if !ok || name != "web-aaaa" {
		t.Fatalf("pod metrics: name=%q ok=%v", name, ok)
	}
	if usage.CPUMilli != 22 || usage.Memory != 64*1024*1024 {
		t.Errorf("pod usage = %+v", usage)
	}
	name, usage, ok = parseMetricsObject(nodeMetrics("node-1", "500m", "1200Mi"))
	if !ok || name != "node-1" || usage.CPUMilli != 500 || usage.Memory != 1200*1024*1024 {
		t.Errorf("node metrics = %s %+v ok=%v", name, usage, ok)
	}
}

func TestWatchSnapshotDegradedRestartsAlert(t *testing.T) {
	cfg := watchTestConfig(t, `
app: web
image: ghcr.io/acme/web
`)
	one := int32(1)
	objects := []runtime.Object{
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "web",
				Namespace: "web",
				Labels: map[string]string{
					release.LabelManagedBy: release.ManagedBy,
					release.LabelName:      "web",
					release.LabelEnv:       "default",
				},
			},
			Spec:   appsv1.DeploymentSpec{Replicas: &one},
			Status: appsv1.DeploymentStatus{ReadyReplicas: 0, Replicas: 1},
		},
		func() runtime.Object {
			p := watchPod("web", "web-x", "rel", metav1.Now(), false, 6)
			p.Status.Phase = corev1.PodRunning
			p.Status.ContainerStatuses[0].State = corev1.ContainerState{
				Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
			}
			return p
		}(),
	}
	tgt := watchTarget(t, cfg, objects)
	snap, err := tgt.WatchSnapshot(context.Background(), cfg)
	if err != nil {
		t.Fatalf("WatchSnapshot: %v", err)
	}
	if snap.Apps[0].Health != watch.HealthDegraded {
		t.Errorf("health = %q, want degraded", snap.Apps[0].Health)
	}
	if snap.Apps[0].Restarts != 6 {
		t.Errorf("restarts = %d", snap.Apps[0].Restarts)
	}
	joined := ""
	for _, a := range snap.Alerts {
		joined += a.Text + "\n"
	}
	if !strings.Contains(joined, "degraded") || !strings.Contains(joined, "6 restarts") {
		t.Errorf("alerts = %s", joined)
	}
}

func watchTestConfig(t *testing.T, yaml string) *config.Config {
	t.Helper()
	cfg := testConfig(t, yaml)
	if cfg.Deploy.Kubernetes.Namespace == "" {
		cfg.Deploy.Kubernetes.Namespace = cfg.App
	}
	return cfg
}

func watchTarget(t *testing.T, cfg *config.Config, objects []runtime.Object, metrics ...runtime.Object) *Target {
	t.Helper()
	tgt := &Target{
		cfg:         cfg,
		log:         testLogger{},
		Namespace:   cfg.Deploy.Kubernetes.Namespace,
		clientset:   fake.NewSimpleClientset(objects...),
		contextName: cfg.App + "-" + cfg.Environment,
		serverHost:  "https://127.0.0.1:6443",
	}
	if len(metrics) == 0 {
		return tgt
	}
	// The fake pluralizes Kind "PodMetrics" to "podmetricss". Create on the
	// real GVRs so List matches what the cluster serves (pods / nodes).
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			podMetricsGVR:  "PodMetricsList",
			nodeMetricsGVR: "NodeMetricsList",
		},
	)
	ctx := context.Background()
	for _, obj := range metrics {
		u := obj.(*unstructured.Unstructured)
		switch u.GetKind() {
		case "NodeMetrics":
			if _, err := dyn.Resource(nodeMetricsGVR).Create(ctx, u, metav1.CreateOptions{}); err != nil {
				t.Fatalf("creating node metrics: %v", err)
			}
		default:
			ns := u.GetNamespace()
			if _, err := dyn.Resource(podMetricsGVR).Namespace(ns).Create(ctx, u, metav1.CreateOptions{}); err != nil {
				t.Fatalf("creating pod metrics: %v", err)
			}
		}
	}
	tgt.dynamic = dyn
	return tgt
}

func watchPod(app, name, rel string, started metav1.Time, ready bool, restarts int32) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "web",
			Labels: map[string]string{
				release.LabelManagedBy: release.ManagedBy,
				release.LabelName:      app,
				release.LabelEnv:       "default",
				release.LabelRelease:   rel,
			},
			CreationTimestamp: started,
		},
		Spec: corev1.PodSpec{NodeName: "node-1"},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:         app,
				Ready:        ready,
				RestartCount: restarts,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{
					StartedAt: started,
				}},
			}},
		},
	}
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: status}}
	return p
}

func watchAccessoryPod(labelName, name string, started metav1.Time) *corev1.Pod {
	p := watchPod(labelName, name, "", started, true, 0)
	p.Labels[release.LabelComponent] = accessoryComponent
	return p
}

func readyCapNode(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			CreationTimestamp: metav1.NewTime(time.Now().Add(-12 * 24 * time.Hour)),
			Labels:            map[string]string{"node-role.kubernetes.io/control-plane": ""},
		},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("2"),
				corev1.ResourceMemory: resource.MustParse("4Gi"),
			},
			NodeInfo: corev1.NodeSystemInfo{KubeletVersion: "v1.34.1"},
		},
	}
}

func podMetrics(ns, name, cpu, mem string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "metrics.k8s.io/v1beta1",
		"kind":       "PodMetrics",
		"metadata":   map[string]any{"name": name, "namespace": ns},
		"containers": []any{
			map[string]any{
				"name":  "app",
				"usage": map[string]any{"cpu": cpu, "memory": mem},
			},
		},
	}}
}

func nodeMetrics(name, cpu, mem string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "metrics.k8s.io/v1beta1",
		"kind":       "NodeMetrics",
		"metadata":   map[string]any{"name": name},
		"usage":      map[string]any{"cpu": cpu, "memory": mem},
	}}
}
