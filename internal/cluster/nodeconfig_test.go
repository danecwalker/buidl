package cluster

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/danewalker/buidl/internal/config"
	"github.com/danewalker/buidl/internal/inventory"
)

// parsed decodes a rendered node config so assertions read against real keys
// rather than substrings.
func parsed(t *testing.T, rendered string) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(rendered), &doc); err != nil {
		t.Fatalf("rendered config is not valid YAML: %v\n%s", err, rendered)
	}
	return doc
}

func testPlan(k8s config.ClusterKubernetes, servers ...inventory.Server) *Plan {
	inv := &inventory.Inventory{Servers: servers}
	inventory.Normalize(inv)
	bootstrap, _ := inv.Bootstrap()
	return &Plan{
		Kubernetes:          k8s,
		Inventory:           inv,
		Bootstrap:           bootstrap,
		Token:               "test-token",
		RegistrationAddress: registrationAddress(k8s, bootstrap),
		TLSSANs:             tlsSANs(k8s, inv),
	}
}

func TestBootstrapConfigInitializesCluster(t *testing.T) {
	server := inventory.Server{Host: "10.0.0.1", Role: inventory.RoleControlPlane, PrivateIP: "10.0.0.1"}
	plan := testPlan(config.ClusterKubernetes{
		ClusterCIDR: "10.42.0.0/16",
		ServiceCIDR: "10.43.0.0/16",
	}, server)

	rendered, err := renderNodeConfig(server, inventory.RoleControlPlane, true, plan)
	if err != nil {
		t.Fatalf("renderNodeConfig: %v", err)
	}
	doc := parsed(t, rendered)

	// cluster-init creates a new embedded-etcd cluster.
	if doc["cluster-init"] != true {
		t.Errorf("cluster-init = %v, want true on the bootstrap node", doc["cluster-init"])
	}
	// The bootstrap node has nothing to join, so `server` must be absent —
	// pointing it at itself would deadlock startup.
	if _, present := doc["server"]; present {
		t.Errorf("bootstrap config must not set `server`, got %v", doc["server"])
	}
	if doc["token"] != "test-token" {
		t.Errorf("token = %v", doc["token"])
	}
	if doc["cluster-cidr"] != "10.42.0.0/16" {
		t.Errorf("cluster-cidr = %v", doc["cluster-cidr"])
	}
}

func TestClusterInitEvenForSingleNode(t *testing.T) {
	// Embedded etcd is chosen even for one node so a control plane can be added
	// later without migrating off sqlite.
	server := inventory.Server{Host: "10.0.0.1"}
	plan := testPlan(config.ClusterKubernetes{}, server)

	rendered, err := renderNodeConfig(plan.Bootstrap, inventory.RoleControlPlane, true, plan)
	if err != nil {
		t.Fatal(err)
	}
	if parsed(t, rendered)["cluster-init"] != true {
		t.Error("a single-node cluster should still use embedded etcd")
	}
	_ = server
}

func TestJoiningControlPlaneRegistersWithEndpoint(t *testing.T) {
	k8s := config.ClusterKubernetes{ControlPlaneEndpoint: "k8s.acme.com"}
	cp1 := inventory.Server{Host: "10.0.0.1", Role: inventory.RoleControlPlane}
	cp2 := inventory.Server{Host: "10.0.0.2", Role: inventory.RoleControlPlane}
	plan := testPlan(k8s, cp1, cp2)

	rendered, err := renderNodeConfig(cp2, inventory.RoleControlPlane, false, plan)
	if err != nil {
		t.Fatal(err)
	}
	doc := parsed(t, rendered)

	// A joining member must register through the stable endpoint, not the
	// bootstrap machine, or losing that machine would break future joins.
	if doc["server"] != "https://k8s.acme.com:6443" {
		t.Errorf("server = %v, want the control-plane endpoint", doc["server"])
	}
	if _, present := doc["cluster-init"]; present {
		t.Error("a joining member must not set cluster-init; it would fork the cluster")
	}
}

func TestWorkerConfigOmitsServerOnlyKeys(t *testing.T) {
	k8s := config.ClusterKubernetes{
		ClusterCIDR: "10.42.0.0/16",
		ServiceCIDR: "10.43.0.0/16",
		Disable:     []string{"traefik"},
	}
	cp := inventory.Server{Host: "10.0.0.1", Role: inventory.RoleControlPlane}
	worker := inventory.Server{Host: "10.0.1.1", Role: inventory.RoleWorker}
	plan := testPlan(k8s, cp, worker)

	rendered, err := renderNodeConfig(worker, inventory.RoleWorker, false, plan)
	if err != nil {
		t.Fatal(err)
	}
	doc := parsed(t, rendered)

	if doc["server"] != "https://10.0.0.1:6443" {
		t.Errorf("server = %v", doc["server"])
	}
	// Agents reject cluster-level keys; including them fails the service at boot.
	for _, serverOnly := range []string{"cluster-init", "cluster-cidr", "service-cidr", "disable", "tls-san", "write-kubeconfig-mode"} {
		if _, present := doc[serverOnly]; present {
			t.Errorf("worker config must not contain server-only key %q", serverOnly)
		}
	}
}

