package kubernetes

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/danecwalker/buidl/internal/config"
	"github.com/danecwalker/buidl/internal/deploy"
)

func TestLocalImageSkipsManagedPullSecret(t *testing.T) {
	target, req := testRequest(t, `
app: web
image: buidl.local/web
registry:
  createPullSecret: true
deploy:
  replicas: 1
`)
	req.Release.Repo = "buidl.local/web"
	if target.managedPullSecret(req.Config) {
		t.Fatal("local image must not create a pull secret even if createPullSecret is true")
	}
}

func TestRenderCreatesManagedPullSecret(t *testing.T) {
	target, req := testRequest(t, `
app: web
image: registry.test/acme/web
registry:
  createPullSecret: true
  username: acme
  password: s3cret
deploy:
  replicas: 1
  kubernetes: {namespace: acme}
`)

	objs, err := target.Render(req)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	var pull *corev1.Secret
	for i := range objs {
		if objs[i].Kind != "Secret" {
			continue
		}
		sec := objs[i].Object.(*corev1.Secret)
		if sec.Type == corev1.SecretTypeDockerConfigJson {
			pull = sec
			break
		}
	}
	if pull == nil {
		t.Fatal("expected a dockerconfigjson Secret so the cluster can pull")
	}
	if pull.Name != "web-registry" {
		t.Errorf("pull secret name = %q, want web-registry", pull.Name)
	}

	raw, ok := pull.Data[corev1.DockerConfigJsonKey]
	if !ok {
		t.Fatal("pull secret missing .dockerconfigjson")
	}
	var doc dockerConfigJSON
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decoding dockerconfigjson: %v", err)
	}
	entry, ok := doc.Auths["registry.test"]
	if !ok {
		t.Fatalf("auths = %v, want an entry for registry.test", doc.Auths)
	}
	if entry.Username != "acme" {
		t.Errorf("username = %q, want acme", entry.Username)
	}
	if entry.Password != "s3cret" {
		t.Error("password was not copied into the pull secret")
	}
	wantAuth := base64.StdEncoding.EncodeToString([]byte("acme:s3cret"))
	if entry.Auth != wantAuth {
		t.Error("auth field should be the base64 of username:password")
	}

	dep := findObject(objs, "Deployment").Object.(*appsv1.Deployment)
	refs := dep.Spec.Template.Spec.ImagePullSecrets
	if len(refs) != 1 || refs[0].Name != "web-registry" {
		t.Errorf("ImagePullSecrets = %v, want web-registry", refs)
	}
}

func TestRenderFailsExplicitPullSecretWithoutCredentials(t *testing.T) {
	target, req := testRequest(t, `
app: web
image: registry.test/acme/web
registry:
  createPullSecret: true
deploy:
  replicas: 1
  kubernetes: {namespace: acme}
`)

	_, err := target.Render(req)
	if err == nil {
		t.Fatal("expected Render to fail when createPullSecret is set and no credential exists")
	}
	if !strings.Contains(err.Error(), "registry.test") {
		t.Errorf("error should name the registry, got: %v", err)
	}
	if !strings.Contains(err.Error(), "docker login") {
		t.Errorf("error should tell the user to docker login, got: %v", err)
	}
}

func TestRenderHonoursExplicitPullSecretFalse(t *testing.T) {
	target, req := testRequest(t, `
app: web
image: registry.test/acme/web
registry:
  createPullSecret: false
deploy:
  replicas: 1
  kubernetes: {namespace: acme}
`)

	objs, err := target.Render(req)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for i := range objs {
		if objs[i].Kind != "Secret" {
			continue
		}
		sec := objs[i].Object.(*corev1.Secret)
		if sec.Type == corev1.SecretTypeDockerConfigJson {
			t.Fatal("createPullSecret: false must not render a pull secret")
		}
	}
	dep := findObject(objs, "Deployment").Object.(*appsv1.Deployment)
	if refs := dep.Spec.Template.Spec.ImagePullSecrets; len(refs) != 0 {
		t.Errorf("ImagePullSecrets = %v, want none", refs)
	}
}

func TestPullSecretRefsNamedAndManaged(t *testing.T) {
	cfg := testConfig(t, `
app: web
image: registry.test/acme/web
registry:
  createPullSecret: true
  pullSecret: already-managed
  username: acme
  password: s3cret
`)
	refs := pullSecretRefs(cfg, true)
	if len(refs) != 2 {
		t.Fatalf("refs = %v, want named + managed", refs)
	}
	if refs[0].Name != "already-managed" || refs[1].Name != "web-registry" {
		t.Errorf("refs = %v, want already-managed then web-registry", refs)
	}
}

func TestRenderSkipsDefaultedPullSecretWithoutCredentials(t *testing.T) {
	// Bypass testConfig's pin-off: this is the four-line-config path.
	res, err := config.Load(config.LoadOptions{
		Path:   writeConfig(t, "app: web\nimage: registry.test/acme/web\n"),
		Strict: true,
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !res.Config.Registry.PullSecretOptional() {
		t.Fatal("expected the defaulted createPullSecret to be optional")
	}

	target := newTestTarget(res.Config)
	objs, err := target.Render(deploy.Request{
		Config:  res.Config,
		Release: testRelease(),
		Root:    ".",
	})
	if err != nil {
		t.Fatalf("Render of a defaulted pull secret without creds must skip, not fail: %v", err)
	}
	for i := range objs {
		if objs[i].Kind != "Secret" {
			continue
		}
		if objs[i].Object.(*corev1.Secret).Type == corev1.SecretTypeDockerConfigJson {
			t.Fatal("missing credentials should skip the pull secret, not render an empty one")
		}
	}
	dep := findObject(objs, "Deployment").Object.(*appsv1.Deployment)
	if refs := dep.Spec.Template.Spec.ImagePullSecrets; len(refs) != 0 {
		t.Errorf("ImagePullSecrets = %v, want none when the secret was skipped", refs)
	}
}
