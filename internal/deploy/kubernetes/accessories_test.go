package kubernetes

import (
	"reflect"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"

	"github.com/danecwalker/buidl/internal/config"
	"github.com/danecwalker/buidl/internal/deploy"
	"github.com/danecwalker/buidl/internal/release"
)

// accessoryBase configures an app with a stateful accessory and a stateless one.
const accessoryBase = `
app: web
image: ghcr.io/acme/web
deploy:
  replicas: 3
  kubernetes: {namespace: acme}
env:
  secret: [DATABASE_URL]
accessories:
  postgres:
    image: postgres:16
    port: 5432
    storage: 10Gi
    storageClass: fast
    env:
      clear: {POSTGRES_DB: web}
      secret: [POSTGRES_PASSWORD]
  redis:
    image: redis:7
    port: 6379
    cmd: [redis-server, --appendonly, "yes"]
`

// accessoryRequest builds a request carrying values for every declared secret.
func accessoryRequest(t *testing.T, yaml string) (*Target, deploy.Request) {
	t.Helper()
	target, req := testRequest(t, yaml)
	req.Secrets = map[string]string{
		"DATABASE_URL":      "postgres://web@web-postgres/web",
		"POSTGRES_PASSWORD": "hunter2-do-not-leak",
	}
	return target, req
}

// findNamed locates a rendered object by kind and name.
func findNamed(objs []Object, kind, name string) *Object {
	for i := range objs {
		if objs[i].Kind == kind && objs[i].Name == name {
			return &objs[i]
		}
	}
	return nil
}

func TestRenderAccessoriesShape(t *testing.T) {
	target, req := accessoryRequest(t, accessoryBase)

	objs, err := target.RenderAccessories(req)
	if err != nil {
		t.Fatalf("RenderAccessories: %v", err)
	}

	tests := []struct {
		name       string
		kind       string
		objectName string
		want       bool
	}{
		{"postgres workload", "StatefulSet", "web-postgres", true},
		{"postgres headless service", "Service", "web-postgres", true},
		{"postgres env secret", "Secret", "web-postgres-env", true},
		{"redis workload", "StatefulSet", "web-redis", true},
		{"redis headless service", "Service", "web-redis", true},
		// redis declares no secrets, so it must not get an empty Secret object.
		{"redis env secret", "Secret", "web-redis-env", false},
		// Accessories are never Deployments: a rescheduled pod must find its volume.
		{"no deployment", "Deployment", "web-postgres", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := findNamed(objs, tt.kind, tt.objectName) != nil
			if got != tt.want {
				t.Errorf("%s/%s present = %v, want %v", tt.kind, tt.objectName, got, tt.want)
			}
		})
	}

	// A Secret the pod references must exist before the pod that mounts it.
	sec := findNamed(objs, "Secret", "web-postgres-env")
	set := findNamed(objs, "StatefulSet", "web-postgres")
	if sec.Order >= set.Order {
		t.Error("the accessory Secret must apply before its StatefulSet")
	}
	if svc := findNamed(objs, "Service", "web-postgres"); svc.Order >= set.Order {
		t.Error("the headless Service must exist before the pod that gets its DNS name")
	}
}

func TestAccessoryServiceIsHeadless(t *testing.T) {
	target, req := accessoryRequest(t, accessoryBase)
	objs, err := target.RenderAccessories(req)
	if err != nil {
		t.Fatalf("RenderAccessories: %v", err)
	}

	svc := findNamed(objs, "Service", "web-postgres").Object.(*corev1.Service)
	// Without clusterIP: None there is no stable per-pod DNS name, which is half
	// the reason to use a StatefulSet at all.
	if svc.Spec.ClusterIP != corev1.ClusterIPNone {
		t.Errorf("clusterIP = %q, want None", svc.Spec.ClusterIP)
	}
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != 5432 {
		t.Errorf("ports = %+v, want the accessory's 5432", svc.Spec.Ports)
	}

	set := findNamed(objs, "StatefulSet", "web-postgres").Object.(*appsv1.StatefulSet)
	// serviceName must name the headless Service or the pods get no DNS records.
	if set.Spec.ServiceName != svc.Name {
		t.Errorf("serviceName = %q, want %q", set.Spec.ServiceName, svc.Name)
	}
	if set.Spec.Selector.MatchLabels[release.LabelName] != svc.Spec.Selector[release.LabelName] {
		t.Error("the Service must select the pods its StatefulSet creates")
	}
}

