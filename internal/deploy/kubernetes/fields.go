package kubernetes

import (
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/danewalker/buidl/internal/deploy"
	"github.com/danewalker/buidl/internal/release"
)

// fieldChanges extracts the meaningful differences between the live and desired
// forms of an object.
//
// This is deliberately a curated list per kind rather than a generic tree walk. A
// generic walk reports every server-defaulted field and buries the two lines that
// matter; naming the fields users actually set keeps a plan reviewable.
func fieldChanges(kind string, live, desired *unstructured.Unstructured) []deploy.FieldChange {
	if live == nil || desired == nil {
		return nil
	}

	switch kind {
	case "Deployment":
		return deploymentFieldChanges(live, desired)
	case "Service":
		return serviceFieldChanges(live, desired)
	case "Ingress":
		return ingressFieldChanges(live, desired)
	case "HorizontalPodAutoscaler":
		return hpaFieldChanges(live, desired)
	case "Secret":
		return secretFieldChanges(live, desired)
	case "PodDisruptionBudget":
		return compare(live, desired, []fieldSpec{
			{"minAvailable", []string{"spec", "minAvailable"}},
			{"maxUnavailable", []string{"spec", "maxUnavailable"}},
		})
	default:
		return nil
	}
}

// fieldSpec maps a readable name to a path in an unstructured object.
type fieldSpec struct {
	name string
	path []string
}

// compare diffs a list of scalar paths.
func compare(live, desired *unstructured.Unstructured, specs []fieldSpec) []deploy.FieldChange {
	var out []deploy.FieldChange
	for _, spec := range specs {
		from := scalarAt(live, spec.path...)
		to := scalarAt(desired, spec.path...)
		if from != to {
			out = append(out, deploy.FieldChange{Field: spec.name, From: from, To: to})
		}
	}
	return out
}

func deploymentFieldChanges(live, desired *unstructured.Unstructured) []deploy.FieldChange {
	changes := compare(live, desired, []fieldSpec{
		{"replicas", []string{"spec", "replicas"}},
		{"strategy", []string{"spec", "strategy", "type"}},
		{"maxSurge", []string{"spec", "strategy", "rollingUpdate", "maxSurge"}},
		{"maxUnavailable", []string{"spec", "strategy", "rollingUpdate", "maxUnavailable"}},
		{"serviceAccount", []string{"spec", "template", "spec", "serviceAccountName"}},
		{"gracePeriod", []string{"spec", "template", "spec", "terminationGracePeriodSeconds"}},
	})

	liveC := firstContainer(live)
	desiredC := firstContainer(desired)
	if liveC == nil || desiredC == nil {
		return changes
	}

	// The image is the single most important line in any deploy plan.
	if from, to := imageOf(liveC), imageOf(desiredC); from != to {
		changes = append(changes, deploy.FieldChange{
			Field: "image",
			From:  abbreviateImage(from),
			To:    abbreviateImage(to),
		})
	}

	changes = append(changes, compare(
		&unstructured.Unstructured{Object: liveC},
		&unstructured.Unstructured{Object: desiredC},
		[]fieldSpec{
			{"cpu request", []string{"resources", "requests", "cpu"}},
			{"memory request", []string{"resources", "requests", "memory"}},
			{"cpu limit", []string{"resources", "limits", "cpu"}},
			{"memory limit", []string{"resources", "limits", "memory"}},
			{"health path", []string{"readinessProbe", "httpGet", "path"}},
			{"command", []string{"command"}},
		})...)

	// Ports are worth naming explicitly: changing one silently breaks the Service.
	if from, to := containerPort(liveC), containerPort(desiredC); from != to {
		changes = append(changes, deploy.FieldChange{Field: "port", From: from, To: to})
	}

	changes = append(changes, envChanges(liveC, desiredC)...)

	// The secret checksum is an implementation detail, but the fact that secret
	// values changed is not — it is why pods will restart.
	liveSum := annotationAt(live, "spec", "template")[release.AnnotationConfigSum]
	desiredSum := annotationAt(desired, "spec", "template")[release.AnnotationConfigSum]
	if liveSum != desiredSum && desiredSum != "" {
		changes = append(changes, deploy.FieldChange{
			Field: "secret values",
			From:  "(previous)",
			To:    "(changed)",
		})
	}

	return changes
}

// envChanges reports added, removed and modified environment variables by name.
func envChanges(liveC, desiredC map[string]any) []deploy.FieldChange {
	liveEnv := envMap(liveC)
	desiredEnv := envMap(desiredC)

	var added, removed, modified []string
	for name, value := range desiredEnv {
		old, existed := liveEnv[name]
		switch {
		case !existed:
			added = append(added, name)
		case old != value:
			modified = append(modified, name)
		}
	}
	for name := range liveEnv {
		if _, still := desiredEnv[name]; !still {
			removed = append(removed, name)
		}
	}

	sort.Strings(added)
	sort.Strings(removed)
	sort.Strings(modified)

	var out []deploy.FieldChange
	if len(added) > 0 {
		out = append(out, deploy.FieldChange{Field: "env added", To: strings.Join(added, ", ")})
	}
	if len(removed) > 0 {
		out = append(out, deploy.FieldChange{Field: "env removed", From: strings.Join(removed, ", ")})
	}
	if len(modified) > 0 {
		out = append(out, deploy.FieldChange{Field: "env changed", To: strings.Join(modified, ", ")})
	}
	return out
}

