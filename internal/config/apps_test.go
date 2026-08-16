package config

import (
	"strings"
	"testing"
)

func TestLoadExtraProcessApp(t *testing.T) {
	path := write(t, `
app: web
image: ghcr.io/acme/web
apps:
  api:
    image: ghcr.io/acme/api
    deploy: {port: 3001}
    proxy: {host: api.example.com, ssl: true}
  worker:
    deploy: {command: ["./worker"]}
accessories:
  postgres: {type: postgres}
`)
	res, err := Load(LoadOptions{Path: path, Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	cfg := res.Config
	if cfg.Member("web") != MemberPrimary {
		t.Fatalf("web: %v", cfg.Member("web"))
	}
	if cfg.Member("api") != MemberProcess {
		t.Fatalf("api: %v", cfg.Member("api"))
	}
	if cfg.Member("postgres") != MemberStateful {
		t.Fatalf("postgres: %v", cfg.Member("postgres"))
	}
	if cfg.Member("nope") != MemberNone {
		t.Fatalf("nope: %v", cfg.Member("nope"))
	}

	api, err := cfg.ForProcessApp("api")
	if err != nil {
		t.Fatal(err)
	}
	if api.App != "api" {
		t.Errorf("api.App = %q", api.App)
	}
	if api.Image != "ghcr.io/acme/api" {
		t.Errorf("api.Image = %q", api.Image)
	}
	if api.Deploy.Port != 3001 {
		t.Errorf("api.Port = %d", api.Deploy.Port)
	}
	if api.Proxy.Host != "api.example.com" {
		t.Errorf("api.Host = %q", api.Proxy.Host)
	}
	if api.Proxy.Enabled == nil || !*api.Proxy.Enabled {
		t.Error("api proxy should be enabled when a host is set")
	}
	if api.Deploy.Kubernetes.Namespace != cfg.Deploy.Kubernetes.Namespace {
		t.Errorf("api namespace = %q, want stack %q", api.Deploy.Kubernetes.Namespace, cfg.Deploy.Kubernetes.Namespace)
	}
	if len(api.Accessories) != 0 {
		t.Errorf("process clone must not carry accessories: %v", api.Accessories)
	}

	worker, err := cfg.ForProcessApp("worker")
	if err != nil {
		t.Fatal(err)
	}
	if worker.Image != cfg.Image {
		t.Errorf("worker should inherit image, got %q", worker.Image)
	}
	if got := worker.Deploy.Command; len(got) != 1 || got[0] != "./worker" {
		t.Errorf("worker command = %v", got)
	}
	if worker.Proxy.Host != "" {
		t.Errorf("worker must not inherit the first app's host, got %q", worker.Proxy.Host)
	}
	if worker.Proxy.Enabled != nil && *worker.Proxy.Enabled {
		t.Error("worker proxy should stay off")
	}

	if _, err := cfg.ForProcessApp("postgres"); err == nil {
		t.Fatal("ForProcessApp(postgres) should fail")
	}
	st, err := cfg.ForStatefulApp("postgres")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st.Accessories["postgres"]; !ok || len(st.Accessories) != 1 {
		t.Errorf("stateful filter = %v", st.Accessories)
	}
}

func TestExtraAppNameClash(t *testing.T) {
	_, err := Load(LoadOptions{Path: write(t, `
app: web
image: ghcr.io/acme/web
apps:
  web: {deploy: {port: 9000}}
`), Strict: true})
	if err == nil || !strings.Contains(err.Error(), "already the stack app") {
		t.Fatalf("want clash with stack name, got %v", err)
	}

	_, err = Load(LoadOptions{Path: write(t, `
app: web
image: ghcr.io/acme/web
accessories:
  postgres: {type: postgres}
apps:
  postgres: {deploy: {command: ["./x"]}}
`), Strict: true})
	if err == nil || !strings.Contains(err.Error(), "already a stateful app") {
		t.Fatalf("want clash with accessory, got %v", err)
	}
}