func TestAccessoryStorage(t *testing.T) {
	tests := []struct {
		name        string
		accessory   string
		wantVolumes bool
		wantMount   string
		wantClass   string
	}{
		{
			name: "storage requests a claim template",
			accessory: `
  postgres:
    image: postgres:16
    storage: 10Gi
    storageClass: fast
`,
			wantVolumes: true,
			wantMount:   "/var/lib/postgresql/data",
			wantClass:   "fast",
		},
		{
			name: "no storage means no volumes at all",
			accessory: `
  cache:
    image: redis:7
`,
			wantVolumes: false,
		},
		{
			name: "storage without a class defers to the cluster default",
			accessory: `
  cache:
    image: redis:7
    storage: 1Gi
`,
			wantVolumes: true,
			wantMount:   "/data",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target, req := accessoryRequest(t, `
app: web
image: ghcr.io/acme/web
deploy:
  kubernetes: {namespace: acme}
accessories:`+tt.accessory)

			objs, err := target.RenderAccessories(req)
			if err != nil {
				t.Fatalf("RenderAccessories: %v", err)
			}
			set := findObject(objs, "StatefulSet").Object.(*appsv1.StatefulSet)
			claims := set.Spec.VolumeClaimTemplates
			mounts := set.Spec.Template.Spec.Containers[0].VolumeMounts

			if !tt.wantVolumes {
				if len(claims) != 0 || len(mounts) != 0 {
					t.Fatalf("claims = %d, mounts = %d, want none without storage", len(claims), len(mounts))
				}
				return
			}

			if len(claims) != 1 {
				t.Fatalf("claims = %d, want 1", len(claims))
			}
			if got := claims[0].Spec.Resources.Requests.Storage().String(); got == "0" {
				t.Error("claim requests no storage")
			}
			// ReadWriteOnce: a second writer on a database's data directory is
			// corruption, not availability.
			if claims[0].Spec.AccessModes[0] != corev1.ReadWriteOnce {
				t.Errorf("accessMode = %v, want ReadWriteOnce", claims[0].Spec.AccessModes)
			}
			switch {
			case tt.wantClass == "" && claims[0].Spec.StorageClassName != nil:
				t.Errorf("storageClassName = %q, want unset so the cluster default applies", *claims[0].Spec.StorageClassName)
			case tt.wantClass != "" && (claims[0].Spec.StorageClassName == nil || *claims[0].Spec.StorageClassName != tt.wantClass):
				t.Errorf("storageClassName = %v, want %q", claims[0].Spec.StorageClassName, tt.wantClass)
			}

			if len(mounts) != 1 || mounts[0].MountPath != tt.wantMount {
				t.Errorf("mounts = %+v, want %s", mounts, tt.wantMount)
			}
			if mounts[0].Name != claims[0].Name {
				t.Errorf("mount %q does not reference the claim template %q", mounts[0].Name, claims[0].Name)
			}
		})
	}
}

// TestAppDeployDoesNotTouchAccessories is the property this feature exists for.
//
// An app rollout happens many times a day. If a single accessory object reached
// Render, every one of those rollouts would reconcile — and could restart — the
// database.
func TestAppDeployDoesNotTouchAccessories(t *testing.T) {
	target, req := accessoryRequest(t, accessoryBase)

	objs, err := target.Render(req)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	for _, obj := range objs {
		if obj.Kind == "StatefulSet" {
			t.Errorf("app render produced a StatefulSet (%s); accessories must not be deployed by a rollout", obj.Name)
		}
		if strings.HasPrefix(obj.Name, "web-postgres") || strings.HasPrefix(obj.Name, "web-redis") {
			t.Errorf("app render produced accessory object %s/%s", obj.Kind, obj.Name)
		}
	}

	// Stronger: the app's manifest must be byte-identical with and without any
	// accessories configured, so adding a database cannot perturb the app at all.
	withAccessories, err := target.ManifestYAML(req)
	if err != nil {
		t.Fatalf("ManifestYAML: %v", err)
	}
	plainTarget, plainReq := accessoryRequest(t, `
app: web
image: ghcr.io/acme/web
deploy:
  replicas: 3
  kubernetes: {namespace: acme}
env:
  secret: [DATABASE_URL]
`)
	without, err := plainTarget.ManifestYAML(plainReq)
	if err != nil {
		t.Fatalf("ManifestYAML: %v", err)
	}
	if withAccessories != without {
		t.Error("configuring an accessory changed the app's rendered manifest")
	}
}