func serviceFieldChanges(live, desired *unstructured.Unstructured) []deploy.FieldChange {
	changes := compare(live, desired, []fieldSpec{
		{"type", []string{"spec", "type"}},
	})

	// A selector change is how blue-green cuts traffic over, so it deserves a
	// prominent line rather than being buried in a diff.
	liveSel, _, _ := unstructured.NestedStringMap(live.Object, "spec", "selector")
	desiredSel, _, _ := unstructured.NestedStringMap(desired.Object, "spec", "selector")
	if from, to := liveSel[release.LabelRelease], desiredSel[release.LabelRelease]; from != to {
		changes = append(changes, deploy.FieldChange{Field: "serving release", From: from, To: to})
	}

	if from, to := servicePorts(live), servicePorts(desired); from != to {
		changes = append(changes, deploy.FieldChange{Field: "ports", From: from, To: to})
	}
	return changes
}

func ingressFieldChanges(live, desired *unstructured.Unstructured) []deploy.FieldChange {
	changes := compare(live, desired, []fieldSpec{
		{"ingressClass", []string{"spec", "ingressClassName"}},
	})

	if from, to := ingressHosts(live), ingressHosts(desired); from != to {
		changes = append(changes, deploy.FieldChange{Field: "hosts", From: from, To: to})
	}

	liveTLS := len(nestedSlice(live, "spec", "tls")) > 0
	desiredTLS := len(nestedSlice(desired, "spec", "tls")) > 0
	if liveTLS != desiredTLS {
		changes = append(changes, deploy.FieldChange{
			Field: "tls",
			From:  boolWord(liveTLS),
			To:    boolWord(desiredTLS),
		})
	}

	liveIssuer := live.GetAnnotations()["cert-manager.io/cluster-issuer"]
	desiredIssuer := desired.GetAnnotations()["cert-manager.io/cluster-issuer"]
	if liveIssuer != desiredIssuer {
		changes = append(changes, deploy.FieldChange{Field: "certIssuer", From: liveIssuer, To: desiredIssuer})
	}
	return changes
}

func hpaFieldChanges(live, desired *unstructured.Unstructured) []deploy.FieldChange {
	return compare(live, desired, []fieldSpec{
		{"minReplicas", []string{"spec", "minReplicas"}},
		{"maxReplicas", []string{"spec", "maxReplicas"}},
	})
}

// secretFieldChanges reports which keys changed, never any values.
//
// A plan is routinely pasted into a pull request or a CI log, so printing a
// secret value here would leak it far more widely than the Secret itself ever is.
func secretFieldChanges(live, desired *unstructured.Unstructured) []deploy.FieldChange {
	liveData, _, _ := unstructured.NestedStringMap(live.Object, "data")
	desiredData, _, _ := unstructured.NestedStringMap(desired.Object, "data")

	var added, removed, changed []string
	for key, value := range desiredData {
		old, existed := liveData[key]
		switch {
		case !existed:
			added = append(added, key)
		case old != value:
			// Only the key name is reported; the differing values stay unread.
			changed = append(changed, key)
		}
	}
	for key := range liveData {
		if _, still := desiredData[key]; !still {
			removed = append(removed, key)
		}
	}

	sort.Strings(added)
	sort.Strings(removed)
	sort.Strings(changed)

	var out []deploy.FieldChange
	if len(added) > 0 {
		out = append(out, deploy.FieldChange{Field: "keys added", To: strings.Join(added, ", ")})
	}
	if len(removed) > 0 {
		out = append(out, deploy.FieldChange{Field: "keys removed", From: strings.Join(removed, ", ")})
	}
	if len(changed) > 0 {
		out = append(out, deploy.FieldChange{Field: "values changed", To: strings.Join(changed, ", ")})
	}
	return out
}

