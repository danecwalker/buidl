package kubernetes

import (
	"context"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/danecwalker/buidl/internal/deploy"
	"github.com/danecwalker/buidl/internal/release"
)

// Shared timeout defaults, used when config leaves them unset.
const (
	defaultDeploy = 5 * time.Minute
	defaultDrain  = 30 * time.Second
)

// isNotFound reports whether err is a Kubernetes 404.
func isNotFound(err error) bool {
	return apierrors.IsNotFound(err)
}

// isConflict reports whether err is a server-side apply field conflict.
func isConflict(err error) bool {
	return apierrors.IsConflict(err)
}

// apply performs a server-side apply of one object.
//
// dryRun asks the API server to validate and merge the object, returning the
// result without persisting it. That is what makes `buidl plan` trustworthy: the
// diff is computed by the same admission and merge logic that a real apply would
// use, not by a client-side approximation.
func (t *Target) apply(ctx context.Context, obj Object, dryRun bool) (*unstructured.Unstructured, error) {
	u, err := toUnstructured(obj.Object, obj.GVK)
	if err != nil {
		return nil, err
	}

	gvr, namespaced, err := t.resourceFor(obj.GVK)
	if err != nil {
		return nil, err
	}

	ri := t.dynamic.Resource(gvr)
	var result *unstructured.Unstructured
	if namespaced {
		result, err = ri.Namespace(t.Namespace).Apply(ctx, obj.Name, u, applyOptions(dryRun))
	} else {
		result, err = ri.Apply(ctx, obj.Name, u, applyOptions(dryRun))
	}
	if err != nil {
		if isConflict(err) {
			return nil, fmt.Errorf("%s/%s is owned by another controller and conflicts with buidl: %w", obj.Kind, obj.Name, err)
		}
		return nil, fmt.Errorf("applying %s/%s: %w", obj.Kind, obj.Name, err)
	}
	return result, nil
}

// get fetches the live version of an object, or nil if absent.
func (t *Target) get(ctx context.Context, obj Object) (*unstructured.Unstructured, error) {
	gvr, namespaced, err := t.resourceFor(obj.GVK)
	if err != nil {
		return nil, err
	}

	ri := t.dynamic.Resource(gvr)
	var live *unstructured.Unstructured
	if namespaced {
		live, err = ri.Namespace(t.Namespace).Get(ctx, obj.Name, metav1.GetOptions{})
	} else {
		live, err = ri.Get(ctx, obj.Name, metav1.GetOptions{})
	}
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading %s/%s: %w", obj.Kind, obj.Name, err)
	}
	return live, nil
}

// Plan implements deploy.Target by dry-running every object and diffing the
// result against what is live.
func (t *Target) Plan(ctx context.Context, req deploy.Request) (*deploy.Plan, error) {
	objs, err := t.Render(req)
	if err != nil {
		return nil, err
	}

	plan := &deploy.Plan{
		Environment: req.Config.Environment,
		Release:     req.Release,
	}

	// Surface risks that are invisible in an object diff but matter to a
	// reviewer approving the change.
	if req.Release.Git.Dirty {
		plan.Warnings = append(plan.Warnings, "building from a dirty working tree; this release is not reproducible from a commit")
	}
	if replicas(req.Config) == 0 {
		plan.Warnings = append(plan.Warnings, "deploy.replicas is 0; this will take the app offline")
	}
	if exists, err := t.namespaceExists(ctx); err == nil && !exists && !req.Config.Deploy.Kubernetes.CreateNamespace {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("namespace %q does not exist; set deploy.kubernetes.createNamespace: true or create it first", t.Namespace))
	}

	desiredReplicas := replicas(req.Config)
	for _, obj := range objs {
		change, err := t.planObject(ctx, obj, desiredReplicas)
		if err != nil {
			return nil, err
		}
		plan.Changes = append(plan.Changes, change)
	}

	return plan, nil
}