// TestAccessoryPodTemplateIsStableAcrossReleases is the same property from the
// other side: even when accessories are explicitly reconciled, doing so during
// an unrelated app release must not produce a new pod template, because a new
// pod template is a restarted database.
func TestAccessoryPodTemplateIsStableAcrossReleases(t *testing.T) {
	target, req := accessoryRequest(t, accessoryBase)

	first, err := target.RenderAccessories(req)
	if err != nil {
		t.Fatalf("RenderAccessories: %v", err)
	}

	// A later release: new ID, new commit, new image digest, new timestamp.
	req.Release.ID = "deadbee-later"
	req.Release.Git.SHA = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	req.Release.Digest = "sha256:" + strings.Repeat("2", 64)
	second, err := target.RenderAccessories(req)
	if err != nil {
		t.Fatalf("RenderAccessories: %v", err)
	}

	a := findNamed(first, "StatefulSet", "web-postgres").Object.(*appsv1.StatefulSet)
	b := findNamed(second, "StatefulSet", "web-postgres").Object.(*appsv1.StatefulSet)

	if !equalPodTemplates(t, a.Spec.Template, b.Spec.Template) {
		t.Errorf("the accessory pod template changed with the app release:\n%v\n%v", a.Spec.Template, b.Spec.Template)
	}
	// The selector is immutable after creation, so a release-dependent value in it
	// would make the second reconcile fail permanently.
	for key, value := range a.Spec.Selector.MatchLabels {
		if strings.Contains(value, req.Release.ID) {
			t.Errorf("selector label %s pins a release (%q)", key, value)
		}
	}
}

// TestAccessoryPodsAreNotSelectedByTheApp guards the worst labeling mistake
// available here: the app's Service picking up the database's pod.
func TestAccessoryPodsAreNotSelectedByTheApp(t *testing.T) {
	target, req := accessoryRequest(t, accessoryBase)

	appObjs, err := target.Render(req)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	accObjs, err := target.RenderAccessories(req)
	if err != nil {
		t.Fatalf("RenderAccessories: %v", err)
	}

	podLabels := findNamed(accObjs, "StatefulSet", "web-postgres").Object.(*appsv1.StatefulSet).Spec.Template.Labels

	matches := func(selector map[string]string) bool {
		for k, v := range selector {
			if podLabels[k] != v {
				return false
			}
		}
		return true
	}

	svc := findObject(appObjs, "Service").Object.(*corev1.Service)
	if matches(svc.Spec.Selector) {
		t.Error("the app's Service selects accessory pods; HTTP traffic would be routed to the database")
	}
	dep := findObject(appObjs, "Deployment").Object.(*appsv1.Deployment)
	if matches(dep.Spec.Selector.MatchLabels) {
		t.Error("the app's Deployment selector matches accessory pods; it would try to own them")
	}

	// Still recognizably ours, and recognizably not the app.
	if podLabels[release.LabelManagedBy] != release.ManagedBy {
		t.Errorf("managed-by = %q, want %q", podLabels[release.LabelManagedBy], release.ManagedBy)
	}
	if podLabels[release.LabelComponent] != accessoryComponent {
		t.Errorf("component = %q, want %q", podLabels[release.LabelComponent], accessoryComponent)
	}
}

