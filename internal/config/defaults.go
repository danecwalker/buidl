package config

import (
	"strings"
)

// Default values applied when a field is omitted. They are chosen so that a
// minimal four-line config produces a safe, zero-downtime production deploy.
const (
	DefaultPort            = int32(8080)
	DefaultHealthcheckPath = "/up"
	DefaultHooksPath       = ".buidl/hooks"
	DefaultRetainReleases  = 10
	DefaultCacheSuffix     = ":buildcache"
	DefaultAutoscaleCPU    = int32(70)
	DefaultRequestCPU      = "100m"
	DefaultRequestMemory   = "128Mi"
)

// applyDefaults fills in omitted fields. It is idempotent.
func applyDefaults(c *Config) {
	if c.Version == 0 {
		c.Version = SchemaVersion
	}
	if c.RetainReleases == 0 {
		c.RetainReleases = DefaultRetainReleases
	}
	if c.HooksPath == "" {
		c.HooksPath = DefaultHooksPath
	}

	// --- build ---
	if c.Build.Driver == "" {
		c.Build.Driver = DriverBuildKit
	}
	if c.Build.Context == "" {
		c.Build.Context = "."
	}
	if c.Build.Dockerfile == "" {
		c.Build.Dockerfile = "Dockerfile"
	}
	if len(c.Build.Platforms) == 0 {
		// Single-platform by default: multi-arch roughly doubles build time and
		// most clusters are homogeneous. Opt in explicitly.
		c.Build.Platforms = []string{"linux/amd64"}
	}
	if c.Build.Cache == "" {
		c.Build.Cache = "registry"
	}
	if c.Build.CacheRef == "" && c.Image != "" {
		c.Build.CacheRef = c.Image + DefaultCacheSuffix
	}

	// Infer the registry host from the image reference so `registry.server`
	// rarely needs to be written out.
	if c.Registry.Server == "" {
		c.Registry.Server = registryFromImage(c.Image)
	}

	// --- deploy ---
	if c.Deploy.Target == "" {
		c.Deploy.Target = "kubernetes"
	}
	if c.Deploy.Port == 0 {
		c.Deploy.Port = DefaultPort
	}
	if c.Deploy.Kubernetes.Namespace == "" {
		c.Deploy.Kubernetes.Namespace = c.App
	}

	// --- healthcheck ---
	// Applied before scale defaults: an HTTP probe is how we decide whether to
	// turn on an HPA. A worker with only an exec probe stays at one replica.
	hc := &c.Deploy.Healthcheck
	if hc.Path == "" && len(hc.Command) == 0 {
		hc.Path = DefaultHealthcheckPath
	}
	if hc.Port == 0 {
		hc.Port = c.Deploy.Port
	}
	if hc.PeriodSeconds == 0 {
		hc.PeriodSeconds = 10
	}
	if hc.TimeoutSeconds == 0 {
		hc.TimeoutSeconds = 5
	}
	if hc.FailureThreshold == 0 {
		hc.FailureThreshold = 3
	}

	applyScaleDefaults(c)

	// --- strategy ---
	st := &c.Deploy.Strategy
	if st.Type == "" {
		st.Type = StrategyRolling
	}
	if st.MaxSurge == "" {
		st.MaxSurge = "25%"
	}
	if st.MaxUnavailable == "" {
		// Zero-downtime is the whole point, so never voluntarily drop capacity.
		st.MaxUnavailable = "0"
	}

	// --- timeouts ---
	if c.Deploy.DeployTimeout.Duration == 0 {
		c.Deploy.DeployTimeout.Duration = defaultDeployTimeout
	}
	if c.Deploy.DrainTimeout.Duration == 0 {
		c.Deploy.DrainTimeout.Duration = defaultDrainTimeout
	}

	// --- proxy ---
	if c.Proxy.Enabled == nil {
		// An app with a hostname wants ingress; a worker without one does not.
		enabled := c.Proxy.Host != "" || len(c.Proxy.Hosts) > 0
		c.Proxy.Enabled = &enabled
	}
	if c.Proxy.SSL && c.Proxy.ClusterIssuer == "" {
		c.Proxy.ClusterIssuer = "letsencrypt-prod"
	}

	applyInfraDefaults(c)

	// --- accessories ---
	for name, acc := range c.Accessories {
		if acc.MountPath == "" && acc.Storage != "" {
			acc.MountPath = defaultMountPath(acc.Image)
		}
		c.Accessories[name] = acc
	}
}

