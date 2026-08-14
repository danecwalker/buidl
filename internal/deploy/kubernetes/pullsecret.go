package kubernetes

import (
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	corev1 "k8s.io/api/core/v1"

	"github.com/danecwalker/buidl/internal/config"
	"github.com/danecwalker/buidl/internal/release"
)

// dockerConfigJSON is the on-disk shape Kubernetes expects in a
// kubernetes.io/dockerconfigjson Secret.
type dockerConfigJSON struct {
	Auths map[string]dockerAuthEntry `json:"auths"`
}

type dockerAuthEntry struct {
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	// Auth is the base64 of "username:password". Some registries only read this
	// field, so both forms are written.
	Auth string `json:"auth"`
}

// pullSecretName is the Secret buidl creates when createPullSecret is set.
func pullSecretName(app string) string {
	return release.ObjectName(app, "registry")
}

// pullSecretRefs returns the imagePullSecrets to attach to the pod.
//
// An explicitly named secret and a buidl-managed one can coexist: a cluster may
// hold credentials for a base-image registry while buidl manages the app's own.
func pullSecretRefs(cfg *config.Config, managed bool) []corev1.LocalObjectReference {
	var refs []corev1.LocalObjectReference
	if cfg.Registry.PullSecret != "" {
		refs = append(refs, corev1.LocalObjectReference{Name: cfg.Registry.PullSecret})
	}
	if managed {
		refs = append(refs, corev1.LocalObjectReference{Name: pullSecretName(cfg.App)})
	}
	return refs
}

// managedPullSecret reports whether a buidl-managed imagePullSecret will
// actually be created. A defaulted createPullSecret with no local credential
// is skipped so a public image and `buidl manifest` still work.
func (t *Target) managedPullSecret(cfg *config.Config) bool {
	if !cfg.Registry.ManagesPullSecret() {
		return false
	}
	if !cfg.Registry.PullSecretOptional() {
		return true
	}
	host, err := registryHostFor(cfg)
	if err != nil {
		return false
	}
	if _, _, err := resolveRegistryCredential(cfg, host); err != nil {
		t.log.Detail("no local credentials for %s; skipping imagePullSecret", host)
		return false
	}
	return true
}

// pullSecret renders the registry credential Secret.
//
// Credentials are resolved but never logged. Returning an error rather than
// silently rendering an empty secret matters: an empty pull secret fails at image
// pull time with "unauthorized", which is a confusing way to learn that a
// credential was missing.
func (t *Target) pullSecret(cfg *config.Config, rel release.Release) (*Object, error) {
	registryHost, err := registryHostFor(cfg)
	if err != nil {
		return nil, err
	}

	username, password, err := resolveRegistryCredential(cfg, registryHost)
	if err != nil {
		if cfg.Registry.PullSecretOptional() {
			t.log.Detail("no local credentials for %s; skipping imagePullSecret", registryHost)
			return nil, nil
		}
		return nil, err
	}

	entry := dockerAuthEntry{
		Username: username,
		Password: password,
		Auth:     base64.StdEncoding.EncodeToString([]byte(username + ":" + password)),
	}
	doc := dockerConfigJSON{Auths: map[string]dockerAuthEntry{registryHost: entry}}

	encoded, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("encoding registry credentials: %w", err)
	}

	name := pullSecretName(cfg.App)
	secret := &corev1.Secret{
		ObjectMeta: t.objectMeta(cfg, rel, name),
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: encoded},
	}

	return &Object{
		GVK:        corev1.SchemeGroupVersion.WithKind("Secret"),
		Name:       name,
		Kind:       "Secret",
		Object:     secret,
		Namespaced: true,
		// Must exist before the workload that references it, or the first pull
		// fails while the kubelet waits for the secret to appear.
		Order: orderSecret,
	}, nil
}

// registryHostFor determines the registry host to authenticate against.
func registryHostFor(cfg *config.Config) (string, error) {
	if cfg.Registry.Server != "" {
		return cfg.Registry.Server, nil
	}
	ref, err := name.ParseReference(cfg.Image)
	if err != nil {
		return "", fmt.Errorf("determining the registry for %q: %w", cfg.Image, err)
	}
	return ref.Context().RegistryStr(), nil
}

// resolveRegistryCredential finds a username and password for the registry.
//
// Explicit config wins so CI can inject a scoped token; otherwise the local Docker
// keychain is used, which is the same source BuildKit pushes with. Using one source
// for both means a working push implies a working pull secret.
func resolveRegistryCredential(cfg *config.Config, host string) (string, string, error) {
	if cfg.Registry.Username != "" && cfg.Registry.Password != "" {
		return cfg.Registry.Username, cfg.Registry.Password, nil
	}

	registry, err := name.NewRegistry(host)
	if err != nil {
		return "", "", fmt.Errorf("parsing registry %q: %w", host, err)
	}

	authenticator, err := authn.DefaultKeychain.Resolve(registry)
	if err != nil {
		return "", "", credentialError(host, err)
	}

	authConfig, err := authenticator.Authorization()
	if err != nil {
		return "", "", credentialError(host, err)
	}

	// A registry token can arrive as an identity/registry token rather than a
	// password; Kubernetes accepts it in the password field.
	username, password := authConfig.Username, authConfig.Password
	if password == "" {
		if authConfig.RegistryToken != "" {
			password = authConfig.RegistryToken
		} else if authConfig.IdentityToken != "" {
			// The conventional username for token-based auth.
			username, password = "<token>", authConfig.IdentityToken
		}
	}

	if username == "" || password == "" {
		return "", "", credentialError(host, fmt.Errorf("no credentials found"))
	}
	return username, password, nil
}

// credentialError explains how to supply registry credentials.
func credentialError(host string, cause error) error {
	return fmt.Errorf("cannot resolve credentials for %s: %w\n\n"+
		"registry.createPullSecret needs a credential to copy into the cluster. Either:\n"+
		"  docker login %s\n"+
		"or set them explicitly (values are interpolated, so use a variable):\n"+
		"  registry:\n    username: %s\n    password: ${REGISTRY_TOKEN}",
		host, cause, host, "your-username")
}