func TestAccessorySecretAndChecksum(t *testing.T) {
	target, req := accessoryRequest(t, accessoryBase)

	render := func(password string) (*corev1.Secret, *appsv1.StatefulSet) {
		req.Secrets["POSTGRES_PASSWORD"] = password
		objs, err := target.RenderAccessories(req)
		if err != nil {
			t.Fatalf("RenderAccessories: %v", err)
		}
		return findNamed(objs, "Secret", "web-postgres-env").Object.(*corev1.Secret),
			findNamed(objs, "StatefulSet", "web-postgres").Object.(*appsv1.StatefulSet)
	}

	secA, setA := render("first")
	_, setB := render("second")
	_, setC := render("first")

	if got := string(secA.Data["POSTGRES_PASSWORD"]); got != "first" {
		t.Errorf("secret value = %q, want the resolved value", got)
	}
	// The app's DATABASE_URL belongs to the app's Secret, not this one.
	if _, found := secA.Data["DATABASE_URL"]; found {
		t.Error("an accessory Secret must carry only the names that accessory declared")
	}

	sumA := setA.Spec.Template.Annotations[release.AnnotationConfigSum]
	sumB := setB.Spec.Template.Annotations[release.AnnotationConfigSum]
	sumC := setC.Spec.Template.Annotations[release.AnnotationConfigSum]
	if sumA == "" {
		t.Fatal("expected a config checksum on the accessory pod template")
	}
	// Without it the accessory would keep running with the stale credential.
	if sumA == sumB {
		t.Error("changing a secret value must change the checksum")
	}
	if sumA != sumC {
		t.Error("an unchanged secret must produce a stable checksum")
	}

	env := setA.Spec.Template.Spec.Containers[0]
	if len(env.EnvFrom) != 1 || env.EnvFrom[0].SecretRef.Name != "web-postgres-env" {
		t.Errorf("envFrom = %+v, want the accessory's own Secret", env.EnvFrom)
	}
}

func TestAccessoryMissingSecretFailsLoudly(t *testing.T) {
	target, req := accessoryRequest(t, accessoryBase)
	delete(req.Secrets, "POSTGRES_PASSWORD")

	_, err := target.RenderAccessories(req)
	if err == nil {
		// Preflight only covers the app's secrets, so silence here would mean a
		// Postgres that boots without a password and fails opaquely.
		t.Fatal("expected an error when an accessory secret is unset")
	}
	if !strings.Contains(err.Error(), "POSTGRES_PASSWORD") {
		t.Errorf("error = %v, want it to name the missing secret", err)
	}
}

// TestAccessorySecretValuesNeverReachPlanOutput mirrors the app-side guarantee:
// plan output lands in pull requests and CI logs.
func TestAccessorySecretValuesNeverReachPlanOutput(t *testing.T) {
	target, req := accessoryRequest(t, accessoryBase)

	toUnstructuredSecret := func(password string) *unstructured.Unstructured {
		req.Secrets["POSTGRES_PASSWORD"] = password
		objs, err := target.RenderAccessories(req)
		if err != nil {
			t.Fatalf("RenderAccessories: %v", err)
		}
		obj := findNamed(objs, "Secret", "web-postgres-env")
		u, err := toUnstructured(obj.Object, obj.GVK)
		if err != nil {
			t.Fatalf("toUnstructured: %v", err)
		}
		return u
	}

	live := toUnstructuredSecret("old-database-password")
	desired := toUnstructuredSecret("new-database-password")

	rendered := joinFields(fieldChanges("Secret", live, desired))
	if !strings.Contains(rendered, "POSTGRES_PASSWORD") {
		t.Errorf("expected the key name to be reported, got: %s", rendered)
	}
	for _, value := range []string{"old-database-password", "new-database-password"} {
		if strings.Contains(rendered, value) {
			t.Fatalf("SECRET VALUE LEAKED into plan output: %q appears in %s", value, rendered)
		}
	}
}

