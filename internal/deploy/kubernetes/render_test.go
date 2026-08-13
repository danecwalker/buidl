package kubernetes

import (
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"

	"github.com/danecwalker/buidl/internal/config"
	"github.com/danecwalker/buidl/internal/deploy"
	"github.com/danecwalker/buidl/internal/gitinfo"
	"github.com/danecwalker/buidl/internal/release"
)

// testLogger discards output.
type testLogger struct{}

func (testLogger) Info(string, ...any)       {}
func (testLogger) Detail(string, ...any)     {}
func (testLogger) Warn(string, ...any)       {}
func (testLogger) Success(string, ...any)    {}
func (testLogger) Step(string)               {}
func (testLogger) StepDetail(string, ...any) {}

// newTestTarget builds a Target without touching a cluster. Render performs no
// API calls, so this exercises the real rendering path.
func newTestTarget(cfg *config.Config) *Target {
	return &Target{cfg: cfg, log: testLogger{}, Namespace: cfg.Deploy.Kubernetes.Namespace}
}

// testConfig returns a defaulted, validated config for rendering tests.
func testConfig(t *testing.T, yaml string) *config.Config {
	t.Helper()
	res, err := config.Load(config.LoadOptions{Path: writeConfig(t, yaml), Strict: true})
	if err != nil {
		t.Fatalf("loading test config: %v", err)
	}
	return res.Config
}

func testRelease() release.Release {
	rel := release.New("production", gitinfo.Info{
		Available: true,
		SHA:       "c653135554592aaaebae29ce2845bd6cd58aace6",
		Branch:    "main",
	}, time.Unix(1755000000, 0), "tester")
	rel.Repo = "ghcr.io/acme/web"
	rel.Tag = rel.ID
	rel.Digest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	return rel
}

func testRequest(t *testing.T, yaml string) (*Target, deploy.Request) {
	cfg := testConfig(t, yaml)
	return newTestTarget(cfg), deploy.Request{
		Config:  cfg,
		Release: testRelease(),
		Root:    ".",
	}
}

// renderBase is a valid config whose final block is `deploy`, so tests can
// append additional deploy keys by concatenation. Namespace is left to default
// from the app name so tests may add their own `kubernetes` block without
// creating a duplicate YAML key.
const renderBase = `
app: web
image: ghcr.io/acme/web
proxy:
  host: acme.com
  ssl: true
deploy:
  replicas: 3
  port: 3000
`

// findObject locates a rendered object by kind.
func findObject(objs []Object, kind string) *Object {
	for i := range objs {
		if objs[i].Kind == kind {
			return &objs[i]
		}
	}
	return nil
}

func TestRenderProducesExpectedObjects(t *testing.T) {
	target, req := testRequest(t, renderBase)

	objs, err := target.Render(req)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	for _, kind := range []string{"ServiceAccount", "Deployment", "Service", "Ingress", "PodDisruptionBudget"} {
		if findObject(objs, kind) == nil {
			t.Errorf("expected a %s to be rendered", kind)
		}
	}
	// No autoscale block means no HPA.
	if findObject(objs, "HorizontalPodAutoscaler") != nil {
		t.Error("did not expect an HPA without deploy.autoscale")
	}

	// Dependencies must apply before the workloads that reference them.
	order := map[string]int{}
	for _, o := range objs {
		order[o.Kind] = o.Order
	}
	if order["ServiceAccount"] >= order["Deployment"] {
		t.Error("ServiceAccount must apply before Deployment")
	}
	if order["Service"] >= order["Ingress"] {
		t.Error("Service must apply before Ingress")
	}
}

func TestRenderRequiresDigest(t *testing.T) {
	target, req := testRequest(t, renderBase)
	// An unpinned release must never be rendered: a tag could drift.
	req.Release.Digest = ""

	if _, err := target.Render(req); err == nil {
		t.Fatal("expected Render to reject a release with no digest")
	}
}

