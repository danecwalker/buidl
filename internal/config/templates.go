package config

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// EnvironmentKind is a named overlay template matching what `buidl init` writes.
type EnvironmentKind string

const (
	EnvironmentStaging    EnvironmentKind = "staging"
	EnvironmentProduction EnvironmentKind = "production"
	EnvironmentPreview    EnvironmentKind = "preview"
)

// ParseEnvironmentKind reports whether s names a built-in overlay template.
func ParseEnvironmentKind(s string) (EnvironmentKind, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "staging":
		return EnvironmentStaging, true
	case "production", "prod", "live":
		return EnvironmentProduction, true
	case "preview", "review", "pr":
		return EnvironmentPreview, true
	default:
		return "", false
	}
}

// InferEnvironmentKind picks a template for a new environment.
//
// An explicit --from template wins. Otherwise a well-known name uses its own
// template, and anything else gets the staging shape: create the namespace,
// stay off production defaults.
func InferEnvironmentKind(name, from string) EnvironmentKind {
	if from != "" {
		if kind, ok := ParseEnvironmentKind(from); ok {
			return kind
		}
	}
	if kind, ok := ParseEnvironmentKind(name); ok {
		return kind
	}
	return EnvironmentStaging
}

// OverlayYAML returns the overlay body `environment new` writes.
//
// Host and namespace follow the init templates when the environment uses a
// well-known name, so `environment new staging` on a file that lost its
// overlay reproduces what init would have written.
func OverlayYAML(kind EnvironmentKind, name, app, host string) string {
	if host == "" {
		host = defaultOverlayHost(kind, name)
	}
	ns := defaultOverlayNamespace(kind, name, app)

	switch kind {
	case EnvironmentProduction:
		return fmt.Sprintf(`deploy:
  kubernetes:
    namespace: %s
  deployTimeout: 10m
proxy:
  host: %s
  ssl: true
`, ns, host)
	case EnvironmentPreview:
		return fmt.Sprintf(`deploy:
  replicas: 1
  kubernetes:
    namespace: %s
    createNamespace: true
    ephemeral: true
proxy:
  host: %s
  ssl: true
`, ns, host)
	default:
		return fmt.Sprintf(`deploy:
  kubernetes:
    namespace: %s
    createNamespace: true
proxy:
  host: %s
  ssl: true
env:
  clear:
    LOG_LEVEL: debug
`, ns, host)
	}
}

// OverlayNode parses OverlayYAML into a mapping node.
func OverlayNode(kind EnvironmentKind, name, app, host string) (*yaml.Node, error) {
	return ParseNode(OverlayYAML(kind, name, app, host))
}

func defaultOverlayHost(kind EnvironmentKind, name string) string {
	switch kind {
	case EnvironmentProduction:
		if name == "production" {
			return "example.com"
		}
		return name + ".example.com"
	case EnvironmentPreview:
		if name == "preview" {
			return "${BUIDL_SLUG}.preview.example.com"
		}
		return "${BUIDL_SLUG}." + name + ".example.com"
	default:
		if name == "staging" {
			return "staging.example.com"
		}
		return name + ".example.com"
	}
}

func defaultOverlayNamespace(kind EnvironmentKind, name, app string) string {
	switch kind {
	case EnvironmentProduction:
		if name == "production" {
			return app
		}
		return app + "-" + name
	case EnvironmentPreview:
		if name == "preview" {
			return app + "-preview-${BUIDL_SLUG}"
		}
		return app + "-" + name + "-${BUIDL_SLUG}"
	default:
		return app + "-" + name
	}
}
