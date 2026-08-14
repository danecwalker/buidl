package kubernetes

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/danecwalker/buidl/internal/config"
)

// resolveScale fills omitted HPA bounds from the fleet, then from Ready
// schedulable nodes, then from a single-node fallback.
//
// Called from Plan, Deploy and Render so every path that emits an HPA has
// concrete min/max. Explicit bounds in buidl.yaml are never overwritten.
func (t *Target) resolveScale(ctx context.Context) {
	if t == nil || t.cfg == nil || t.cfg.Deploy.Autoscale == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}

	nodes := config.FleetSize(t.cfg)
	if nodes == 0 && t.clientset != nil {
		if n, err := t.readyNodeCount(ctx); err == nil && n > 0 {
			nodes = n
		}
	}
	config.ResolveAutoscale(t.cfg, nodes)
}

// readyNodeCount is the number of nodes that can take work right now.
func (t *Target) readyNodeCount(ctx context.Context) (int, error) {
	list, err := t.clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, node := range list.Items {
		if node.Spec.Unschedulable {
			continue
		}
		if nodeReady(node) {
			n++
		}
	}
	return n, nil
}

func nodeReady(node corev1.Node) bool {
	for _, cond := range node.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}
