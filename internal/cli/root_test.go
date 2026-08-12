package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/danewalker/buidl/internal/config"
	"github.com/danewalker/buidl/internal/ui"
)

// writeKubeconfig writes a kubeconfig holding the named contexts and points
// KUBECONFIG at it.
func writeKubeconfig(t *testing.T, current string, contexts ...string) {
	t.Helper()

	cfg := clientcmdapi.NewConfig()
	for _, name := range contexts {
		cfg.Clusters[name] = &clientcmdapi.Cluster{Server: "https://" + name + ".example.com:6443"}
		cfg.AuthInfos[name] = &clientcmdapi.AuthInfo{Token: "t"}
		cfg.Contexts[name] = &clientcmdapi.Context{Cluster: name, AuthInfo: name}
	}
	cfg.CurrentContext = current

	path := filepath.Join(t.TempDir(), "config")
	if err := clientcmd.WriteToFile(*cfg, path); err != nil {
		t.Fatalf("writing kubeconfig: %v", err)
	}
	t.Setenv("KUBECONFIG", path)
}

// TestManagedContextRefusesToAddressTheWrongCluster is the guard against the most
// alarming wrong answer buidl can give: on a machine that never ran deploy,
// falling through to the kubeconfig's current context made `status -e production`
// query docker-desktop and report the app as not deployed.
func TestManagedContext(t *testing.T) {
	tests := []struct {
		name string
		cfg  *config.Config
		// current is the kubeconfig's current context, and contexts what it holds.
		current  string
		contexts []string
		want     string
		wantErr  string
	}{
		{
			name:     "no infra leaves resolution to the kubeconfig",
			cfg:      &config.Config{App: "api", Environment: "production"},
			current:  "docker-desktop",
			contexts: []string{"docker-desktop"},
			want:     "",
		},
		{
			name: "a pinned context is the guardrail and wins",
			cfg: &config.Config{
				App:         "api",
				Environment: "production",
				Infra:       &config.Infra{},
				Deploy:      config.Deploy{Kubernetes: config.Kubernetes{Context: "pinned"}},
			},
			current:  "docker-desktop",
			contexts: []string{"docker-desktop"},
			want:     "",
		},
		{
			name:     "managed context present is adopted",
			cfg:      &config.Config{App: "api", Environment: "production", Infra: &config.Infra{}},
			current:  "docker-desktop",
			contexts: []string{"docker-desktop", "api-production"},
			want:     "api-production",
		},
		{
			// The regression: a different cluster is current and the managed one was
			// never fetched here.
			name:     "managed context missing is a hard error",
			cfg:      &config.Config{App: "api", Environment: "production", Infra: &config.Infra{}},
			current:  "docker-desktop",
			contexts: []string{"docker-desktop"},
			wantErr:  "api-production",
		},
		{
			name:     "single-environment config names the app alone",
			cfg:      &config.Config{App: "api", Environment: "default", Infra: &config.Infra{}},
			current:  "",
			contexts: []string{"api"},
			want:     "api",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			writeKubeconfig(t, tt.current, tt.contexts...)

			app, _ := newTestApp(t, ui.ModePlain)
			app.cfg = tt.cfg

			got, err := app.managedContext()
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("managedContext = %q, want an error naming the missing context", got)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error = %q, want it to name %q", err, tt.wantErr)
				}
				// The user cannot act on the error without being told how to fix it.
				if !strings.Contains(err.Error(), "buidl cluster kubeconfig") {
					t.Errorf("error = %q, want the command that fetches credentials", err)
				}
				// The current context must never be offered as a substitute.
				if strings.Contains(err.Error(), "docker-desktop") {
					t.Errorf("error = %q, must not suggest the current context", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("managedContext: %v", err)
			}
			if got != tt.want {
				t.Errorf("managedContext = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestEnvironmentFlagHint keeps the hint copy-pasteable: a wrong -e would send
// the user to fetch credentials for the wrong cluster.
func TestEnvironmentFlagHint(t *testing.T) {
	app, _ := newTestApp(t, ui.ModePlain)

	app.cfg = &config.Config{Environment: "production"}
	if got := app.environmentFlag(); got != " -e production" {
		t.Errorf("environmentFlag = %q, want the -e flag", got)
	}

	// A single-environment config has no -e to pass; suggesting one would fail.
	app.cfg = &config.Config{Environment: "default"}
	if got := app.environmentFlag(); got != "" {
		t.Errorf("environmentFlag = %q, want empty for the default environment", got)
	}
}
