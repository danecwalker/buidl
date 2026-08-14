package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// DefaultFilenames are searched in order when no explicit path is given.
var DefaultFilenames = []string{"buidl.yaml", "buidl.yml", ".buidl/config.yaml"}

// SchemaVersion is the schema version this build understands.
const SchemaVersion = 1

// LoadOptions controls resolution of a config file into a single environment.
type LoadOptions struct {
	// Path to the config file. If empty, DefaultFilenames are searched upward
	// from Dir.
	Path string
	// Dir is where the search for a config file starts. Defaults to ".".
	Dir string
	// Environment names the overlay to apply. When empty, defaultEnvironment is
	// used, then an environment named "staging" if one is declared. Production
	// is never implied.
	Environment string
	// Vars are injected into the interpolation context and take precedence over
	// the process environment. This is how BUIDL_SHA, BUIDL_BRANCH, BUIDL_SLUG
	// and friends reach the config.
	Vars map[string]string
	// Strict rejects unknown fields. On by default via Load; typos in a deploy
	// config are almost always bugs worth failing on.
	Strict bool
}

// Result is a resolved config plus the provenance needed for good error messages
// and for `buidl config show`.
type Result struct {
	Config *Config
	// Path is the absolute path of the file that was loaded.
	Path string
	// Root is the directory containing the config file. All relative paths in
	// the config (build context, hooks) resolve against it.
	Root string
	// Environments lists every environment declared in the file, sorted.
	Environments []string
}

// Load finds, parses, overlays, interpolates, defaults and validates a config.
func Load(opts LoadOptions) (*Result, error) {
	path, err := resolvePath(opts)
	if err != nil {
		return nil, err
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if doc == nil {
		return nil, fmt.Errorf("%s is empty", path)
	}

	envNames, overlay, chosen, err := selectEnvironment(doc, opts.Environment)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	// The overlay is merged over the base, then `environments` is dropped so the
	// strict decode below doesn't see a key the resolved struct ignores.
	delete(doc, "environments")
	delete(doc, "defaultEnvironment")
	merged := deepMerge(doc, overlay)

	vars := interpolationContext(opts.Vars, chosen)
	if err := interpolate(merged, vars, nil); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	out, err := yaml.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("re-encoding merged config: %w", err)
	}

	cfg := &Config{}
	dec := yaml.NewDecoder(bytes.NewReader(out))
	dec.KnownFields(opts.Strict)
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, decodeHint(err))
	}
	cfg.Environment = chosen

	root := filepath.Dir(path)
	applyDefaults(cfg)
	if err := Validate(cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	return &Result{Config: cfg, Path: path, Root: root, Environments: envNames}, nil
}

// resolvePath honors an explicit path or walks up from Dir looking for a config.
func resolvePath(opts LoadOptions) (string, error) {
	if opts.Path != "" {
		abs, err := filepath.Abs(opts.Path)
		if err != nil {
			return "", err
		}
		if _, err := os.Stat(abs); err != nil {
			return "", fmt.Errorf("config file %s: %w", opts.Path, err)
		}
		return abs, nil
	}

	dir := opts.Dir
	if dir == "" {
		dir = "."
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}

	// Walk up so `buidl deploy` works from any subdirectory of the repo, the
	// same way git and npm behave.
	for {
		for _, name := range DefaultFilenames {
			candidate := filepath.Join(abs, name)
			if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
				return candidate, nil
			}
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return "", fmt.Errorf("no buidl.yaml found in %s or any parent directory (run `buidl init`)", dir)
		}
		abs = parent
	}
}

// selectEnvironment returns the declared environment names, the chosen overlay,
// and the chosen name.
func selectEnvironment(doc map[string]any, requested string) (names []string, overlay map[string]any, chosen string, err error) {
	envsRaw, hasEnvs := doc["environments"]
	envs := map[string]any{}
	if hasEnvs && envsRaw != nil {
		m, ok := envsRaw.(map[string]any)
		if !ok {
			return nil, nil, "", errors.New("`environments` must be a mapping of name to overlay")
		}
		envs = m
	}
	for name := range envs {
		names = append(names, name)
	}
	sort.Strings(names)

	if requested == "" {
		if def, ok := doc["defaultEnvironment"].(string); ok && def != "" {
			requested = def
		} else if implied := implicitEnvironment(names); implied != "" {
			// Staging is the happy-path environment. Production is never implied:
			// destroying or promoting the wrong one is worse than asking.
			requested = implied
		}
	}

	// A config with no environments at all is valid: a single-target app.
	if requested == "" {
		if len(names) == 0 {
			return names, map[string]any{}, "default", nil
		}
		return nil, nil, "", fmt.Errorf("this config declares environments (%s); pass -e/--env to pick one, or set defaultEnvironment", strings.Join(names, ", "))
	}

	raw, ok := envs[requested]
	if !ok {
		if len(names) == 0 {
			return nil, nil, "", fmt.Errorf("environment %q requested but no `environments` are declared", requested)
		}
		return nil, nil, "", fmt.Errorf("unknown environment %q (declared: %s)", requested, strings.Join(names, ", "))
	}
	if raw == nil {
		// `staging:` with an empty body means "inherit the base as-is".
		return names, map[string]any{}, requested, nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, nil, "", fmt.Errorf("environment %q must be a mapping", requested)
	}
	return names, m, requested, nil
}