func TestTLSSANsCoverEveryControlPlaneAddress(t *testing.T) {
	k8s := config.ClusterKubernetes{
		ControlPlaneEndpoint: "k8s.acme.com",
		TLSSANs:              []string{"api.internal"},
	}
	cp1 := inventory.Server{Host: "203.0.113.1", Role: inventory.RoleControlPlane, PrivateIP: "10.0.0.1"}
	cp2 := inventory.Server{Host: "203.0.113.2", Role: inventory.RoleControlPlane, PrivateIP: "10.0.0.2"}
	worker := inventory.Server{Host: "203.0.113.9", Role: inventory.RoleWorker}

	sans := tlsSANs(k8s, &inventory.Inventory{Servers: []inventory.Server{cp1, cp2, worker}})

	// Any address a client or node might dial must be in the certificate, or
	// kubectl fails with a confusing TLS error against a reachable endpoint.
	for _, want := range []string{"k8s.acme.com", "api.internal", "203.0.113.1", "10.0.0.1", "203.0.113.2", "10.0.0.2"} {
		if !containsString(sans, want) {
			t.Errorf("TLS SANs %v should include %q", sans, want)
		}
	}
	// Workers do not serve the API, so their addresses are irrelevant.
	if containsString(sans, "203.0.113.9") {
		t.Error("worker addresses should not be in the API server certificate")
	}
}

func TestTLSSANsDeduplicate(t *testing.T) {
	k8s := config.ClusterKubernetes{
		ControlPlaneEndpoint: "10.0.0.1",
		TLSSANs:              []string{"10.0.0.1"},
	}
	cp := inventory.Server{Host: "10.0.0.1", Role: inventory.RoleControlPlane, PrivateIP: "10.0.0.1"}

	sans := tlsSANs(k8s, &inventory.Inventory{Servers: []inventory.Server{cp}})

	count := 0
	for _, s := range sans {
		if s == "10.0.0.1" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected 10.0.0.1 once, got %d times in %v", count, sans)
	}
}

func TestRegistrationAddressPrefersEndpoint(t *testing.T) {
	bootstrap := inventory.Server{Host: "203.0.113.1", PrivateIP: "10.0.0.1"}

	withEndpoint := registrationAddress(config.ClusterKubernetes{ControlPlaneEndpoint: "k8s.acme.com"}, bootstrap)
	if withEndpoint != "https://k8s.acme.com:6443" {
		t.Errorf("registrationAddress = %q", withEndpoint)
	}

	// Without an endpoint, nodes register over the private network.
	withoutEndpoint := registrationAddress(config.ClusterKubernetes{}, bootstrap)
	if withoutEndpoint != "https://10.0.0.1:6443" {
		t.Errorf("registrationAddress = %q, want the private address", withoutEndpoint)
	}
}

func TestNodeIPPinnedFromPrivateIP(t *testing.T) {
	// Without node-ip, a dual-homed cloud server may advertise its public address
	// and route all intra-cluster traffic over the internet.
	server := inventory.Server{Host: "203.0.113.1", Role: inventory.RoleWorker, PrivateIP: "10.0.0.5"}
	cp := inventory.Server{Host: "10.0.0.1", Role: inventory.RoleControlPlane}
	plan := testPlan(config.ClusterKubernetes{}, cp, server)

	rendered, err := renderNodeConfig(server, inventory.RoleWorker, false, plan)
	if err != nil {
		t.Fatal(err)
	}
	if got := parsed(t, rendered)["node-ip"]; got != "10.0.0.5" {
		t.Errorf("node-ip = %v, want the private IP", got)
	}
}

func TestLabelsAndTaintsRender(t *testing.T) {
	server := inventory.Server{
		Host:   "10.0.1.1",
		Role:   inventory.RoleWorker,
		Labels: map[string]string{"pool": "gpu", "zone": "a"},
		Taints: []string{"gpu=true:NoSchedule"},
	}
	cp := inventory.Server{Host: "10.0.0.1", Role: inventory.RoleControlPlane}
	plan := testPlan(config.ClusterKubernetes{}, cp, server)

	rendered, err := renderNodeConfig(server, inventory.RoleWorker, false, plan)
	if err != nil {
		t.Fatal(err)
	}
	doc := parsed(t, rendered)

	labels := toStrings(doc["node-label"])
	if !containsString(labels, "pool=gpu") || !containsString(labels, "zone=a") {
		t.Errorf("node-label = %v", labels)
	}
	taints := toStrings(doc["node-taint"])
	if !containsString(taints, "gpu=true:NoSchedule") {
		t.Errorf("node-taint = %v", taints)
	}
}