// planObject computes the change for a single object.
func (t *Target) planObject(ctx context.Context, obj Object, replicas int32) (deploy.Change, error) {
	change := deploy.Change{Kind: obj.Kind, Name: obj.Name}

	live, err := t.get(ctx, obj)
	if err != nil {
		return change, err
	}

	if live == nil {
		change.Action = deploy.ActionCreate
		change.Summary = fmt.Sprintf("create %s/%s", obj.Kind, obj.Name)
		change.Impact = impactOf(obj.Kind, change.Action, nil, replicas)
		return change, nil
	}

	// Dry-run apply yields the object as the server would store it, including
	// defaulted and mutated fields. Diffing against that avoids the noise a
	// naive client-side comparison produces.
	merged, err := t.apply(ctx, obj, true)
	if err != nil {
		return change, err
	}

	diff, err := diffObjects(live, merged)
	if err != nil {
		return change, err
	}

	if diff == "" {
		change.Action = deploy.ActionUnchanged
		change.Summary = fmt.Sprintf("%s/%s unchanged", obj.Kind, obj.Name)
		return change, nil
	}

	change.Action = deploy.ActionUpdate
	change.Diff = diff
	change.Summary = fmt.Sprintf("update %s/%s", obj.Kind, obj.Name)
	// Field-level detail is what makes the plan reviewable; the raw diff stays
	// available behind --detailed.
	change.Fields = fieldChanges(obj.Kind, live, merged)
	change.Impact = impactOf(obj.Kind, change.Action, change.Fields, replicas)
	return change, nil
}

// diffObjects renders a unified diff between two objects, ignoring fields that
// the server manages and that would otherwise produce a diff on every run.
func diffObjects(live, desired *unstructured.Unstructured) (string, error) {
	a := normalizeForDiff(live)
	b := normalizeForDiff(desired)

	aYAML, err := yaml.Marshal(a)
	if err != nil {
		return "", err
	}
	bYAML, err := yaml.Marshal(b)
	if err != nil {
		return "", err
	}
	if string(aYAML) == string(bYAML) {
		return "", nil
	}
	return unifiedDiff(string(aYAML), string(bYAML)), nil
}

// serverManagedFields are set or updated by the API server and controllers, and
// are not part of the desired state buidl declares. Diffing them would report a
// change on every deploy.
var serverManagedFields = [][]string{
	{"metadata", "creationTimestamp"},
	{"metadata", "generation"},
	{"metadata", "resourceVersion"},
	{"metadata", "uid"},
	{"metadata", "managedFields"},
	{"metadata", "selfLink"},
	{"status"},
	// Written by the Deployment controller, not by us.
	{"metadata", "annotations", "deployment.kubernetes.io/revision"},
	// kubectl's legacy annotation, present on objects previously applied by hand.
	{"metadata", "annotations", "kubectl.kubernetes.io/last-applied-configuration"},
	// buidl stamps these on every release, so they always differ and say nothing
	// useful about the shape of the change.
	{"metadata", "annotations", release.AnnotationTime},
}

func normalizeForDiff(u *unstructured.Unstructured) map[string]any {
	if u == nil {
		return nil
	}
	copied := u.DeepCopy()
	for _, path := range serverManagedFields {
		unstructured.RemoveNestedField(copied.Object, path...)
	}
	// Pod template annotations carry the same per-release noise.
	unstructured.RemoveNestedField(copied.Object, "spec", "template", "metadata", "annotations", release.AnnotationTime)
	unstructured.RemoveNestedField(copied.Object, "spec", "template", "metadata", "creationTimestamp")
	return copied.Object
}

// unifiedDiff renders a minimal line-oriented diff.
//
// This is a deliberately simple implementation rather than a dependency: plan
// output is read by humans, and for the shapes of change a deploy produces
// (a handful of scalar edits inside a stable document) a longest-common-
// subsequence walk over lines is entirely sufficient.
func unifiedDiff(a, b string) string {
	aLines := strings.Split(strings.TrimRight(a, "\n"), "\n")
	bLines := strings.Split(strings.TrimRight(b, "\n"), "\n")

	// Build an LCS table. Object YAML is small enough that O(n*m) is fine.
	n, m := len(aLines), len(bLines)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if aLines[i] == bLines[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}

	type line struct {
		op   byte // ' ', '-', '+'
		text string
	}
	var lines []line
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case aLines[i] == bLines[j]:
			lines = append(lines, line{' ', aLines[i]})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			lines = append(lines, line{'-', aLines[i]})
			i++
		default:
			lines = append(lines, line{'+', bLines[j]})
			j++
		}
	}
	for ; i < n; i++ {
		lines = append(lines, line{'-', aLines[i]})
	}
	for ; j < m; j++ {
		lines = append(lines, line{'+', bLines[j]})
	}

	// Emit only changed regions plus a little context, so a two-field change in a
	// 200-line Deployment reads as a two-field change.
	const context = 2
	keep := make([]bool, len(lines))
	for idx, l := range lines {
		if l.op == ' ' {
			continue
		}
		for k := max(0, idx-context); k <= min(len(lines)-1, idx+context); k++ {
			keep[k] = true
		}
	}

	var b2 strings.Builder
	skipping := false
	for idx, l := range lines {
		if !keep[idx] {
			if !skipping {
				b2.WriteString("  ...\n")
				skipping = true
			}
			continue
		}
		skipping = false
		b2.WriteByte(l.op)
		b2.WriteByte(' ')
		b2.WriteString(l.text)
		b2.WriteByte('\n')
	}
	return b2.String()
}