// impactOf describes the runtime consequence of a change, so a reviewer can tell
// a pod-restarting change from an inert one.
func impactOf(kind string, action deploy.Action, fields []deploy.FieldChange, replicas int32) string {
	if action == deploy.ActionUnchanged {
		return ""
	}

	switch kind {
	case "Deployment":
		if action == deploy.ActionCreate {
			return fmt.Sprintf("starts %s", pluralInstances(replicas))
		}
		// Any pod-template field forces new pods; metadata-only edits do not.
		for _, f := range fields {
			switch f.Field {
			case "image", "port", "command", "secret values",
				"env added", "env removed", "env changed",
				"cpu request", "memory request", "cpu limit", "memory limit",
				"health path", "serviceAccount", "gracePeriod":
				return fmt.Sprintf("replaces %s", pluralInstances(replicas))
			}
		}
		for _, f := range fields {
			if f.Field == "replicas" {
				return fmt.Sprintf("scales to %s", f.To)
			}
		}
		return "no restart"

	case "Service":
		for _, f := range fields {
			if f.Field == "serving release" {
				return "switches live traffic"
			}
			if f.Field == "ports" || f.Field == "type" {
				return "affects routing"
			}
		}

	case "Ingress":
		if action == deploy.ActionCreate {
			return "publishes externally"
		}
		for _, f := range fields {
			if f.Field == "hosts" || f.Field == "tls" {
				return "changes external routing"
			}
		}

	case "Secret":
		if len(fields) > 0 {
			return "triggers a rollout"
		}

	case "Namespace":
		if action == deploy.ActionCreate {
			return "creates the namespace"
		}

	case "HorizontalPodAutoscaler":
		if action == deploy.ActionCreate {
			return "takes over scaling"
		}
	}
	return ""
}

func pluralInstances(n int32) string {
	if n == 1 {
		return "1 instance"
	}
	return fmt.Sprintf("%d instances", n)
}

// --- unstructured helpers ---------------------------------------------------

// scalarAt renders the value at a path as a string, or "" when absent.
func scalarAt(u *unstructured.Unstructured, path ...string) string {
	value, found, err := unstructured.NestedFieldNoCopy(u.Object, path...)
	if err != nil || !found || value == nil {
		return ""
	}
	switch v := value.(type) {
	case string:
		return v
	case []any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			parts = append(parts, fmt.Sprintf("%v", item))
		}
		return strings.Join(parts, " ")
	default:
		return fmt.Sprintf("%v", value)
	}
}

func nestedSlice(u *unstructured.Unstructured, path ...string) []any {
	value, found, err := unstructured.NestedSlice(u.Object, path...)
	if err != nil || !found {
		return nil
	}
	return value
}

// firstContainer returns the primary container's map, which is the app container.
func firstContainer(u *unstructured.Unstructured) map[string]any {
	containers := nestedSlice(u, "spec", "template", "spec", "containers")
	if len(containers) == 0 {
		return nil
	}
	c, ok := containers[0].(map[string]any)
	if !ok {
		return nil
	}
	return c
}

func imageOf(container map[string]any) string {
	if image, ok := container["image"].(string); ok {
		return image
	}
	return ""
}

// abbreviateImage shortens a digest reference for display, keeping the repository
// and enough of the digest to identify it.
func abbreviateImage(ref string) string {
	repo, digest, found := strings.Cut(ref, "@sha256:")
	if !found {
		return ref
	}
	if len(digest) > 12 {
		digest = digest[:12]
	}
	// The repository is identical across a normal deploy, so lead with the part
	// that actually differs.
	_ = repo
	return "sha256:" + digest
}

func containerPort(container map[string]any) string {
	ports, ok := container["ports"].([]any)
	if !ok || len(ports) == 0 {
		return ""
	}
	first, ok := ports[0].(map[string]any)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%v", first["containerPort"])
}

// envMap flattens a container's env into name→value, ignoring valueFrom entries
// whose values live elsewhere.
func envMap(container map[string]any) map[string]string {
	out := map[string]string{}
	entries, ok := container["env"].([]any)
	if !ok {
		return out
	}
	for _, entry := range entries {
		e, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		name, _ := e["name"].(string)
		if name == "" {
			continue
		}
		if value, ok := e["value"].(string); ok {
			out[name] = value
			continue
		}
		// A field or secret reference has no literal value; record its presence so
		// adding or removing one is still reported.
		out[name] = "(from reference)"
	}
	return out
}

func servicePorts(u *unstructured.Unstructured) string {
	ports := nestedSlice(u, "spec", "ports")
	parts := make([]string, 0, len(ports))
	for _, entry := range ports {
		p, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		parts = append(parts, fmt.Sprintf("%v→%v", p["port"], p["targetPort"]))
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

func ingressHosts(u *unstructured.Unstructured) string {
	rules := nestedSlice(u, "spec", "rules")
	hosts := make([]string, 0, len(rules))
	for _, entry := range rules {
		r, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		if host, ok := r["host"].(string); ok {
			hosts = append(hosts, host)
		}
	}
	sort.Strings(hosts)
	return strings.Join(hosts, ",")
}

// annotationAt reads the annotations map at a nested path, such as the pod
// template's.
func annotationAt(u *unstructured.Unstructured, path ...string) map[string]string {
	full := append(append([]string{}, path...), "metadata", "annotations")
	out, _, err := unstructured.NestedStringMap(u.Object, full...)
	if err != nil || out == nil {
		return map[string]string{}
	}
	return out
}

func boolWord(b bool) string {
	if b {
		return "enabled"
	}
	return "disabled"
}