func TestDeploymentUsesDigestPinnedImage(t *testing.T) {
	target, req := testRequest(t, renderBase)
	objs, err := target.Render(req)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	dep := findObject(objs, "Deployment").Object.(*appsv1.Deployment)
	image := dep.Spec.Template.Spec.Containers[0].Image

	if !strings.Contains(image, "@sha256:") {
		t.Errorf("image = %q, want a digest-pinned reference", image)
	}
	if strings.Contains(image, ":"+req.Release.Tag) {
		t.Errorf("image = %q, must not be tag-based", image)
	}
}

func TestSelectorExcludesReleaseForRollingUpdates(t *testing.T) {
	target, req := testRequest(t, renderBase)
	objs, err := target.Render(req)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	dep := findObject(objs, "Deployment").Object.(*appsv1.Deployment)

	// A Deployment's selector is immutable. Including the release ID would make
	// the second deploy fail permanently.
	if _, found := dep.Spec.Selector.MatchLabels[release.LabelRelease]; found {
		t.Error("selector must not include the release label for a rolling update")
	}
	// Pods still carry it, so logs and blue-green can target one release.
	if got := dep.Spec.Template.Labels[release.LabelRelease]; got != req.Release.ID {
		t.Errorf("pod release label = %q, want %q", got, req.Release.ID)
	}
}

func TestBlueGreenNamesPerReleaseAndPinsServiceSelector(t *testing.T) {
	target, req := testRequest(t, renderBase+`
  strategy:
    type: bluegreen
`)
	objs, err := target.Render(req)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	dep := findObject(objs, "Deployment")
	// Blue-green needs both releases to coexist, so names must differ per release.
	if !strings.Contains(dep.Name, req.Release.ID) {
		t.Errorf("blue-green deployment name = %q, want it to include the release id", dep.Name)
	}

	svc := findObject(objs, "Service").Object.(*corev1.Service)
	// The Service selector is the atomic cutover point.
	if got := svc.Spec.Selector[release.LabelRelease]; got != req.Release.ID {
		t.Errorf("service selector release = %q, want %q", got, req.Release.ID)
	}
}

func TestRollingServiceSelectorIsReleaseAgnostic(t *testing.T) {
	target, req := testRequest(t, renderBase)
	objs, _ := target.Render(req)
	svc := findObject(objs, "Service").Object.(*corev1.Service)

	// For a rolling update the Service must match every release, or traffic would
	// drop during the transition.
	if _, found := svc.Spec.Selector[release.LabelRelease]; found {
		t.Error("rolling-update Service selector must not pin a release")
	}
}

func TestZeroDowntimeStrategyDefaults(t *testing.T) {
	target, req := testRequest(t, renderBase)
	objs, _ := target.Render(req)
	dep := findObject(objs, "Deployment").Object.(*appsv1.Deployment)

	if dep.Spec.Strategy.Type != appsv1.RollingUpdateDeploymentStrategyType {
		t.Fatalf("strategy = %v, want RollingUpdate", dep.Spec.Strategy.Type)
	}
	if got := dep.Spec.Strategy.RollingUpdate.MaxUnavailable.String(); got != "0" {
		t.Errorf("maxUnavailable = %q, want 0", got)
	}
}

func TestAutoscaleOmitsReplicas(t *testing.T) {
	target, req := testRequest(t, renderBase+`
  autoscale: {min: 3, max: 10, cpuPercent: 70}
`)
	objs, err := target.Render(req)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	dep := findObject(objs, "Deployment").Object.(*appsv1.Deployment)
	// Setting replicas here would make buidl and the HPA fight on every deploy.
	if dep.Spec.Replicas != nil {
		t.Errorf("replicas = %d, want unset when an HPA owns scaling", *dep.Spec.Replicas)
	}
	if findObject(objs, "HorizontalPodAutoscaler") == nil {
		t.Error("expected an HPA")
	}
}

