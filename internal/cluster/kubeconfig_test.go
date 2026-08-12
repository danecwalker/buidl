package cluster

import (
	"os"
	"path/filepath"
	"testing"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// fetchedConfig builds a kubeconfig shaped like the one a distribution writes
// on-node: everything named "default", server pointing at localhost.
func fetchedConfig(server string) *clientcmdapi.Config {
	cfg := clientcmdapi.NewConfig()
	cfg.Clusters["default"] = &clientcmdapi.Cluster{
		Server:                   server,
		CertificateAuthorityData: []byte("ca"),
	}
	cfg.AuthInfos["default"] = &clientcmdapi.AuthInfo{
		ClientCertificateData: []byte("cert"),
		ClientKeyData:         []byte("key"),
	}
	cfg.Contexts["default"] = &clientcmdapi.Context{Cluster: "default", AuthInfo: "default"}
	cfg.CurrentContext = "default"
	return cfg
}

func TestRenameContextRewiresEverything(t *testing.T) {
	cfg := fetchedConfig("https://127.0.0.1:6443")
	renameContext(cfg, "api-production")

	// Both distributions name everything "default", which collides the moment a
	// second cluster is managed.
	if _, ok := cfg.Clusters["api-production"]; !ok {
		t.Errorf("clusters = %v, want the renamed entry", keysOfClusters(cfg))
	}
	if _, ok := cfg.AuthInfos["api-production"]; !ok {
		t.Error("auth info was not renamed")
	}
	ctx, ok := cfg.Contexts["api-production"]
	if !ok {
		t.Fatal("context was not renamed")
	}
	// The context's references must be updated too, or it points at nothing.
	if ctx.Cluster != "api-production" {
		t.Errorf("context cluster = %q, want the renamed cluster", ctx.Cluster)
	}
	if ctx.AuthInfo != "api-production" {
		t.Errorf("context auth = %q, want the renamed user", ctx.AuthInfo)
	}
	if cfg.CurrentContext != "api-production" {
		t.Errorf("current context = %q", cfg.CurrentContext)
	}
	// The old names must be gone, not duplicated.
	if len(cfg.Clusters) != 1 || len(cfg.Contexts) != 1 || len(cfg.AuthInfos) != 1 {
		t.Error("rename should replace entries, not add to them")
	}
}

// TestMergePreservesExistingContexts is the critical test here: overwriting
// ~/.kube/config would destroy access to every other cluster the user has.
func TestMergePreservesExistingContexts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")

	existing := clientcmdapi.NewConfig()
	existing.Clusters["prod-eks"] = &clientcmdapi.Cluster{Server: "https://eks.example.com"}
	existing.AuthInfos["prod-eks"] = &clientcmdapi.AuthInfo{Token: "eks-token"}
	existing.Contexts["prod-eks"] = &clientcmdapi.Context{Cluster: "prod-eks", AuthInfo: "prod-eks"}
	existing.CurrentContext = "prod-eks"
	if err := clientcmd.WriteToFile(*existing, path); err != nil {
		t.Fatal(err)
	}

	fetched := fetchedConfig("https://10.0.0.1:6443")
	renameContext(fetched, "api-staging")

	if _, err := MergeKubeconfig(fetched, path, false); err != nil {
		t.Fatalf("MergeKubeconfig: %v", err)
	}

	merged, err := clientcmd.LoadFromFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// The pre-existing cluster must survive untouched.
	if _, ok := merged.Contexts["prod-eks"]; !ok {
		t.Error("merging destroyed an existing context")
	}
	if merged.Clusters["prod-eks"].Server != "https://eks.example.com" {
		t.Error("an existing cluster's server was modified")
	}
	if _, ok := merged.Contexts["api-staging"]; !ok {
		t.Error("the new context was not added")
	}
	// Without --set-current, the user's active context must not move.
	if merged.CurrentContext != "prod-eks" {
		t.Errorf("current context = %q, want it unchanged", merged.CurrentContext)
	}
}

func TestMergeSetsCurrentContextWhenRequested(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")

	fetched := fetchedConfig("https://10.0.0.1:6443")
	renameContext(fetched, "api-staging")

	if _, err := MergeKubeconfig(fetched, path, true); err != nil {
		t.Fatal(err)
	}

	merged, err := clientcmd.LoadFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if merged.CurrentContext != "api-staging" {
		t.Errorf("current context = %q, want api-staging", merged.CurrentContext)
	}
}

func TestMergeCreatesMissingFileAndDirectory(t *testing.T) {
	// A first-time user has no ~/.kube at all.
	path := filepath.Join(t.TempDir(), "nested", "dir", "config")

	fetched := fetchedConfig("https://10.0.0.1:6443")
	renameContext(fetched, "api")

	written, err := MergeKubeconfig(fetched, path, true)
	if err != nil {
		t.Fatalf("MergeKubeconfig: %v", err)
	}
	if written != path {
		t.Errorf("written = %q, want %q", written, path)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file was not created: %v", err)
	}
}

func TestMergedKubeconfigIsNotWorldReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")

	fetched := fetchedConfig("https://10.0.0.1:6443")
	renameContext(fetched, "api")

	if _, err := MergeKubeconfig(fetched, path, true); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// A kubeconfig holds cluster-admin credentials.
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("kubeconfig mode = %04o, want no group or other access", perm)
	}
}

func TestMergeOverwritesSameNamedContext(t *testing.T) {
	// Re-running `cluster kubeconfig` after rotating credentials must replace the
	// stale entry rather than leaving the old one in place.
	path := filepath.Join(t.TempDir(), "config")

	first := fetchedConfig("https://10.0.0.1:6443")
	renameContext(first, "api")
	if _, err := MergeKubeconfig(first, path, true); err != nil {
		t.Fatal(err)
	}

	second := fetchedConfig("https://10.0.0.9:6443")
	renameContext(second, "api")
	if _, err := MergeKubeconfig(second, path, true); err != nil {
		t.Fatal(err)
	}

	merged, err := clientcmd.LoadFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := merged.Clusters["api"].Server; got != "https://10.0.0.9:6443" {
		t.Errorf("server = %q, want the refreshed address", got)
	}
	if len(merged.Contexts) != 1 {
		t.Errorf("contexts = %d, want the entry replaced not duplicated", len(merged.Contexts))
	}
}

func keysOfClusters(cfg *clientcmdapi.Config) []string {
	out := make([]string, 0, len(cfg.Clusters))
	for k := range cfg.Clusters {
		out = append(out, k)
	}
	return out
}