// registryFromImage extracts the registry host from an image reference.
// "ghcr.io/acme/web" -> "ghcr.io"; "acme/web" -> "docker.io" (the implicit
// Docker Hub default, matching the OCI reference grammar).
func registryFromImage(image string) string {
	if image == "" {
		return ""
	}
	parts := strings.SplitN(image, "/", 2)
	if len(parts) == 1 {
		return "docker.io"
	}
	host := parts[0]
	// A first segment is only a registry if it looks like a host: it contains a
	// dot, a colon (port), or is exactly "localhost".
	if strings.ContainsAny(host, ".:") || host == "localhost" {
		return host
	}
	return "docker.io"
}

// defaultMountPath guesses the data directory for well-known accessory images so
// a Postgres accessory doesn't need a mountPath spelled out.
func defaultMountPath(image string) string {
	name := image
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	if i := strings.IndexAny(name, ":@"); i >= 0 {
		name = name[:i]
	}
	switch name {
	case "postgres", "postgresql":
		return "/var/lib/postgresql/data"
	case "mysql", "mariadb":
		return "/var/lib/mysql"
	case "redis", "valkey":
		return "/data"
	case "mongo", "mongodb":
		return "/data/db"
	default:
		return "/data"
	}
}

// applyScaleDefaults chooses static replicas vs an HPA when the user omitted
// both, then fills HPA bounds and the resource requests utilization needs.
func applyScaleDefaults(c *Config) {
	if c.Deploy.Replicas == nil && c.Deploy.Autoscale == nil {
		switch {
		case PreviewLike(c.Environment):
			// A preview is one user kicking the tyres. Autoscaling it wastes
			// cluster capacity and makes the namespace harder to reason about.
			one := int32(1)
			c.Deploy.Replicas = &one
		case isHTTPApp(c):
			c.Deploy.Autoscale = &Autoscale{CPUPercent: DefaultAutoscaleCPU}
		default:
			one := int32(1)
			c.Deploy.Replicas = &one
		}
	}

	as := c.Deploy.Autoscale
	if as == nil {
		if c.Deploy.Replicas == nil {
			one := int32(1)
			c.Deploy.Replicas = &one
		}
		return
	}

	if as.CPUPercent == 0 && as.MemoryPercent == 0 {
		as.CPUPercent = DefaultAutoscaleCPU
	}
	if as.Min == 0 {
		as.derivedMin = true
	}
	if as.Max == 0 {
		as.derivedMax = true
	}

	// CPU/memory utilization is a ratio of usage to requests. Without requests
	// the HPA cannot compute a signal and never scales.
	if c.Deploy.Resources.Requests == nil {
		c.Deploy.Resources.Requests = map[string]string{}
	}
	if c.Deploy.Resources.Requests["cpu"] == "" {
		c.Deploy.Resources.Requests["cpu"] = DefaultRequestCPU
	}
	if c.Deploy.Resources.Requests["memory"] == "" {
		c.Deploy.Resources.Requests["memory"] = DefaultRequestMemory
	}

	ResolveAutoscale(c, FleetSize(c))
}

// isHTTPApp reports whether the healthcheck is an HTTP probe. Workers that
// only expose an exec probe are not assumed to want traffic-based scaling.
func isHTTPApp(c *Config) bool {
	return c.Deploy.Healthcheck.Path != "" && len(c.Deploy.Healthcheck.Command) == 0
}