func TestSingleReplicaSkipsPDBAndSpread(t *testing.T) {
	target, req := testRequest(t, `
app: web
image: ghcr.io/acme/web
deploy:
  replicas: 1
  kubernetes: {namespace: acme}
`)
	objs, err := target.Render(req)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	// A PDB with one replica would block every node drain.
	if findObject(objs, "PodDisruptionBudget") != nil {
		t.Error("did not expect a PDB with a single replica")
	}
	dep := findObject(objs, "Deployment").Object.(*appsv1.Deployment)
	if len(dep.Spec.Template.Spec.TopologySpreadConstraints) != 0 {
		t.Error("did not expect topology spread with a single replica")
	}
}

func TestTopologySpreadIsBestEffort(t *testing.T) {
	target, req := testRequest(t, renderBase)
	objs, _ := target.Render(req)
	dep := findObject(objs, "Deployment").Object.(*appsv1.Deployment)

	constraints := dep.Spec.Template.Spec.TopologySpreadConstraints
	if len(constraints) == 0 {
		t.Fatal("expected a topology spread constraint")
	}
	// DoNotSchedule would leave pods Pending forever on a single-node cluster.
	if constraints[0].WhenUnsatisfiable != corev1.ScheduleAnyway {
		t.Errorf("WhenUnsatisfiable = %v, want ScheduleAnyway", constraints[0].WhenUnsatisfiable)
	}
}

func TestNoIngressWithoutHost(t *testing.T) {
	target, req := testRequest(t, `
app: worker
image: ghcr.io/acme/worker
deploy:
  kubernetes: {namespace: acme}
  healthcheck:
    command: [test, -f, /tmp/ready]
`)
	objs, err := target.Render(req)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if findObject(objs, "Ingress") != nil {
		t.Error("a worker with no host must not get an Ingress")
	}

	dep := findObject(objs, "Deployment").Object.(*appsv1.Deployment)
	probe := dep.Spec.Template.Spec.Containers[0].ReadinessProbe
	if probe.Exec == nil {
		t.Error("expected an exec probe when healthcheck.command is set")
	}
	if probe.HTTPGet != nil {
		t.Error("did not expect an HTTP probe alongside a command probe")
	}
}

func TestIngressTLSAndIssuer(t *testing.T) {
	target, req := testRequest(t, renderBase)
	objs, _ := target.Render(req)

	obj := findObject(objs, "Ingress")
	ing := obj.Object.(*networkingv1.Ingress)

	if len(ing.Spec.TLS) != 1 || ing.Spec.TLS[0].Hosts[0] != "acme.com" {
		t.Errorf("TLS = %+v, want a cert for acme.com", ing.Spec.TLS)
	}
	if got := ing.Annotations["cert-manager.io/cluster-issuer"]; got != "letsencrypt-prod" {
		t.Errorf("cluster-issuer = %q, want letsencrypt-prod", got)
	}
	if ing.Spec.Rules[0].Host != "acme.com" {
		t.Errorf("host = %q", ing.Spec.Rules[0].Host)
	}
}

func TestSecretChecksumTriggersRollout(t *testing.T) {
	cfg := testConfig(t, `
app: web
image: ghcr.io/acme/web
deploy:
  kubernetes: {namespace: acme}
env:
  secret: [DATABASE_URL]
`)
	target := newTestTarget(cfg)
	rel := testRelease()

	render := func(secretValue string) *appsv1.Deployment {
		objs, err := target.Render(deploy.Request{
			Config:  cfg,
			Release: rel,
			Root:    ".",
			Secrets: map[string]string{"DATABASE_URL": secretValue},
		})
		if err != nil {
			t.Fatalf("Render: %v", err)
		}
		if findObject(objs, "Secret") == nil {
			t.Fatal("expected a Secret to be rendered")
		}
		return findObject(objs, "Deployment").Object.(*appsv1.Deployment)
	}

	a := render("postgres://a")
	b := render("postgres://b")
	c := render("postgres://a")

	keyA := a.Spec.Template.Annotations[release.AnnotationConfigSum]
	keyB := b.Spec.Template.Annotations[release.AnnotationConfigSum]
	keyC := c.Spec.Template.Annotations[release.AnnotationConfigSum]

	if keyA == "" {
		t.Fatal("expected a config checksum annotation on the pod template")
	}
	// Without this, changing a secret would leave pods running the stale value.
	if keyA == keyB {
		t.Error("changing a secret value must change the pod template checksum")
	}
	// And it must be stable, or every deploy would churn pods needlessly.
	if keyA != keyC {
		t.Error("an unchanged secret must produce a stable checksum")
	}
}