func TestAccessoryContainerDetails(t *testing.T) {
	target, req := accessoryRequest(t, accessoryBase)
	objs, err := target.RenderAccessories(req)
	if err != nil {
		t.Fatalf("RenderAccessories: %v", err)
	}

	redis := findNamed(objs, "StatefulSet", "web-redis").Object.(*appsv1.StatefulSet)
	c := redis.Spec.Template.Spec.Containers[0]

	// The image is deployed exactly as written: buidl did not build it and has no
	// release to pin it to.
	if c.Image != "redis:7" {
		t.Errorf("image = %q, want redis:7", c.Image)
	}
	if strings.Join(c.Command, " ") != "redis-server --appendonly yes" {
		t.Errorf("command = %v", c.Command)
	}
	if len(c.Ports) != 1 || c.Ports[0].ContainerPort != 6379 {
		t.Errorf("ports = %+v", c.Ports)
	}
	if *redis.Spec.Replicas != 1 {
		t.Errorf("replicas = %d, want 1; a second replica is a second primary", *redis.Spec.Replicas)
	}

	pg := findNamed(objs, "StatefulSet", "web-postgres").Object.(*appsv1.StatefulSet)
	pgc := pg.Spec.Template.Spec.Containers[0]
	if len(pgc.Env) != 1 || pgc.Env[0].Name != "POSTGRES_DB" {
		t.Errorf("env = %+v, want only the accessory's own clear vars", pgc.Env)
	}
	// The app's injected variables change every release; injecting them here
	// would restart the database on every deploy.
	for _, e := range pgc.Env {
		if strings.HasPrefix(e.Name, "BUIDL_") {
			t.Errorf("accessory env must not carry %s", e.Name)
		}
	}
	if pgc.SecurityContext == nil || pgc.SecurityContext.AllowPrivilegeEscalation == nil || *pgc.SecurityContext.AllowPrivilegeEscalation {
		t.Error("privilege escalation must be disabled")
	}
	// Deliberately not forced non-root: the canonical database images initialize
	// their data directory as root and drop privileges themselves.
	if pg.Spec.Template.Spec.SecurityContext != nil && pg.Spec.Template.Spec.SecurityContext.RunAsNonRoot != nil {
		t.Error("accessories must not force runAsNonRoot; it crash-loops the standard postgres image")
	}
}

func TestNoAccessoriesRendersNothing(t *testing.T) {
	target, req := accessoryRequest(t, renderBase)

	objs, err := target.RenderAccessories(req)
	if err != nil {
		t.Fatalf("RenderAccessories: %v", err)
	}
	if len(objs) != 0 {
		t.Errorf("rendered %d objects for a config with no accessories", len(objs))
	}
}

func TestAccessoryRenderIsDeterministic(t *testing.T) {
	target, req := accessoryRequest(t, accessoryBase)

	var first []string
	for range 5 {
		objs, err := target.RenderAccessories(req)
		if err != nil {
			t.Fatalf("RenderAccessories: %v", err)
		}
		var order []string
		for _, o := range objs {
			order = append(order, o.Kind+"/"+o.Name)
		}
		if first == nil {
			first = order
			continue
		}
		if strings.Join(order, ",") != strings.Join(first, ",") {
			t.Fatalf("apply order is unstable: %v then %v", first, order)
		}
	}
}

func TestAccessoryDoesNotRequireADigestPinnedRelease(t *testing.T) {
	target, req := accessoryRequest(t, accessoryBase)
	// An accessory image is not part of the app's release, so reconciling one
	// must work from a `promote` or an accessory-only run that never built.
	req.Release.Digest = ""

	if _, err := target.RenderAccessories(req); err != nil {
		t.Fatalf("RenderAccessories with no digest: %v", err)
	}
}

