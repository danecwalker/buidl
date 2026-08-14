package kubernetes

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

func TestResolveScaleUsesReadyNodes(t *testing.T) {
	cfg := testConfig(t, `
app: web
image: ghcr.io/acme/web
`)
	if cfg.Deploy.Autoscale == nil {
		t.Fatal("expected a default HPA")
	}
	if cfg.Deploy.Autoscale.Min != 1 || cfg.Deploy.Autoscale.Max != 4 {
		t.Fatalf("precondition: bounds = %d/%d, want the 1-node fallback", cfg.Deploy.Autoscale.Min, cfg.Deploy.Autoscale.Max)
	}

	tgt := &Target{
		cfg: cfg,
		log: testLogger{},
		clientset: fake.NewSimpleClientset(
			readyNode("a"),
			readyNode("b"),
			unschedulableNode("c"),
			notReadyNode("d"),
		),
	}
	tgt.resolveScale(context.Background())

	if cfg.Deploy.Autoscale.Min != 2 || cfg.Deploy.Autoscale.Max != 8 {
		t.Errorf("bounds = %d/%d, want 2/8 from two Ready schedulable nodes", cfg.Deploy.Autoscale.Min, cfg.Deploy.Autoscale.Max)
	}
}

func TestResolveScalePrefersFleetOverNodes(t *testing.T) {
	cfg := testConfig(t, `
app: web
image: ghcr.io/acme/web
infra:
  servers:
    - {host: 10.0.0.1, role: control-plane}
    - {host: 10.0.1.1, role: worker}
    - {host: 10.0.1.2, role: worker}
`)
	tgt := &Target{
		cfg:       cfg,
		log:       testLogger{},
		clientset: fake.NewSimpleClientset(readyNode("only-one")),
	}
	tgt.resolveScale(context.Background())

	if cfg.Deploy.Autoscale.Min != 2 || cfg.Deploy.Autoscale.Max != 9 {
		t.Errorf("bounds = %d/%d, want 2/9 from the 3-server fleet, not the single Ready node", cfg.Deploy.Autoscale.Min, cfg.Deploy.Autoscale.Max)
	}
}

func readyNode(name string) runtime.Object {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{
				Type:   corev1.NodeReady,
				Status: corev1.ConditionTrue,
			}},
		},
	}
}

func unschedulableNode(name string) runtime.Object {
	n := readyNode(name).(*corev1.Node)
	n.Spec.Unschedulable = true
	return n
}

func notReadyNode(name string) runtime.Object {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{
				Type:   corev1.NodeReady,
				Status: corev1.ConditionFalse,
			}},
		},
	}
}
