package config

import (
	"fmt"
	"sort"
	"strings"
)

// AppSpec is an extra process app in the stack. Omitted fields inherit from
// the first process (top-level image / deploy / env). Proxy does not inherit:
// a second process without a host is a worker, not a second Ingress on the
// same hostname.
type AppSpec struct {
	Image  string `yaml:"image"`
	Deploy Deploy `yaml:"deploy"`
	Proxy  Proxy  `yaml:"proxy"`
	Env    Env    `yaml:"env"`
}

// MemberKind is how a name participates in the stack.
type MemberKind int

const (
	// MemberNone is not in the stack.
	MemberNone MemberKind = iota
	// MemberPrimary is the first process: top-level image/deploy/proxy.
	// Object names use Config.App, which is also the stack / namespace seed.
	MemberPrimary
	// MemberProcess is an extra process app under Apps.
	MemberProcess
	// MemberStateful is a typed accessory (postgres, redis).
	MemberStateful
)

// Member reports how name is declared. The empty name and the stack name
// both mean the first process.
func (c *Config) Member(name string) MemberKind {
	if c == nil {
		return MemberNone
	}
	if name == "" || name == c.App {
		return MemberPrimary
	}
	if _, ok := c.Apps[name]; ok {
		return MemberProcess
	}
	if _, ok := c.Accessories[name]; ok {
		return MemberStateful
	}
	return MemberNone
}

// ProcessAppNames is the first process plus extra process apps, in a
// stable order. Stateful apps are not included.
func (c *Config) ProcessAppNames() []string {
	if c == nil {
		return nil
	}
	names := []string{c.App}
	extras := make([]string, 0, len(c.Apps))
	for n := range c.Apps {
		extras = append(extras, n)
	}
	sort.Strings(extras)
	return append(names, extras...)
}

// StackMembers is every app the CLI will name in errors: process apps
// then stateful apps.
func (c *Config) StackMembers() []string {
	if c == nil {
		return nil
	}
	names := c.ProcessAppNames()
	stateful := make([]string, 0, len(c.Accessories))
	for n := range c.Accessories {
		stateful = append(stateful, n)
	}
	sort.Strings(stateful)
	return append(names, stateful...)
}

// UnknownAppError lists the stack so a typo is fixable without opening YAML.
func (c *Config) UnknownAppError(name string) error {
	members := c.StackMembers()
	if len(members) == 0 {
		return fmt.Errorf("unknown app %q", name)
	}
	return fmt.Errorf("unknown app %q (this stack: %s)", name, strings.Join(members, ", "))
}

// ForProcessApp returns a config that deploys one process app.
//
// Accessories are cleared: a named or looped process deploy must not create
// or reconcile stateful apps. The stack deploy does that once, separately.
// Namespace stays the stack namespace so extra apps share one environment.
func (c *Config) ForProcessApp(name string) (*Config, error) {
	if c == nil {
		return nil, fmt.Errorf("no config")
	}
	switch c.Member(name) {
	case MemberPrimary:
		out := *c
		out.Accessories = nil
		out.Apps = nil
		return &out, nil
	case MemberProcess:
		return applyAppSpec(c, name, c.Apps[name]), nil
	case MemberStateful:
		return nil, fmt.Errorf("%q is a stateful app; deploy it with `buidl deploy %s`", name, name)
	default:
		return nil, c.UnknownAppError(name)
	}
}

// ForStatefulApp returns a config whose Accessories map contains only name,
// so PlanAccessories / ApplyAccessories act on that one app.
func (c *Config) ForStatefulApp(name string) (*Config, error) {
	if c == nil {
		return nil, fmt.Errorf("no config")
	}
	acc, ok := c.Accessories[name]
	if !ok {
		if c.Member(name) != MemberNone {
			return nil, fmt.Errorf("%q is not a stateful app", name)
		}
		return nil, c.UnknownAppError(name)
	}
	out := *c
	out.Apps = nil
	out.Accessories = map[string]Accessory{name: acc}
	return &out, nil
}

func applyAppSpec(base *Config, name string, spec AppSpec) *Config {
	out := *base
	out.App = name
	out.Accessories = nil
	out.Apps = nil
	out.Deploy.Kubernetes.Namespace = base.Deploy.Kubernetes.Namespace

	if spec.Image != "" {
		out.Image = spec.Image
	}
	overlayDeploy(&out.Deploy, spec.Deploy)

	// Extra processes do not inherit the first app's hostname.
	if spec.Proxy.Host != "" || len(spec.Proxy.Hosts) > 0 || spec.Proxy.Enabled != nil {
		out.Proxy = spec.Proxy
	} else {
		out.Proxy = Proxy{}
	}
	if out.Proxy.Enabled == nil {
		enabled := out.Proxy.Host != "" || len(out.Proxy.Hosts) > 0
		out.Proxy.Enabled = &enabled
	}
	if out.Proxy.SSL && out.Proxy.ClusterIssuer == "" {
		out.Proxy.ClusterIssuer = "letsencrypt-prod"
	}

	out.Env = mergeEnv(base.Env, spec.Env)
	return &out
}

func overlayDeploy(dst *Deploy, src Deploy) {
	if src.Target != "" {
		dst.Target = src.Target
	}
	if src.Port != 0 {
		dst.Port = src.Port
	}
	if len(src.Command) > 0 {
		dst.Command = src.Command
	}
	if src.Replicas != nil {
		dst.Replicas = src.Replicas
	}
	if src.Healthcheck.Path != "" {
		dst.Healthcheck.Path = src.Healthcheck.Path
	}
	if src.Healthcheck.Readiness != "" {
		dst.Healthcheck.Readiness = src.Healthcheck.Readiness
	}
	if src.Healthcheck.Liveness != "" {
		dst.Healthcheck.Liveness = src.Healthcheck.Liveness
	}
	if src.Healthcheck.Startup != "" {
		dst.Healthcheck.Startup = src.Healthcheck.Startup
	}
	if src.Strategy.Type != "" {
		dst.Strategy.Type = src.Strategy.Type
	}
	if src.Autoscale != nil {
		dst.Autoscale = src.Autoscale
	}
}

func mergeEnv(base, overlay Env) Env {
	out := Env{
		Dotenv:      base.Dotenv,
		DotenvFiles: base.DotenvFiles,
		SecretRefs:  append([]string{}, base.SecretRefs...),
	}
	if overlay.Dotenv {
		out.Dotenv = true
	}
	if len(overlay.DotenvFiles) > 0 {
		out.DotenvFiles = overlay.DotenvFiles
	}
	if len(overlay.SecretRefs) > 0 {
		out.SecretRefs = overlay.SecretRefs
	}
	if len(base.Clear) > 0 || len(overlay.Clear) > 0 {
		out.Clear = map[string]string{}
		for k, v := range base.Clear {
			out.Clear[k] = v
		}
		for k, v := range overlay.Clear {
			out.Clear[k] = v
		}
	}
	seen := map[string]bool{}
	for _, k := range base.Secret {
		if !seen[k] {
			out.Secret = append(out.Secret, k)
			seen[k] = true
		}
	}
	for _, k := range overlay.Secret {
		if !seen[k] {
			out.Secret = append(out.Secret, k)
			seen[k] = true
		}
	}
	return out
}
