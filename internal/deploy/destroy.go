package deploy

import (
	"strings"
	"time"

	"github.com/danecwalker/buidl/internal/config"
)

// DestroyRequest asks a backend to tear an environment down.
type DestroyRequest struct {
	Config *config.Config
	Root   string
	// Slug is the current BUIDL_SLUG, used only to recognise a namespace that
	// was derived from the branch or PR. Empty is fine when the config marks
	// the environment ephemeral explicitly.
	Slug string
	// DryRun reports what would be deleted without deleting it.
	DryRun bool
	// StaleAfter, when greater than zero, deletes every matching ephemeral
	// namespace older than this duration instead of the current slug's
	// namespace. That is the leak backstop for a missed PR-closed event.
	StaleAfter time.Duration
}

// DestroyMode is how teardown is carried out.
type DestroyMode string

const (
	// DestroyModeNamespace deletes the whole namespace. Preview apps live in a
	// namespace of their own, so this is the complete teardown.
	DestroyModeNamespace DestroyMode = "namespace"
	// DestroyModeObjects deletes buidl-managed app objects and leaves the
	// namespace (and any accessories) in place. Used for long-lived
	// environments where the namespace is shared with data that must survive.
	DestroyModeObjects DestroyMode = "objects"
	// DestroyModeStale is a sweep of several ephemeral namespaces.
	DestroyModeStale DestroyMode = "stale"
	// DestroyModeNone means there was nothing to delete (already gone).
	DestroyModeNone DestroyMode = "none"
)

// DestroyOutcome reports what teardown did.
type DestroyOutcome struct {
	Environment string
	Namespace   string
	Mode        DestroyMode
	Changes     []Change
	// Namespaces lists every namespace removed (or that would be removed) by a
	// stale sweep. Single-namespace destroy puts the one name in Namespace.
	Namespaces []string
}

// DestroyScope is the policy decision for one environment, independent of
// whether the objects currently exist.
type DestroyScope int

const (
	// ScopeRefused means destroy must not run. The namespace is protected or
	// the configuration is unsafe.
	ScopeRefused DestroyScope = iota
	// ScopeNamespace means delete the whole namespace.
	ScopeNamespace
	// ScopeObjects means delete managed app objects only.
	ScopeObjects
)

// DestroyDecision is the policy outcome for one config.
type DestroyDecision struct {
	Scope  DestroyScope
	Reason string
}

// DecideDestroy chooses how to tear an environment down.
//
// The unit of teardown for a preview app is the namespace: that is why preview
// namespaces are slug-derived and created by buidl. Staging and production
// share a namespace with accessories and release history, so they lose only
// the app objects. Production-like names are not refused here — the CLI
// requires --force for those — because a deliberate teardown of the app
// (leaving the database) is a real operation.
func DecideDestroy(cfg *config.Config, slug string) DestroyDecision {
	ns := cfg.Deploy.Kubernetes.Namespace
	if config.ProtectedNamespace(ns) {
		return DestroyDecision{
			Scope:  ScopeRefused,
			Reason: "refusing to destroy protected namespace " + quote(ns),
		}
	}

	if ephemeral(cfg, slug) && cfg.Deploy.Kubernetes.CreateNamespace {
		return DestroyDecision{
			Scope:  ScopeNamespace,
			Reason: "ephemeral preview namespace",
		}
	}
	return DestroyDecision{
		Scope:  ScopeObjects,
		Reason: "long-lived environment; accessories and the namespace are left in place",
	}
}

// IsEphemeral reports whether this environment is a disposable preview.
func IsEphemeral(cfg *config.Config) bool {
	return ephemeral(cfg, "")
}

func ephemeral(cfg *config.Config, slug string) bool {
	if cfg.Deploy.Kubernetes.Ephemeral != nil {
		return *cfg.Deploy.Kubernetes.Ephemeral
	}
	// Implicit: a preview-like environment whose namespace was created for
	// this branch/PR. The namespace must not be the app's default name —
	// that is how a misconfigured preview: block would otherwise delete the
	// shared app namespace.
	if !config.PreviewLike(cfg.Environment) || !cfg.Deploy.Kubernetes.CreateNamespace {
		return false
	}
	return namespaceLooksPreview(cfg.Deploy.Kubernetes.Namespace, cfg.App, slug)
}

func namespaceLooksPreview(ns, app, slug string) bool {
	if ns == "" || ns == "default" || ns == app {
		return false
	}
	if strings.Contains(ns, "preview") || strings.Contains(ns, "review") {
		return true
	}
	if slug != "" && slug != "unknown" && strings.Contains(ns, slug) {
		return true
	}
	return false
}

func quote(s string) string {
	return `"` + s + `"`
}