// TestAccessoryObjectsDecodeIntoTypedScheme is the offline equivalent of
// `kubectl apply --dry-run` for accessories: it proves the rendered objects are
// structurally valid against the real API types.
func TestAccessoryObjectsDecodeIntoTypedScheme(t *testing.T) {
	target, req := accessoryRequest(t, accessoryBase)
	objs, err := target.RenderAccessories(req)
	if err != nil {
		t.Fatalf("RenderAccessories: %v", err)
	}
	if len(objs) == 0 {
		t.Fatal("nothing rendered")
	}

	for _, obj := range objs {
		u, err := toUnstructured(obj.Object, obj.GVK)
		if err != nil {
			t.Fatalf("%s/%s: toUnstructured: %v", obj.Kind, obj.Name, err)
		}

		if u.GetAPIVersion() == "" || u.GetKind() == "" {
			t.Errorf("%s/%s: missing apiVersion or kind", obj.Kind, obj.Name)
		}
		if u.GetNamespace() == "" {
			t.Errorf("%s/%s: namespaced object has no namespace", obj.Kind, obj.Name)
		}

		typed, err := scheme.Scheme.New(obj.GVK)
		if err != nil {
			t.Fatalf("%s: unknown GVK %s: %v", obj.Kind, obj.GVK, err)
		}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, typed); err != nil {
			t.Errorf("%s/%s: does not decode into %s: %v", obj.Kind, obj.Name, obj.GVK, err)
		}

		if _, found, _ := unstructuredNestedFieldNoCopy(u.Object, "metadata", "creationTimestamp"); found {
			t.Errorf("%s/%s: creationTimestamp must be stripped before apply", obj.Kind, obj.Name)
		}
		if _, found, _ := unstructuredNestedFieldNoCopy(u.Object, "status"); found {
			t.Errorf("%s/%s: status must be stripped before apply", obj.Kind, obj.Name)
		}

		// A volumeClaimTemplate is a full PVC and carries the same null
		// creationTimestamp one level down, where the top-level strip cannot see it.
		claims, _, _ := unstructuredNestedFieldNoCopy(u.Object, "spec", "volumeClaimTemplates")
		for _, entry := range asSlice(claims) {
			claim, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			if _, found, _ := unstructuredNestedFieldNoCopy(claim, "metadata", "creationTimestamp"); found {
				t.Errorf("%s/%s: volumeClaimTemplate creationTimestamp must be stripped", obj.Kind, obj.Name)
			}
			if _, found, _ := unstructuredNestedFieldNoCopy(claim, "status"); found {
				t.Errorf("%s/%s: volumeClaimTemplate status must be stripped", obj.Kind, obj.Name)
			}
		}
	}
}

func TestAccessoryImpactIsPhrasedForStatefulWorkloads(t *testing.T) {
	tests := []struct {
		name   string
		action deploy.Action
		fields []deploy.FieldChange
		want   string
	}{
		{
			name:   "create",
			action: deploy.ActionCreate,
			want:   "creates the accessory and its storage",
		},
		{
			name:   "image change restarts",
			action: deploy.ActionUpdate,
			fields: []deploy.FieldChange{{Field: "image", From: "a", To: "b"}},
			want:   "restarts the accessory",
		},
		{
			name:   "secret change restarts",
			action: deploy.ActionUpdate,
			fields: []deploy.FieldChange{{Field: "secret values"}},
			want:   "restarts the accessory",
		},
		{
			name:   "metadata-only edit is inert",
			action: deploy.ActionUpdate,
			fields: []deploy.FieldChange{{Field: "strategy", From: "a", To: "b"}},
			want:   "no restart",
		},
		{
			name:   "unchanged says nothing",
			action: deploy.ActionUnchanged,
			want:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := impactOf("StatefulSet", tt.action, tt.fields, accessoryReplicas); got != tt.want {
				t.Errorf("impact = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAccessoryNamesStayWithinKubernetesLimit(t *testing.T) {
	cfg := &config.Config{App: strings.Repeat("a", 60)}
	name := accessoryName(cfg, "postgres-primary")
	if len(name) > 63 {
		t.Errorf("name length = %d, want <= 63", len(name))
	}
	if strings.HasSuffix(name, "-") {
		t.Errorf("name %q must not end with a dash", name)
	}
}

// equalPodTemplates compares two pod templates in the form they reach the API
// server, so the comparison sees exactly what would be applied.
func equalPodTemplates(t *testing.T, a, b corev1.PodTemplateSpec) bool {
	t.Helper()
	ma, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&a)
	if err != nil {
		t.Fatalf("converting pod template: %v", err)
	}
	mb, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&b)
	if err != nil {
		t.Fatalf("converting pod template: %v", err)
	}
	return reflect.DeepEqual(ma, mb)
}

func asSlice(v any) []any {
	out, _ := v.([]any)
	return out
}
