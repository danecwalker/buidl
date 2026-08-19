package kubernetes

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/danecwalker/buidl/internal/config"
	"github.com/danecwalker/buidl/internal/release"
)

func TestObjectsToApplyDefersLiveBlueGreenService(t *testing.T) {
	target, req := testRequest(t, renderBase+`
  strategy:
    type: bluegreen
`)
	objs, err := target.Render(req)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if findObject(objs, "Service") == nil {
		t.Fatal("render must still include Service so plan and cutover can use it")
	}

	target.clientset = fake.NewSimpleClientset(&corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      release.ObjectName(req.Config.App),
			Namespace: target.Namespace,
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{release.LabelRelease: "old-release"},
		},
	})

	got := target.objectsToApply(context.Background(), req, objs)
	if findObject(got, "Service") != nil {
		t.Fatal("applying the Service now would cut traffic to pods that are not ready")
	}
	if findObject(got, "Deployment") == nil {
		t.Fatal("the new Deployment must still apply")
	}
}

func TestObjectsToApplyCreatesServiceOnFirstBlueGreenDeploy(t *testing.T) {
	target, req := testRequest(t, renderBase+`
  strategy:
    type: bluegreen
`)
	objs, err := target.Render(req)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	target.clientset = fake.NewSimpleClientset()

	got := target.objectsToApply(context.Background(), req, objs)
	if findObject(got, "Service") == nil {
		t.Fatal("the first deploy must create the Service")
	}
}

func TestObjectsToApplyKeepsRollingService(t *testing.T) {
	target, req := testRequest(t, renderBase+`
  strategy:
    type: rolling
`)
	objs, err := target.Render(req)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	target.clientset = fake.NewSimpleClientset(&corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      release.ObjectName(req.Config.App),
			Namespace: target.Namespace,
		},
	})

	got := target.objectsToApply(context.Background(), req, objs)
	if findObject(got, "Service") == nil {
		t.Fatal("a rolling update must keep applying the Service")
	}
}

func TestOmitKindDropsOnlyTheNamedKind(t *testing.T) {
	objs := []Object{
		{Kind: "Deployment", Name: "web"},
		{Kind: "Service", Name: "web"},
		{Kind: "Ingress", Name: "web"},
	}
	got := omitKind(objs, "Service")
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if findObject(got, "Service") != nil {
		t.Fatal("Service should have been omitted")
	}
	if findObject(got, "Deployment") == nil || findObject(got, "Ingress") == nil {
		t.Fatal("other kinds must be kept")
	}
}

func TestReadyEndpointAddressesIgnoresNotReady(t *testing.T) {
	ep := &corev1.Endpoints{
		Subsets: []corev1.EndpointSubset{{
			Addresses:         []corev1.EndpointAddress{{IP: "10.0.0.1"}, {IP: "10.0.0.2"}},
			NotReadyAddresses: []corev1.EndpointAddress{{IP: "10.0.0.3"}},
		}},
	}
	if got := readyEndpointAddresses(ep); got != 2 {
		t.Errorf("ready = %d, want 2", got)
	}
	if got := readyEndpointAddresses(&corev1.Endpoints{}); got != 0 {
		t.Errorf("empty = %d, want 0", got)
	}
}

func TestWaitForReadyEndpointsSeesAddresses(t *testing.T) {
	cfg := testConfig(t, renderBase+`
  strategy:
    type: bluegreen
`)
	tgt := endpointTarget(cfg, &corev1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: cfg.Deploy.Kubernetes.Namespace},
		Subsets: []corev1.EndpointSubset{{
			Addresses: []corev1.EndpointAddress{{IP: "10.0.0.1"}},
		}},
	})
	if err := tgt.waitForReadyEndpoints(context.Background(), "web", time.Second); err != nil {
		t.Fatalf("waitForReadyEndpoints: %v", err)
	}
}

func TestWaitForReadyEndpointsTimesOutWhenEmpty(t *testing.T) {
	cfg := testConfig(t, renderBase+`
  strategy:
    type: bluegreen
`)
	tgt := endpointTarget(cfg)
	err := tgt.waitForReadyEndpoints(context.Background(), "web", 50*time.Millisecond)
	if err == nil {
		t.Fatal("expected a timeout against an empty Endpoints object")
	}
	if !strings.Contains(err.Error(), "no ready endpoints") {
		t.Errorf("error %q, want no ready endpoints", err)
	}
}

func TestServiceExists(t *testing.T) {
	cfg := testConfig(t, renderBase)
	tgt := &Target{
		cfg:       cfg,
		log:       testLogger{},
		Namespace: cfg.Deploy.Kubernetes.Namespace,
		clientset: fake.NewSimpleClientset(&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: cfg.Deploy.Kubernetes.Namespace},
		}),
	}
	if !tgt.serviceExists(context.Background(), cfg) {
		t.Fatal("expected the Service to exist")
	}

	tgt.clientset = fake.NewSimpleClientset()
	if tgt.serviceExists(context.Background(), cfg) {
		t.Fatal("empty cluster must report the Service as missing")
	}
}

func endpointTarget(cfg *config.Config, objects ...runtime.Object) *Target {
	return &Target{
		cfg:       cfg,
		log:       testLogger{},
		Namespace: cfg.Deploy.Kubernetes.Namespace,
		clientset: fake.NewSimpleClientset(objects...),
	}
}