func TestEnvIsDeterministicAndOverridable(t *testing.T) {
	target, req := testRequest(t, `
app: web
image: ghcr.io/acme/web
deploy:
  port: 3000
  kubernetes: {namespace: acme}
env:
  clear:
    ZULU: last
    ALPHA: first
    PORT: "9999"
`)

	var previous []corev1.EnvVar
	for range 5 {
		objs, err := target.Render(req)
		if err != nil {
			t.Fatalf("Render: %v", err)
		}
		env := findObject(objs, "Deployment").Object.(*appsv1.Deployment).Spec.Template.Spec.Containers[0].Env

		if previous != nil {
			for i := range env {
				if env[i].Name != previous[i].Name {
					t.Fatalf("env ordering is unstable: %v then %v", previous, env)
				}
			}
		}
		previous = env
	}

	byName := map[string]string{}
	for _, e := range previous {
		byName[e.Name] = e.Value
	}
	// An explicit value must win over the injected default.
	if byName["PORT"] != "9999" {
		t.Errorf("PORT = %q, want the user's 9999", byName["PORT"])
	}
	for _, key := range []string{"BUIDL_ENV", "BUIDL_RELEASE", "BUIDL_APP"} {
		if byName[key] == "" {
			t.Errorf("expected %s to be injected", key)
		}
	}
}

func TestSecurityHardening(t *testing.T) {
	target, req := testRequest(t, renderBase)
	objs, _ := target.Render(req)
	pod := findObject(objs, "Deployment").Object.(*appsv1.Deployment).Spec.Template.Spec

	if pod.SecurityContext == nil || pod.SecurityContext.RunAsNonRoot == nil || !*pod.SecurityContext.RunAsNonRoot {
		t.Error("pods must be forced to run as non-root")
	}
	sc := pod.Containers[0].SecurityContext
	if sc == nil || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Error("privilege escalation must be disabled")
	}
	if sc.Capabilities == nil || len(sc.Capabilities.Drop) == 0 {
		t.Error("all capabilities should be dropped")
	}
}

func TestGracefulShutdownWiring(t *testing.T) {
	target, req := testRequest(t, renderBase+`
  drainTimeout: 60s
`)
	objs, _ := target.Render(req)
	pod := findObject(objs, "Deployment").Object.(*appsv1.Deployment).Spec.Template.Spec

	if pod.TerminationGracePeriodSeconds == nil || *pod.TerminationGracePeriodSeconds != 60 {
		t.Errorf("grace period = %v, want 60", pod.TerminationGracePeriodSeconds)
	}
	// A preStop delay lets load balancers stop routing before SIGTERM arrives.
	if lc := pod.Containers[0].Lifecycle; lc == nil || lc.PreStop == nil || lc.PreStop.Sleep == nil {
		t.Error("expected a preStop sleep to drain connections")
	}
}