// implicitEnvironment returns "staging" when that name is declared, using the
// spelling from the file. Production-like names are never implied.
func implicitEnvironment(names []string) string {
	for _, n := range names {
		if strings.EqualFold(n, "staging") {
			return n
		}
	}
	return ""
}

// deepMerge overlays src onto dst, recursing into nested mappings. Scalars and
// sequences are replaced wholesale rather than appended: a staging environment
// that sets `platforms: [linux/arm64]` means exactly that, not "also arm64".
func deepMerge(dst, src map[string]any) map[string]any {
	out := make(map[string]any, len(dst)+len(src))
	for k, v := range dst {
		out[k] = v
	}
	for k, sv := range src {
		if dv, ok := out[k]; ok {
			dm, dok := dv.(map[string]any)
			sm, sok := sv.(map[string]any)
			if dok && sok {
				out[k] = deepMerge(dm, sm)
				continue
			}
		}
		out[k] = sv
	}
	return out
}

// varPattern matches ${NAME}, ${NAME:-default} and ${NAME:?message}.
var varPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::([-?])([^}]*))?\}`)

// interpolationContext layers explicit vars over the process environment.
func interpolationContext(vars map[string]string, env string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		if vars != nil {
			if v, ok := vars[name]; ok {
				return v, true
			}
		}
		if name == "BUIDL_ENV" {
			return env, true
		}
		return os.LookupEnv(name)
	}
}

// interpolate walks the merged document replacing ${VAR} references in every
// string value. Keys are left alone.
//
// Doing this on the parsed document rather than on the raw bytes means a value
// containing YAML metacharacters can never corrupt the document structure.
func interpolate(node any, lookup func(string) (string, bool), path []string) error {
	switch n := node.(type) {
	case map[string]any:
		for k, v := range n {
			switch tv := v.(type) {
			case string:
				replaced, err := expand(tv, lookup)
				if err != nil {
					return fmt.Errorf("%s: %w", strings.Join(append(path, k), "."), err)
				}
				n[k] = replaced
			default:
				if err := interpolate(v, lookup, append(path, k)); err != nil {
					return err
				}
			}
		}
	case []any:
		for i, v := range n {
			switch tv := v.(type) {
			case string:
				replaced, err := expand(tv, lookup)
				if err != nil {
					return fmt.Errorf("%s[%d]: %w", strings.Join(path, "."), i, err)
				}
				n[i] = replaced
			default:
				if err := interpolate(v, lookup, path); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// expand substitutes variable references in a single string.
func expand(s string, lookup func(string) (string, bool)) (string, error) {
	if !strings.Contains(s, "${") {
		return s, nil
	}
	var firstErr error
	out := varPattern.ReplaceAllStringFunc(s, func(match string) string {
		groups := varPattern.FindStringSubmatch(match)
		name, op, arg := groups[1], groups[2], groups[3]
		val, found := lookup(name)
		switch {
		case found && val != "":
			return val
		case op == "-":
			// ${VAR:-default}
			return arg
		case op == "?":
			// ${VAR:?why it's needed} — fail loudly rather than deploy a
			// half-configured app.
			if firstErr == nil {
				msg := arg
				if msg == "" {
					msg = "must be set"
				}
				firstErr = fmt.Errorf("required variable %s is not set: %s", name, msg)
			}
			return ""
		case found:
			// Set but empty, with no default requested.
			return ""
		default:
			if firstErr == nil {
				firstErr = fmt.Errorf("variable %s is not set (use ${%s:-default} to make it optional)", name, name)
			}
			return ""
		}
	})
	if firstErr != nil {
		return "", firstErr
	}
	return out, nil
}

// decodeHint makes yaml's strict-mode errors point at the fix.
func decodeHint(err error) error {
	msg := err.Error()
	if strings.Contains(msg, "not found in type") {
		return fmt.Errorf("%w\n\nhint: unknown field — check for a typo, or run `buidl config schema` to list valid keys", err)
	}
	return err
}
