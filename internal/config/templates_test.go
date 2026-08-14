package config

import (
	"strings"
	"testing"
)

func TestOverlayYAMLMatchesInitShape(t *testing.T) {
	tests := []struct {
		kind     EnvironmentKind
		name     string
		contains []string
	}{
		{
			kind: EnvironmentStaging, name: "staging",
			contains: []string{"namespace: web-staging", "createNamespace: true", "host: staging.example.com", "LOG_LEVEL: debug"},
		},
		{
			kind: EnvironmentProduction, name: "production",
			contains: []string{"namespace: web", "deployTimeout: 10m", "host: example.com"},
		},
		{
			kind: EnvironmentPreview, name: "preview",
			contains: []string{
				"namespace: web-preview-${BUIDL_SLUG}",
				"ephemeral: true",
				"host: ${BUIDL_SLUG}.preview.example.com",
				"replicas: 1",
			},
		},
	}

	for _, tt := range tests {
		t.Run(string(tt.kind), func(t *testing.T) {
			raw := OverlayYAML(tt.kind, tt.name, "web", "")
			for _, want := range tt.contains {
				if !strings.Contains(raw, want) {
					t.Errorf("overlay missing %q:\n%s", want, raw)
				}
			}

			// A template that does not load would make `environment new` write
			// a file the rest of the tool cannot use.
			path := write(t, "app: web\nimage: ghcr.io/acme/web\nenvironments:\n  "+tt.name+":\n"+indent(raw, 4))
			_, err := Load(LoadOptions{
				Path:        path,
				Environment: tt.name,
				Strict:      true,
				Vars:        map[string]string{"BUIDL_SLUG": "example"},
			})
			if err != nil {
				t.Fatalf("overlay must load: %v\n%s", err, raw)
			}
		})
	}
}

func TestInferEnvironmentKind(t *testing.T) {
	if got := InferEnvironmentKind("qa", ""); got != EnvironmentStaging {
		t.Errorf("custom name = %q, want staging", got)
	}
	if got := InferEnvironmentKind("production", ""); got != EnvironmentProduction {
		t.Errorf("production name = %q", got)
	}
	if got := InferEnvironmentKind("qa", "preview"); got != EnvironmentPreview {
		t.Errorf("--from preview = %q", got)
	}
}

func indent(s string, n int) string {
	pad := strings.Repeat(" ", n)
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	for i, line := range lines {
		if line != "" {
			lines[i] = pad + line
		}
	}
	return strings.Join(lines, "\n") + "\n"
}
