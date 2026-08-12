package kubernetes

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/danewalker/buidl/internal/deploy"
	"github.com/danewalker/buidl/internal/release"
)

// deployment builds a minimal Deployment as unstructured, for diffing.
func deployment(image string, replicas int64, env map[string]string, mutate ...func(map[string]any)) *unstructured.Unstructured {
	envList := make([]any, 0, len(env))
	// Sorted for a stable fixture; envMap keys by name so order does not matter.
	for _, name := range sortedKeys(env) {
		envList = append(envList, map[string]any{"name": name, "value": env[name]})
	}

	container := map[string]any{
		"name":  "web",
		"image": image,
		"env":   envList,
		"ports": []any{map[string]any{"containerPort": int64(3000), "name": "http"}},
		"resources": map[string]any{
			"requests": map[string]any{"cpu": "100m", "memory": "128Mi"},
		},
		"readinessProbe": map[string]any{
			"httpGet": map[string]any{"path": "/up"},
		},
	}

	obj := map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]any{"name": "web"},
		"spec": map[string]any{
			"replicas": replicas,
			"strategy": map[string]any{
				"type":          "RollingUpdate",
				"rollingUpdate": map[string]any{"maxSurge": "25%", "maxUnavailable": "0"},
			},
			"template": map[string]any{
				"metadata": map[string]any{"annotations": map[string]any{}},
				"spec": map[string]any{
					"containers":         []any{container},
					"serviceAccountName": "web",
				},
			},
		},
	}
	for _, m := range mutate {
		m(obj)
	}
	return &unstructured.Unstructured{Object: obj}
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// findField locates a change by field name.
func findField(changes []deploy.FieldChange, name string) *deploy.FieldChange {
	for i := range changes {
		if changes[i].Field == name {
			return &changes[i]
		}
	}
	return nil
}

func TestImageChangeIsReported(t *testing.T) {
	live := deployment("ghcr.io/acme/web@sha256:"+strings.Repeat("a", 64), 2, nil)
	desired := deployment("ghcr.io/acme/web@sha256:"+strings.Repeat("b", 64), 2, nil)

	changes := fieldChanges("Deployment", live, desired)

	image := findField(changes, "image")
	if image == nil {
		t.Fatalf("expected an image change, got %v", changes)
	}
	// Abbreviated, since the repository is identical and only the digest differs.
	if !strings.HasPrefix(image.From, "sha256:aaaa") || !strings.HasPrefix(image.To, "sha256:bbbb") {
		t.Errorf("image change = %s", image)
	}
	if len(image.From) > 25 || len(image.To) > 25 {
		t.Errorf("digests should be abbreviated for display: %s", image)
	}
}

func TestReplicaChangeIsReported(t *testing.T) {
	changes := fieldChanges("Deployment", deployment("img", 2, nil), deployment("img", 5, nil))

	replicas := findField(changes, "replicas")
	if replicas == nil {
		t.Fatalf("expected a replicas change, got %v", changes)
	}
	if replicas.From != "2" || replicas.To != "5" {
		t.Errorf("replicas change = %s", replicas)
	}
}

func TestEnvChangesReportedByName(t *testing.T) {
	live := deployment("img", 1, map[string]string{"KEEP": "same", "CHANGED": "old", "GONE": "x"})
	desired := deployment("img", 1, map[string]string{"KEEP": "same", "CHANGED": "new", "ADDED": "y"})

	changes := fieldChanges("Deployment", live, desired)

	added := findField(changes, "env added")
	if added == nil || added.To != "ADDED" {
		t.Errorf("env added = %v", added)
	}
	removed := findField(changes, "env removed")
	if removed == nil || removed.From != "GONE" {
		t.Errorf("env removed = %v", removed)
	}
	modified := findField(changes, "env changed")
	if modified == nil || modified.To != "CHANGED" {
		t.Errorf("env changed = %v", modified)
	}
	// An unchanged variable must not appear anywhere.
	if strings.Contains(joinFields(changes), "KEEP") {
		t.Errorf("unchanged env should not be reported: %v", changes)
	}
}

func TestNoChangesForIdenticalObjects(t *testing.T) {
	live := deployment("img", 3, map[string]string{"A": "1"})
	desired := deployment("img", 3, map[string]string{"A": "1"})

	if changes := fieldChanges("Deployment", live, desired); len(changes) != 0 {
		t.Errorf("identical objects should produce no field changes, got %v", changes)
	}
}

