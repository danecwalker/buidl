package deploy

import (
	"testing"

	"github.com/danecwalker/buidl/internal/config"
)

func TestDecideDestroy(t *testing.T) {
	trueVal := true
	falseVal := false

	tests := []struct {
		name string
		cfg  *config.Config
		slug string
		want DestroyScope
	}{
		{
			name: "explicit ephemeral preview deletes the namespace",
			cfg: &config.Config{
				App:         "web",
				Environment: "preview",
				Deploy: config.Deploy{Kubernetes: config.Kubernetes{
					Namespace:       "web-preview-pr-12",
					CreateNamespace: true,
					Ephemeral:       &trueVal,
				}},
			},
			slug: "pr-12",
			want: ScopeNamespace,
		},
		{
			name: "implicit preview with slug-derived namespace",
			cfg: &config.Config{
				App:         "web",
				Environment: "preview",
				Deploy: config.Deploy{Kubernetes: config.Kubernetes{
					Namespace:       "web-preview-pr-12",
					CreateNamespace: true,
				}},
			},
			slug: "pr-12",
			want: ScopeNamespace,
		},
		{
			name: "preview opted out stays object-only",
			cfg: &config.Config{
				App:         "web",
				Environment: "preview",
				Deploy: config.Deploy{Kubernetes: config.Kubernetes{
					Namespace:       "web-preview-pr-12",
					CreateNamespace: true,
					Ephemeral:       &falseVal,
				}},
			},
			want: ScopeObjects,
		},
		{
			name: "preview landing in the app namespace is not ephemeral",
			cfg: &config.Config{
				App:         "web",
				Environment: "preview",
				Deploy: config.Deploy{Kubernetes: config.Kubernetes{
					Namespace:       "web",
					CreateNamespace: true,
				}},
			},
			want: ScopeObjects,
		},
		{
			name: "staging keeps the namespace",
			cfg: &config.Config{
				App:         "web",
				Environment: "staging",
				Deploy: config.Deploy{Kubernetes: config.Kubernetes{
					Namespace:       "web-staging",
					CreateNamespace: true,
				}},
			},
			want: ScopeObjects,
		},
		{
			name: "production is object-only (CLI still requires --force)",
			cfg: &config.Config{
				App:         "web",
				Environment: "production",
				Deploy: config.Deploy{Kubernetes: config.Kubernetes{
					Namespace: "web",
				}},
			},
			want: ScopeObjects,
		},
		{
			name: "protected namespace is refused",
			cfg: &config.Config{
				App:         "web",
				Environment: "preview",
				Deploy: config.Deploy{Kubernetes: config.Kubernetes{
					Namespace:       "default",
					CreateNamespace: true,
					Ephemeral:       &trueVal,
				}},
			},
			want: ScopeRefused,
		},
		{
			name: "kube-system is refused",
			cfg: &config.Config{
				App:         "web",
				Environment: "preview",
				Deploy: config.Deploy{Kubernetes: config.Kubernetes{
					Namespace: "kube-system",
					Ephemeral: &trueVal,
				}},
			},
			want: ScopeRefused,
		},
		{
			name: "slug in namespace of a custom preview-like env",
			cfg: &config.Config{
				App:         "web",
				Environment: "review",
				Deploy: config.Deploy{Kubernetes: config.Kubernetes{
					Namespace:       "web-feature-oauth",
					CreateNamespace: true,
				}},
			},
			slug: "feature-oauth",
			want: ScopeNamespace,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DecideDestroy(tt.cfg, tt.slug)
			if got.Scope != tt.want {
				t.Errorf("Scope = %v (%s), want %v", got.Scope, got.Reason, tt.want)
			}
		})
	}
}
