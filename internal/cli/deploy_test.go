package cli

import (
	"strings"
	"testing"

	"github.com/danewalker/buidl/internal/config"
	"github.com/danewalker/buidl/internal/ui"
)

// TestCheckPromoteRepositories covers the case where a promotion would pair the
// source's digest with the destination's repository. The reference does not
// exist, and preflight reports it as "image ... is not available", which reads
// like a registry outage rather than a config mismatch.
func TestCheckPromoteRepositories(t *testing.T) {
	tests := []struct {
		name    string
		source  string
		dest    string
		wantErr bool
	}{
		{
			name:   "one image for every environment",
			source: "ghcr.io/acme/web",
			dest:   "ghcr.io/acme/web",
		},
		{
			name:    "image overlaid per environment",
			source:  "ghcr.io/acme/web-staging",
			dest:    "ghcr.io/acme/web",
			wantErr: true,
		},
		{
			// Same repository name in a different registry is still a different
			// repository, and the digest is not there either.
			name:    "different registry",
			source:  "ghcr.io/acme/web",
			dest:    "registry.acme.com/acme/web",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkPromoteRepositories("staging", tt.source, "production", tt.dest)
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("checkPromoteRepositories: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("want an error; a digest does not exist in another repository")
			}
			// The message must name the real cause, not look like a registry problem.
			if !strings.Contains(err.Error(), "cannot cross repositories") {
				t.Errorf("error = %q, want it to say promote cannot cross repositories", err)
			}
			// Both repositories belong in it, or the user cannot see which overlay
			// caused this.
			for _, want := range []string{tt.source, tt.dest} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want it to name %q", err, want)
				}
			}
		})
	}
}

// TestPromoteRejectsEnvironmentFlag guards a silent lie: --from and --to
// overwrite -e, so `promote -e staging --from a --to b` read as though staging
// were involved while touching neither.
func TestPromoteRejectsEnvironmentFlag(t *testing.T) {
	app, _ := newTestApp(t, ui.ModePlain)
	app.opts.environment = "staging"

	cmd := newPromoteCmd(app)
	cmd.SetArgs([]string{"--from", "a", "--to", "b"})
	cmd.SetOut(&strings.Builder{})
	cmd.SetErr(&strings.Builder{})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("want an error; -e is ignored by promote")
	}
	if !strings.Contains(err.Error(), "--from and --to") {
		t.Errorf("error = %q, want it to point at --from/--to", err)
	}
}

// TestClusterDescription covers the one prompt whose job is to say which cluster
// is about to be touched. It ran before convergence set the context field, so it
// printed "(cluster: )".
func TestClusterDescription(t *testing.T) {
	tests := []struct {
		name     string
		cfg      *config.Config
		current  string
		contexts []string
		want     string
	}{
		{
			name: "explicit context",
			cfg: &config.Config{
				App:         "api",
				Environment: "production",
				Deploy:      config.Deploy{Kubernetes: config.Kubernetes{Context: "acme-prod"}},
			},
			want: "acme-prod",
		},
		{
			// The regression: a managed cluster's context is set during convergence,
			// which has not happened when the prompt runs.
			name:     "managed cluster before convergence",
			cfg:      &config.Config{App: "api", Environment: "production", Infra: &config.Infra{}},
			current:  "docker-desktop",
			contexts: []string{"docker-desktop"},
			want:     "api-production",
		},
		{
			name:     "unmanaged cluster falls back to what a deploy would use",
			cfg:      &config.Config{App: "api", Environment: "production"},
			current:  "acme-eks",
			contexts: []string{"acme-eks"},
			want:     "acme-eks (current kubeconfig context)",
		},
		{
			name:     "nothing selected says so rather than nothing",
			cfg:      &config.Config{App: "api", Environment: "production"},
			current:  "",
			contexts: nil,
			want:     "unknown",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			writeKubeconfig(t, tt.current, tt.contexts...)

			app, _ := newTestApp(t, ui.ModePlain)
			app.cfg = tt.cfg

			if got := app.clusterDescription(); got != tt.want {
				t.Errorf("clusterDescription = %q, want %q", got, tt.want)
			}
		})
	}
}