func TestSecretChecksumSurfacesAsValueChange(t *testing.T) {
	withSum := func(sum string) func(map[string]any) {
		return func(obj map[string]any) {
			template := obj["spec"].(map[string]any)["template"].(map[string]any)
			template["metadata"].(map[string]any)["annotations"] = map[string]any{
				release.AnnotationConfigSum: sum,
			}
		}
	}

	live := deployment("img", 1, nil, withSum("aaa"))
	desired := deployment("img", 1, nil, withSum("bbb"))

	changes := fieldChanges("Deployment", live, desired)

	// The checksum itself is meaningless to a user; the fact that secrets changed
	// is why pods will restart.
	sum := findField(changes, "secret values")
	if sum == nil {
		t.Fatalf("expected a secret values change, got %v", changes)
	}
	if strings.Contains(joinFields(changes), "aaa") || strings.Contains(joinFields(changes), "bbb") {
		t.Errorf("the raw checksum should not be shown: %v", changes)
	}
}

// TestSecretValuesAreNeverReported is the security-critical test in this file.
//
// Plan output is routinely pasted into pull requests and CI logs, so a leaked
// secret value there is exposed far more widely than the Secret itself ever is.
func TestSecretValuesAreNeverReported(t *testing.T) {
	secret := func(data map[string]string) *unstructured.Unstructured {
		d := map[string]any{}
		for k, v := range data {
			d[k] = v
		}
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "Secret",
			"metadata":   map[string]any{"name": "web-env"},
			"data":       d,
		}}
	}

	// Base64-ish values standing in for real credentials.
	live := secret(map[string]string{
		"DATABASE_URL": "cG9zdGdyZXM6Ly9vbGQtc2VjcmV0",
		"OLD_KEY":      "b2xkLXZhbHVl",
	})
	desired := secret(map[string]string{
		"DATABASE_URL": "cG9zdGdyZXM6Ly9uZXctc2VjcmV0",
		"NEW_KEY":      "bmV3LXZhbHVl",
	})

	changes := fieldChanges("Secret", live, desired)
	rendered := joinFields(changes)

	// Key names are safe and useful.
	for _, keyName := range []string{"DATABASE_URL", "NEW_KEY", "OLD_KEY"} {
		if !strings.Contains(rendered, keyName) {
			t.Errorf("expected key name %q to be reported, got: %s", keyName, rendered)
		}
	}
	// Values are not.
	for _, value := range []string{
		"cG9zdGdyZXM6Ly9vbGQtc2VjcmV0",
		"cG9zdGdyZXM6Ly9uZXctc2VjcmV0",
		"b2xkLXZhbHVl",
		"bmV3LXZhbHVl",
	} {
		if strings.Contains(rendered, value) {
			t.Fatalf("SECRET VALUE LEAKED into plan output: %q appears in %s", value, rendered)
		}
	}
}

func TestServiceSelectorChangeIsFlagged(t *testing.T) {
	svc := func(releaseID string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "Service",
			"metadata":   map[string]any{"name": "web"},
			"spec": map[string]any{
				"type":     "ClusterIP",
				"selector": map[string]any{release.LabelRelease: releaseID},
				"ports":    []any{map[string]any{"port": int64(80), "targetPort": "http"}},
			},
		}}
	}

	changes := fieldChanges("Service", svc("old-release"), svc("new-release"))

	// This is the blue-green cutover, so it deserves a prominent line.
	serving := findField(changes, "serving release")
	if serving == nil {
		t.Fatalf("expected a serving release change, got %v", changes)
	}
	if serving.From != "old-release" || serving.To != "new-release" {
		t.Errorf("serving release = %s", serving)
	}
}

func TestIngressChangesReported(t *testing.T) {
	ingress := func(host string, tls bool) *unstructured.Unstructured {
		obj := map[string]any{
			"apiVersion": "networking.k8s.io/v1",
			"kind":       "Ingress",
			"metadata":   map[string]any{"name": "web", "annotations": map[string]any{}},
			"spec": map[string]any{
				"rules": []any{map[string]any{"host": host}},
			},
		}
		if tls {
			obj["spec"].(map[string]any)["tls"] = []any{map[string]any{"hosts": []any{host}}}
		}
		return &unstructured.Unstructured{Object: obj}
	}

	changes := fieldChanges("Ingress", ingress("old.acme.com", false), ingress("new.acme.com", true))

	hosts := findField(changes, "hosts")
	if hosts == nil || hosts.From != "old.acme.com" || hosts.To != "new.acme.com" {
		t.Errorf("hosts change = %v", hosts)
	}
	tls := findField(changes, "tls")
	if tls == nil || tls.From != "disabled" || tls.To != "enabled" {
		t.Errorf("tls change = %v", tls)
	}
}