// applyAll applies every object in order and returns what was done.
//
// On failure the changes applied so far are returned alongside the error, and the
// failing object is included with its Err set. That matters: apply is not atomic,
// so a mid-sequence failure leaves the namespace in a mixed state, and the only
// way a user can reason about it is to be told exactly which objects landed.
func (t *Target) applyAll(ctx context.Context, objs []Object, replicas int32) ([]deploy.Change, error) {
	changes := make([]deploy.Change, 0, len(objs))

	for _, obj := range objs {
		live, err := t.get(ctx, obj)
		if err != nil {
			changes = append(changes, deploy.Change{
				Kind: obj.Kind, Name: obj.Name, Err: err,
				Summary: fmt.Sprintf("could not read %s/%s", obj.Kind, obj.Name),
			})
			return changes, err
		}

		action := deploy.ActionUpdate
		if live == nil {
			action = deploy.ActionCreate
		}

		change := deploy.Change{
			Action:  action,
			Kind:    obj.Kind,
			Name:    obj.Name,
			Summary: fmt.Sprintf("%s %s/%s", action, obj.Kind, obj.Name),
		}

		applied, err := t.apply(ctx, obj, false)
		if err != nil {
			change.Err = err
			changes = append(changes, change)
			return changes, err
		}

		change.Applied = true
		if live != nil {
			// Report what actually changed, not merely that something did. This is
			// the record of "what was done".
			change.Fields = fieldChanges(obj.Kind, live, applied)
			change.Impact = impactOf(obj.Kind, action, change.Fields, replicas)
			if len(change.Fields) == 0 {
				// Server-side apply is idempotent, so an unchanged object is normal
				// and should not read as a modification.
				change.Action = deploy.ActionUnchanged
				change.Summary = fmt.Sprintf("%s/%s unchanged", obj.Kind, obj.Name)
			}
		} else {
			change.Impact = impactOf(obj.Kind, action, nil, replicas)
		}

		// Info, not Detail: what a deploy did to the cluster is the primary record
		// of the run and must not require --verbose to see.
		t.logChange(change)
		changes = append(changes, change)
	}

	return changes, nil
}

// logChange reports one applied object.
func (t *Target) logChange(change deploy.Change) {
	line := fmt.Sprintf("%-9s %s/%s", change.Action, change.Kind, change.Name)
	if summary := change.FieldSummary(); summary != "" {
		line += "  (" + summary + ")"
	}
	if change.Impact != "" {
		line += "  [" + change.Impact + "]"
	}
	t.log.Info("%s", line)
}

// ManifestYAML renders the desired state as a multi-document YAML stream.
//
// This is the escape hatch: `buidl manifest` output can be committed, reviewed,
// piped to kubectl, or handed to Argo CD. A deploy tool that cannot show you
// exactly what it will submit is asking for more trust than it deserves.
func (t *Target) ManifestYAML(req deploy.Request) (string, error) {
	objs, err := t.Render(req)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	for _, obj := range objs {
		u, err := toUnstructured(obj.Object, obj.GVK)
		if err != nil {
			return "", err
		}
		data, err := yaml.Marshal(u.Object)
		if err != nil {
			return "", err
		}
		b.WriteString("---\n")
		fmt.Fprintf(&b, "# %s/%s\n", obj.Kind, obj.Name)
		b.Write(data)
	}
	return b.String(), nil
}
