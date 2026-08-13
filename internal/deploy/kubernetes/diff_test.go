package kubernetes

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/danecwalker/buidl/internal/release"
)

func TestUnifiedDiffDetectsChangedLine(t *testing.T) {
	a := "spec:\n  replicas: 2\n  port: 80\n"
	b := "spec:\n  replicas: 3\n  port: 80\n"

	diff := unifiedDiff(a, b)

	if !strings.Contains(diff, "-   replicas: 2") {
		t.Errorf("expected the old line to be marked removed:\n%s", diff)
	}
	if !strings.Contains(diff, "+   replicas: 3") {
		t.Errorf("expected the new line to be marked added:\n%s", diff)
	}
	// Unchanged context should be present but unmarked.
	if !strings.Contains(diff, "   port: 80") {
		t.Errorf("expected surrounding context:\n%s", diff)
	}
}

func TestUnifiedDiffEmptyForIdenticalInput(t *testing.T) {
	// unifiedDiff is only called when the documents differ, but an all-context
	// result must still contain no +/- markers.
	diff := unifiedDiff("a: 1\nb: 2\n", "a: 1\nb: 2\n")
	for _, line := range strings.Split(diff, "\n") {
		if strings.HasPrefix(line, "+") || strings.HasPrefix(line, "-") {
			t.Errorf("identical input produced a change marker: %q", line)
		}
	}
}

func TestUnifiedDiffElidesUnchangedRegions(t *testing.T) {
	// A one-field change inside a large object must read as a one-field change,
	// not as a 200-line dump.
	var a, b strings.Builder
	for i := range 100 {
		a.WriteString("field")
		b.WriteString("field")
		a.WriteByte(byte('0' + i%10))
		b.WriteByte(byte('0' + i%10))
		a.WriteString(": same\n")
		b.WriteString(": same\n")
	}
	a.WriteString("changed: before\n")
	b.WriteString("changed: after\n")

	diff := unifiedDiff(a.String(), b.String())

	if !strings.Contains(diff, "...") {
		t.Errorf("expected unchanged regions to be elided:\n%s", diff)
	}
	if lines := strings.Count(diff, "\n"); lines > 12 {
		t.Errorf("diff should be compact, got %d lines:\n%s", lines, diff)
	}
	if !strings.Contains(diff, "changed: after") {
		t.Errorf("the actual change must be shown:\n%s", diff)
	}
}

func TestUnifiedDiffHandlesAdditionsAndRemovals(t *testing.T) {
	// Each line is rendered as the op character, a space, then the text.
	diff := unifiedDiff("a: 1\n", "a: 1\nb: 2\nc: 3\n")
	if !strings.Contains(diff, "+ b: 2") || !strings.Contains(diff, "+ c: 3") {
		t.Errorf("expected pure additions:\n%s", diff)
	}

	diff = unifiedDiff("a: 1\nb: 2\n", "a: 1\n")
	if !strings.Contains(diff, "- b: 2") {
		t.Errorf("expected a removal:\n%s", diff)
	}
}

// TestDiffObjectsIgnoresServerManagedFields is the property that keeps `plan`
// honest: a re-plan of an unchanged release must report no changes.
func TestDiffObjectsIgnoresServerManagedFields(t *testing.T) {
	live := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]any{
			"name":              "web",
			"namespace":         "acme",
			"resourceVersion":   "12345",
			"generation":        int64(7),
			"uid":               "abc-123",
			"creationTimestamp": "2026-01-01T00:00:00Z",
			"managedFields":     []any{map[string]any{"manager": "buidl"}},
			"annotations": map[string]any{
				"deployment.kubernetes.io/revision": "3",
				release.AnnotationTime:              "2026-01-01T00:00:00Z",
				release.AnnotationRelease:           "abc123-xyz",
			},
		},
		"spec":   map[string]any{"replicas": int64(3)},
		"status": map[string]any{"readyReplicas": int64(3)},
	}}

	desired := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]any{
			"name":            "web",
			"namespace":       "acme",
			"resourceVersion": "99999",
			"generation":      int64(9),
			"uid":             "abc-123",
			"managedFields":   []any{map[string]any{"manager": "kubectl"}},
			"annotations": map[string]any{
				"deployment.kubernetes.io/revision": "4",
				// A fresh deploy timestamp must not register as a change.
				release.AnnotationTime:    "2026-08-13T00:00:00Z",
				release.AnnotationRelease: "abc123-xyz",
			},
		},
		"spec": map[string]any{"replicas": int64(3)},
	}}

	diff, err := diffObjects(live, desired)
	if err != nil {
		t.Fatalf("diffObjects: %v", err)
	}
	if diff != "" {
		t.Errorf("server-managed fields should not produce a diff, got:\n%s", diff)
	}
}

func TestDiffObjectsReportsRealChange(t *testing.T) {
	base := func(replicas int64) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"metadata":   map[string]any{"name": "web"},
			"spec":       map[string]any{"replicas": replicas},
		}}
	}

	diff, err := diffObjects(base(2), base(5))
	if err != nil {
		t.Fatalf("diffObjects: %v", err)
	}
	if diff == "" {
		t.Fatal("expected a diff for a changed replica count")
	}
	if !strings.Contains(diff, "5") {
		t.Errorf("diff should show the new value:\n%s", diff)
	}
}

func TestNormalizeForDiffDoesNotMutateInput(t *testing.T) {
	// diffObjects is called with live objects that the caller may reuse.
	original := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "web", "resourceVersion": "1"},
		"status":   map[string]any{"ready": true},
	}}

	normalizeForDiff(original)

	if _, found, _ := unstructured.NestedString(original.Object, "metadata", "resourceVersion"); !found {
		t.Error("normalizeForDiff must not mutate its input")
	}
	if _, found, _ := unstructured.NestedBool(original.Object, "status", "ready"); !found {
		t.Error("normalizeForDiff must not strip status from its input")
	}
}