func TestProbeDerivation(t *testing.T) {
	target, req := testRequest(t, renderBase)
	objs, _ := target.Render(req)
	c := findObject(objs, "Deployment").Object.(*appsv1.Deployment).Spec.Template.Spec.Containers[0]

	if c.ReadinessProbe == nil || c.LivenessProbe == nil || c.StartupProbe == nil {
		t.Fatal("expected readiness, liveness and startup probes")
	}
	// Liveness must be more forgiving than readiness: it kills the container.
	if c.LivenessProbe.FailureThreshold <= c.ReadinessProbe.FailureThreshold {
		t.Errorf("liveness threshold (%d) should exceed readiness (%d)",
			c.LivenessProbe.FailureThreshold, c.ReadinessProbe.FailureThreshold)
	}
	if c.StartupProbe.PeriodSeconds >= c.ReadinessProbe.PeriodSeconds {
		t.Error("startup probe should poll more frequently than readiness")
	}
}

func TestNamespaceCreatedOnlyWhenRequested(t *testing.T) {
	target, req := testRequest(t, renderBase+`
  kubernetes:
    namespace: acme-preview
    createNamespace: true
`)
	objs, err := target.Render(req)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	ns := findObject(objs, "Namespace")
	if ns == nil {
		t.Fatal("expected a Namespace when createNamespace is true")
	}
	if ns.Namespaced {
		t.Error("Namespace is a cluster-scoped object")
	}
	if ns.Order != orderNamespace {
		t.Error("Namespace must apply first")
	}
}

// TestRenderedObjectsDecodeIntoTypedScheme verifies every rendered object
// survives a round trip through the Kubernetes scheme.
//
// This is the offline equivalent of `kubectl apply --dry-run`: it proves the
// objects are structurally valid against the real API types, including that no
// field we strip for apply (like creationTimestamp) breaks decoding.
func TestRenderedObjectsDecodeIntoTypedScheme(t *testing.T) {
	target, req := testRequest(t, renderBase+`
  autoscale: {min: 3, max: 10, cpuPercent: 70}
  kubernetes:
    namespace: acme
    createNamespace: true
`)
	objs, err := target.Render(req)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(objs) == 0 {
		t.Fatal("nothing rendered")
	}

	for _, obj := range objs {
		u, err := toUnstructured(obj.Object, obj.GVK)
		if err != nil {
			t.Fatalf("%s/%s: toUnstructured: %v", obj.Kind, obj.Name, err)
		}

		// Every object must carry the apiVersion/kind that server-side apply needs.
		if u.GetAPIVersion() == "" || u.GetKind() == "" {
			t.Errorf("%s/%s: missing apiVersion or kind", obj.Kind, obj.Name)
		}
		if u.GetName() == "" {
			t.Errorf("%s: missing name", obj.Kind)
		}
		if obj.Namespaced && u.GetNamespace() == "" {
			t.Errorf("%s/%s: namespaced object has no namespace", obj.Kind, obj.Name)
		}

		// Converting back into a typed object proves the field names and shapes
		// match the real API schema.
		typed, err := scheme.Scheme.New(obj.GVK)
		if err != nil {
			t.Fatalf("%s: unknown GVK %s: %v", obj.Kind, obj.GVK, err)
		}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, typed); err != nil {
			t.Errorf("%s/%s: does not decode into %s: %v", obj.Kind, obj.Name, obj.GVK, err)
		}

		// A "null" creationTimestamp is rejected by the apply endpoint.
		if _, found, _ := unstructuredNestedFieldNoCopy(u.Object, "metadata", "creationTimestamp"); found {
			t.Errorf("%s/%s: creationTimestamp must be stripped before apply", obj.Kind, obj.Name)
		}
		if _, found, _ := unstructuredNestedFieldNoCopy(u.Object, "status"); found {
			t.Errorf("%s/%s: status must be stripped before apply", obj.Kind, obj.Name)
		}
	}
}

func TestObjectNameStaysWithinKubernetesLimit(t *testing.T) {
	longApp := strings.Repeat("a", 60)
	name := release.ObjectName(longApp, "some-release-id-that-is-long")
	if len(name) > 63 {
		t.Errorf("name length = %d, want <= 63", len(name))
	}
	if strings.HasSuffix(name, "-") {
		t.Errorf("name %q must not end with a dash", name)
	}
}
