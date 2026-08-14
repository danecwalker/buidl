package config

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	defaultDeployTimeout = 5 * time.Minute
	defaultDrainTimeout  = 30 * time.Second
)

// dnsLabel matches RFC 1123 label syntax, which is what Kubernetes requires for
// object names and what we derive from App and accessory names.
var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// envVarName matches POSIX-ish environment variable names.
var envVarName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// quantity loosely matches Kubernetes resource quantities ("100m", "1", "512Mi",
// "1.5"). Exact parsing happens in the Kubernetes renderer; this catches typos
// early, at config-load time, with a better error message.
var quantity = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?(m|k|Ki|M|Mi|G|Gi|T|Ti|P|Pi|E|Ei)?$`)

// Errors aggregates every problem found in a config so users fix them in one
// pass instead of one-per-run.
type Errors []string

func (e Errors) Error() string {
	if len(e) == 1 {
		return e[0]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d configuration problems:", len(e))
	for _, msg := range e {
		fmt.Fprintf(&b, "\n  - %s", msg)
	}
	return b.String()
}

// Validate checks a defaulted Config for internal consistency.
func Validate(c *Config) error {
	var errs Errors
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Sprintf(format, args...))
	}

	if c.Version > SchemaVersion {
		add("version %d is newer than this buidl build supports (max %d); upgrade buidl", c.Version, SchemaVersion)
	}

	// --- app ---
	switch {
	case c.App == "":
		add("`app` is required")
	case len(c.App) > 63:
		add("`app` must be 63 characters or fewer (got %d)", len(c.App))
	case !dnsLabel.MatchString(c.App):
		add("`app` must be lowercase alphanumeric with dashes (got %q)", c.App)
	}

	// --- image ---
	if c.Image == "" {
		if c.Build.Driver != DriverPrebuilt {
			add("`image` is required (e.g. ghcr.io/acme/%s)", orPlaceholder(c.App))
		}
	} else {
		validateImageRef(c.Image, add)
	}

	// --- build ---
	switch c.Build.Driver {
	case DriverBuildKit, DriverPrebuilt:
	default:
		add("`build.driver` must be %q or %q (got %q)", DriverBuildKit, DriverPrebuilt, c.Build.Driver)
	}
	switch c.Build.Cache {
	case "registry", "inline", "none":
	default:
		add("`build.cache` must be one of registry, inline, none (got %q)", c.Build.Cache)
	}
	for _, p := range c.Build.Platforms {
		if strings.Count(p, "/") < 1 {
			add("`build.platforms` entry %q must look like os/arch (e.g. linux/amd64)", p)
		}
	}
	for id := range c.Build.Secrets {
		if id == "" {
			add("`build.secrets` keys must be non-empty secret ids")
		}
	}

	// --- deploy ---
	if c.Deploy.Target != "kubernetes" {
		add("`deploy.target` %q is not supported yet (only \"kubernetes\" is implemented)", c.Deploy.Target)
	}
	if c.Deploy.Port < 1 || c.Deploy.Port > 65535 {
		add("`deploy.port` must be between 1 and 65535 (got %d)", c.Deploy.Port)
	}
	if c.Deploy.Replicas != nil && *c.Deploy.Replicas < 0 {
		add("`deploy.replicas` cannot be negative (got %d)", *c.Deploy.Replicas)
	}
	if ns := c.Deploy.Kubernetes.Namespace; ns != "" && !dnsLabel.MatchString(ns) {
		add("`deploy.kubernetes.namespace` must be a valid DNS label (got %q)", ns)
	}
	if c.Deploy.Kubernetes.Ephemeral != nil && *c.Deploy.Kubernetes.Ephemeral {
		// An ephemeral production environment would make `destroy` delete the
		// live namespace. Catch that at load time, not at teardown.
		if ProductionLike(c.Environment) {
			add("`deploy.kubernetes.ephemeral` cannot be true for environment %q", c.Environment)
		}
		if ns := c.Deploy.Kubernetes.Namespace; ProtectedNamespace(ns) {
			add("`deploy.kubernetes.ephemeral` cannot target protected namespace %q", ns)
		}
	}

	// --- healthcheck ---
	hc := c.Deploy.Healthcheck
	if hc.Path != "" && len(hc.Command) > 0 {
		add("set either `deploy.healthcheck.path` or `deploy.healthcheck.command`, not both")
	}
	if hc.Path != "" && !strings.HasPrefix(hc.Path, "/") {
		add("`deploy.healthcheck.path` must start with / (got %q)", hc.Path)
	}
	if hc.Port < 1 || hc.Port > 65535 {
		add("`deploy.healthcheck.port` must be between 1 and 65535 (got %d)", hc.Port)
	}

	// --- strategy ---
	switch c.Deploy.Strategy.Type {
	case StrategyRolling, StrategyBlueGreen, StrategyRecreate:
	default:
		add("`deploy.strategy.type` must be one of rolling, bluegreen, recreate (got %q)", c.Deploy.Strategy.Type)
	}
	validateSurge("deploy.strategy.maxSurge", c.Deploy.Strategy.MaxSurge, add)
	validateSurge("deploy.strategy.maxUnavailable", c.Deploy.Strategy.MaxUnavailable, add)

	// A rolling update that may take every pod down at once is not zero-downtime.
	// Flag it rather than silently shipping an outage.
	if c.Deploy.Strategy.Type == StrategyRolling && c.Deploy.Strategy.MaxSurge == "0" && c.Deploy.Strategy.MaxUnavailable == "0" {
		add("`deploy.strategy` sets both maxSurge and maxUnavailable to 0, which can never make progress")
	}

	// --- autoscale ---
	// min/max of 0 mean "derive from the fleet" and are filled by
	// ResolveAutoscale. Reject only impossible combinations.
	if as := c.Deploy.Autoscale; as != nil {
		if as.Min < 0 {
			add("`deploy.autoscale.min` cannot be negative (got %d)", as.Min)
		}
		if as.Max < 0 {
			add("`deploy.autoscale.max` cannot be negative (got %d)", as.Max)
		}
		if as.Min > 0 && as.Max > 0 && as.Max < as.Min {
			add("`deploy.autoscale.max` (%d) must be >= min (%d)", as.Max, as.Min)
		}
		if as.CPUPercent == 0 && as.MemoryPercent == 0 {
			add("`deploy.autoscale` needs cpuPercent and/or memoryPercent to have a scaling signal")
		}
	}

	validateResources("deploy.resources", c.Deploy.Resources, add)
	validateEnv("env", c.Env, add)
	validateInfra(c, add)

	// --- proxy ---
	if c.Proxy.Enabled != nil && *c.Proxy.Enabled {
		if c.Proxy.Host == "" && len(c.Proxy.Hosts) == 0 {
			add("`proxy.enabled` is true but no `proxy.host` is set")
		}
		for _, h := range append([]string{c.Proxy.Host}, c.Proxy.Hosts...) {
			if h == "" {
				continue
			}
			if strings.ContainsAny(h, "/:") {
				add("`proxy.host` must be a bare hostname without scheme or port (got %q)", h)
			}
		}
	}
	if c.Proxy.SSL && (c.Proxy.Enabled == nil || !*c.Proxy.Enabled) {
		add("`proxy.ssl` is set but the proxy is disabled")
	}

	// --- accessories ---
	for name, acc := range c.Accessories {
		prefix := "accessories." + name
		if !dnsLabel.MatchString(name) {
			add("%s: accessory name must be a valid DNS label", prefix)
		}
		if acc.Image == "" {
			add("%s.image is required", prefix)
		}
		if acc.Port != 0 && (acc.Port < 1 || acc.Port > 65535) {
			add("%s.port must be between 1 and 65535 (got %d)", prefix, acc.Port)
		}
		if acc.Storage != "" && !quantity.MatchString(acc.Storage) {
			add("%s.storage %q is not a valid size (e.g. 10Gi)", prefix, acc.Storage)
		}
		validateResources(prefix+".resources", acc.Resources, add)
		validateEnv(prefix+".env", acc.Env, add)
	}

	if len(errs) > 0 {
		return errs
	}
	return nil
}

// validateImageRef rejects references that carry a tag or digest. Tags are
// assigned per release by buidl, so a pinned tag in config would either be
// ignored or silently override the release identity.
func validateImageRef(image string, add func(string, ...any)) {
	if strings.Contains(image, "@") {
		add("`image` must not include a digest; buidl resolves digests per release (got %q)", image)
		return
	}
	// A colon after the last slash is a tag. A colon before it is a registry port.
	lastSlash := strings.LastIndex(image, "/")
	if i := strings.LastIndex(image, ":"); i > lastSlash {
		add("`image` must not include a tag; buidl tags each release (use %q)", image[:i])
		return
	}
	if strings.ToLower(image) != image {
		add("`image` must be lowercase (got %q)", image)
	}
}

// validateSurge accepts a count or a percentage.
func validateSurge(field, value string, add func(string, ...any)) {
	if value == "" {
		return
	}
	if strings.HasSuffix(value, "%") {
		n, err := strconv.Atoi(strings.TrimSuffix(value, "%"))
		if err != nil || n < 0 || n > 100 {
			add("`%s` must be a percentage between 0%% and 100%% (got %q)", field, value)
		}
		return
	}
	if n, err := strconv.Atoi(value); err != nil || n < 0 {
		add("`%s` must be a non-negative count or a percentage (got %q)", field, value)
	}
}

func validateResources(prefix string, r Resources, add func(string, ...any)) {
	check := func(kind string, m map[string]string) {
		for k, v := range m {
			switch k {
			case "cpu", "memory", "ephemeral-storage":
			default:
				if !strings.Contains(k, "/") {
					add("%s.%s.%s is not a recognized resource name", prefix, kind, k)
				}
			}
			if !quantity.MatchString(v) {
				add("%s.%s.%s = %q is not a valid quantity (e.g. 100m, 512Mi)", prefix, kind, k, v)
			}
		}
	}
	check("requests", r.Requests)
	check("limits", r.Limits)
}

func validateEnv(prefix string, e Env, add func(string, ...any)) {
	for k := range e.Clear {
		if !envVarName.MatchString(k) {
			add("%s.clear: %q is not a valid environment variable name", prefix, k)
		}
	}
	seen := map[string]bool{}
	for _, k := range e.Secret {
		if !envVarName.MatchString(k) {
			add("%s.secret: %q is not a valid environment variable name", prefix, k)
		}
		if seen[k] {
			add("%s.secret: %q is listed twice", prefix, k)
		}
		seen[k] = true
		if _, clash := e.Clear[k]; clash {
			add("%s: %q is declared in both clear and secret", prefix, k)
		}
	}
}

func orPlaceholder(s string) string {
	if s == "" {
		return "app"
	}
	return s
}