func TestImpactDistinguishesRestartingChanges(t *testing.T) {
	tests := []struct {
		name   string
		fields []deploy.FieldChange
		want   string
	}{
		{
			name:   "image change replaces instances",
			fields: []deploy.FieldChange{{Field: "image", From: "a", To: "b"}},
			want:   "replaces 3 instances",
		},
		{
			name:   "secret change replaces instances",
			fields: []deploy.FieldChange{{Field: "secret values"}},
			want:   "replaces 3 instances",
		},
		{
			name:   "env change replaces instances",
			fields: []deploy.FieldChange{{Field: "env changed", To: "LOG_LEVEL"}},
			want:   "replaces 3 instances",
		},
		{
			name:   "resource change replaces instances",
			fields: []deploy.FieldChange{{Field: "memory limit", From: "512Mi", To: "1Gi"}},
			want:   "replaces 3 instances",
		},
		{
			// Scaling adds or removes pods without replacing the existing ones.
			name:   "replica change scales",
			fields: []deploy.FieldChange{{Field: "replicas", From: "3", To: "5"}},
			want:   "scales to 5",
		},
		{
			// A label-only edit is inert, and saying so prevents needless worry.
			name:   "inert change does not restart",
			fields: []deploy.FieldChange{{Field: "strategy", From: "RollingUpdate", To: "Recreate"}},
			want:   "no restart",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := impactOf("Deployment", deploy.ActionUpdate, tt.fields, 3)
			if got != tt.want {
				t.Errorf("impact = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestImpactForCreates(t *testing.T) {
	if got := impactOf("Deployment", deploy.ActionCreate, nil, 3); got != "starts 3 instances" {
		t.Errorf("Deployment create impact = %q", got)
	}
	if got := impactOf("Deployment", deploy.ActionCreate, nil, 1); got != "starts 1 instance" {
		t.Errorf("singular form = %q", got)
	}
	if got := impactOf("Ingress", deploy.ActionCreate, nil, 1); got != "publishes externally" {
		t.Errorf("Ingress create impact = %q", got)
	}
	if got := impactOf("Namespace", deploy.ActionCreate, nil, 1); got != "creates the namespace" {
		t.Errorf("Namespace create impact = %q", got)
	}
}

func TestImpactOfUnchangedIsEmpty(t *testing.T) {
	if got := impactOf("Deployment", deploy.ActionUnchanged, nil, 3); got != "" {
		t.Errorf("unchanged impact = %q, want empty", got)
	}
}

func TestServiceCutoverImpact(t *testing.T) {
	fields := []deploy.FieldChange{{Field: "serving release", From: "a", To: "b"}}
	if got := impactOf("Service", deploy.ActionUpdate, fields, 3); got != "switches live traffic" {
		t.Errorf("impact = %q", got)
	}
}

func TestFieldChangeString(t *testing.T) {
	f := deploy.FieldChange{Field: "replicas", From: "2", To: "5"}
	if got := f.String(); got != "replicas: 2 → 5" {
		t.Errorf("String = %q", got)
	}
	// An added value has no prior state; "(unset)" is clearer than an empty gap.
	added := deploy.FieldChange{Field: "env added", To: "NEW"}
	if got := added.String(); !strings.Contains(got, "(unset)") {
		t.Errorf("String = %q, want it to mark the missing side", got)
	}
}

func TestUnknownKindProducesNoFieldChanges(t *testing.T) {
	// A kind buidl does not model should degrade to the raw diff rather than
	// inventing field names.
	obj := &unstructured.Unstructured{Object: map[string]any{
		"kind": "CustomThing",
		"spec": map[string]any{"whatever": "x"},
	}}
	if changes := fieldChanges("CustomThing", obj, obj); len(changes) != 0 {
		t.Errorf("expected no field changes for an unmodeled kind, got %v", changes)
	}
}

func TestNilObjectsAreSafe(t *testing.T) {
	if changes := fieldChanges("Deployment", nil, nil); changes != nil {
		t.Errorf("nil inputs should produce no changes, got %v", changes)
	}
}

func joinFields(changes []deploy.FieldChange) string {
	parts := make([]string, 0, len(changes))
	for _, c := range changes {
		parts = append(parts, c.String())
	}
	return strings.Join(parts, " | ")
}
