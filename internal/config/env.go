package config

import "strings"

// ProductionLike reports environment names that must never be torn down as a
// preview. Destroying one is allowed only with an explicit --force, and even
// then only the app objects go — never the namespace.
func ProductionLike(env string) bool {
	switch strings.ToLower(env) {
	case "production", "prod", "live", "main":
		return true
	}
	return false
}

// PreviewLike reports environment names that exist to host per-PR apps.
func PreviewLike(env string) bool {
	switch strings.ToLower(env) {
	case "preview", "review", "pr":
		return true
	}
	return false
}

// ProtectedNamespace reports namespaces that buidl will never delete, even
// when a config marks them ephemeral. These hold cluster services or are the
// default landing zone for unqualified workloads.
func ProtectedNamespace(ns string) bool {
	switch ns {
	case "default", "kube-system", "kube-public", "kube-node-lease",
		"cert-manager", "buidl-system":
		return true
	}
	return false
}