func TestRenderIsDeterministic(t *testing.T) {
	// An unstable render would rewrite the config file and restart the service on
	// every `cluster up`, causing a needless API outage each run.
	server := inventory.Server{
		Host:   "10.0.1.1",
		Role:   inventory.RoleWorker,
		Labels: map[string]string{"a": "1", "b": "2", "c": "3", "d": "4", "e": "5"},
	}
	cp := inventory.Server{Host: "10.0.0.1", Role: inventory.RoleControlPlane}
	plan := testPlan(config.ClusterKubernetes{}, cp, server)

	first, err := renderNodeConfig(server, inventory.RoleWorker, false, plan)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 20 {
		again, err := renderNodeConfig(server, inventory.RoleWorker, false, plan)
		if err != nil {
			t.Fatal(err)
		}
		if again != first {
			t.Fatalf("render differed on attempt %d:\n%s\n---\n%s", i, first, again)
		}
	}
}

func TestExtraArgsPassThrough(t *testing.T) {
	k8s := config.ClusterKubernetes{
		ExtraArgs: map[string]string{"kubelet-arg": "max-pods=200"},
	}
	cp := inventory.Server{Host: "10.0.0.1", Role: inventory.RoleControlPlane}
	plan := testPlan(k8s, cp)

	rendered, err := renderNodeConfig(cp, inventory.RoleControlPlane, true, plan)
	if err != nil {
		t.Fatal(err)
	}
	if got := parsed(t, rendered)["kubelet-arg"]; got != "max-pods=200" {
		t.Errorf("kubelet-arg = %v, want the passthrough value", got)
	}
}

func TestKubeconfigModeIsNotWorldReadable(t *testing.T) {
	cp := inventory.Server{Host: "10.0.0.1", Role: inventory.RoleControlPlane}
	plan := testPlan(config.ClusterKubernetes{}, cp)

	rendered, err := renderNodeConfig(cp, inventory.RoleControlPlane, true, plan)
	if err != nil {
		t.Fatal(err)
	}
	// 0644 would expose cluster-admin credentials to every local user.
	if got := parsed(t, rendered)["write-kubeconfig-mode"]; got != "0640" {
		t.Errorf("write-kubeconfig-mode = %v, want 0640", got)
	}
}

func TestRenderedConfigCarriesProvenanceHeader(t *testing.T) {
	cp := inventory.Server{Host: "10.0.0.1", Role: inventory.RoleControlPlane}
	plan := testPlan(config.ClusterKubernetes{}, cp)

	rendered, err := renderNodeConfig(cp, inventory.RoleControlPlane, true, plan)
	if err != nil {
		t.Fatal(err)
	}
	// Someone finding this file on a server should know what wrote it and that
	// local edits will be lost.
	if !strings.Contains(rendered, "Managed by buidl") {
		t.Errorf("expected a provenance header:\n%s", rendered)
	}
}

func TestNormalizeConfigIgnoresComments(t *testing.T) {
	a := "# header changes every run\ntoken: x\nserver: https://h:6443\n"
	b := "# a totally different header\n\ntoken: x\nserver: https://h:6443\n"
	// A header-only difference must not be seen as a configuration change.
	if normalizeConfig(a) != normalizeConfig(b) {
		t.Error("comment-only differences should normalize away")
	}

	c := "token: x\nserver: https://other:6443\n"
	if normalizeConfig(a) == normalizeConfig(c) {
		t.Error("a real value change must survive normalization")
	}
}

// TestNormalizeConfigIgnoresTheToken pins the fix for a convergence bug found
// against a real cluster.
//
// k3s rewrites the join token it is given into a derived form
// ("K10<ca-hash>::server:<secret>"), so the value read back never equals the one
// written. Comparing tokens marked the bootstrap node as needing reconfiguration
// on every run — restarting the API server each time and never settling.
func TestNormalizeConfigIgnoresTheToken(t *testing.T) {
	written := "cluster-init: true\ntoken: plain-generated-secret\n"
	onDisk := "cluster-init: true\ntoken: K10abc123::server:plain-generated-secret\n"

	if normalizeConfig(written) != normalizeConfig(onDisk) {
		t.Error("a token rewritten by the distribution must not read as a config change")
	}

	// Everything else must still be compared.
	changed := "cluster-init: false\ntoken: plain-generated-secret\n"
	if normalizeConfig(written) == normalizeConfig(changed) {
		t.Error("non-token changes must still be detected")
	}
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func toStrings(v any) []string {
	items, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
