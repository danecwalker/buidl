// Package secrets resolves the values for names declared in env.secret.
//
// buidl never stores secret values. buidl.yaml lists only NAMES; values are
// resolved at deploy time and written straight into a Kubernetes Secret. That
// keeps credentials out of version control by construction rather than by
// convention.
//
// The file layout follows Kamal's, because the split it encodes is genuinely
// useful: one committed file that declares *which* secrets exist and where they
// come from, and per-environment files that are gitignored because people
// inevitably paste literals into them.
//
//	.buidl/secrets-common        committed; indirections only, no literal values
//	.buidl/secrets               gitignored; applies to every environment
//	.buidl/secrets.<environment> gitignored; overrides for one environment
//
// Later sources win, and the process environment wins over all of them so that CI
// can inject values without any file being present.
package secrets

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Directory is where buidl keeps its local state, relative to the project root.
const Directory = ".buidl"

// File names within Directory.
const (
	// CommonFile is safe to commit: it should contain only indirections such as
	// DATABASE_URL=$PROD_DATABASE_URL.
	CommonFile = "secrets-common"
	// SharedFile applies to every environment and is gitignored.
	SharedFile = "secrets"
)

// DefaultFile is the gitignored shared secrets file, relative to the root.
var DefaultFile = filepath.Join(Directory, SharedFile)

// EnvironmentFile returns the per-environment secrets path.
//
// With no environment selected — or the synthetic "default" one a config without
// environments resolves to — there is no distinct per-environment file, so this
// returns the shared path rather than inventing `.buidl/secrets.default`.
func EnvironmentFile(environment string) string {
	if environment == "" || environment == "default" {
		return DefaultFile
	}
	return filepath.Join(Directory, SharedFile+"."+environment)
}

// CommonPath returns the committed common secrets path.
func CommonPath() string {
	return filepath.Join(Directory, CommonFile)
}

// Source records where a value came from, for display without leaking the value.
type Source string

const (
	SourceEnv         Source = "environment"
	SourceDotenv      Source = ".env"
	SourceCommon      Source = "secrets-common"
	SourceShared      Source = "secrets"
	SourceEnvironment Source = "secrets.<env>"
	// SourceDerived is a value synthesized from a typed accessory (for example
	// DATABASE_URL from POSTGRES_PASSWORD). It is not stored; it is computed
	// at resolve time when the name is declared and no other source set it.
	SourceDerived Source = "derived"
)

// Options configures resolution.
type Options struct {
	// Root is the project directory.
	Root string
	// Environment selects the per-environment files.
	Environment string
	// Names are the secret names declared in buidl.yaml.
	Names []string
	// Dotenv enables reading .env and .env.<environment>, so a project that
	// already keeps its configuration there need not restate every name.
	Dotenv bool
	// DotenvFiles overrides the discovered dotenv files with an explicit list,
	// relative to Root.
	DotenvFiles []string
}

// Resolution is the outcome of resolving one environment's secrets.
type Resolution struct {
	// Values maps name to value. Never logged.
	Values map[string]string
	// Sources maps name to where it was found, safe to display.
	Sources map[string]Source
	// Missing lists declared names with no value anywhere.
	Missing []string
	// Files lists the files that were read, for display.
	Files []string
	// Discovered lists names found in dotenv files that buidl.yaml did not
	// declare. They are not deployed; reporting them prevents the silent
	// surprise of a variable that exists locally but not in the cluster.
	Discovered []string
	// Warnings are non-fatal concerns, such as an ignored .env.local.
	Warnings []string
}

// layer is one source of values, in increasing precedence order.
type layer struct {
	source Source
	values map[string]string
	path   string
}

// Resolve looks up each declared name across every layer.
//
// Precedence, lowest to highest:
//
//	.env, .env.<environment>          if Dotenv is enabled
//	.buidl/secrets-common             committed, indirections only
//	.buidl/secrets                    gitignored, all environments
//	.buidl/secrets.<environment>      gitignored, one environment
//	the process environment           always wins
//
// buidl's own files outrank generic dotenv files because they are written
// deliberately for deploys, and the process environment outranks everything so CI
// injection is never overridden by a stale file on a developer's machine.
func Resolve(opts Options) (*Resolution, error) {
	res := &Resolution{
		Values:  map[string]string{},
		Sources: map[string]Source{},
	}

	layers, err := loadLayers(opts, res)
	if err != nil {
		return nil, err
	}

	if len(opts.Names) == 0 {
		// Still report what was found, so `variable list` can show a project its
		// undeclared variables even with nothing declared yet.
		res.Discovered = undeclared(layers, nil)
		return res, nil
	}

	declared := map[string]bool{}
	for _, name := range opts.Names {
		declared[name] = true
	}

	for _, name := range opts.Names {
		if v, ok := os.LookupEnv(name); ok && v != "" {
			res.Values[name] = v
			res.Sources[name] = SourceEnv
			continue
		}
		// Highest-precedence file that defines it.
		found := false
		for i := len(layers) - 1; i >= 0; i-- {
			if v, ok := layers[i].values[name]; ok && v != "" {
				res.Values[name] = v
				res.Sources[name] = layers[i].source
				found = true
				break
			}
		}
		if !found {
			res.Missing = append(res.Missing, name)
		}
	}

	res.Discovered = undeclared(layers, declared)
	sort.Strings(res.Missing)
	return res, nil
}

// loadLayers reads every configured source in precedence order.
func loadLayers(opts Options, res *Resolution) ([]layer, error) {
	type spec struct {
		source Source
		path   string
	}

	var specs []spec

	if opts.Dotenv {
		for _, path := range dotenvFiles(opts) {
			specs = append(specs, spec{SourceDotenv, path})
		}
		// A .env.local present but deliberately skipped is worth saying out loud:
		// its absence from the deploy is a decision, not an oversight.
		for _, ignored := range localDotenvFiles(opts) {
			if _, err := os.Stat(filepath.Join(opts.Root, ignored)); err == nil {
				res.Warnings = append(res.Warnings, fmt.Sprintf(
					"ignoring %s: .local files are machine-local dev config by convention and are never deployed", ignored))
			}
		}
	}

	specs = append(specs,
		spec{SourceCommon, CommonPath()},
		spec{SourceShared, DefaultFile},
		spec{SourceEnvironment, EnvironmentFile(opts.Environment)},
	)

	var layers []layer
	// With no environment selected, EnvironmentFile resolves to the shared path,
	// so the same file would otherwise be read — and reported — twice.
	seen := map[string]bool{}

	for _, s := range specs {
		if seen[s.path] {
			continue
		}
		seen[s.path] = true

		values, found, err := loadFile(filepath.Join(opts.Root, s.path))
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		res.Files = append(res.Files, s.path)
		layers = append(layers, layer{source: s.source, values: values, path: s.path})
	}
	return layers, nil
}

// dotenvFiles returns the dotenv files to read, lowest precedence first.
func dotenvFiles(opts Options) []string {
	if len(opts.DotenvFiles) > 0 {
		return opts.DotenvFiles
	}
	files := []string{".env"}
	if opts.Environment != "" && opts.Environment != "default" {
		files = append(files, ".env."+opts.Environment)
	}
	return files
}

// localDotenvFiles returns the dotenv files deliberately excluded from deploys.
func localDotenvFiles(opts Options) []string {
	files := []string{".env.local"}
	if opts.Environment != "" && opts.Environment != "default" {
		files = append(files, ".env."+opts.Environment+".local")
	}
	return files
}

// undeclared lists names present in files but not declared in buidl.yaml.
func undeclared(layers []layer, declared map[string]bool) []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range layers {
		// Only dotenv layers are reported: a name in .buidl/secrets that is not
		// declared is a leftover, whereas a name in .env is normal application
		// config that simply is not being deployed.
		if l.source != SourceDotenv {
			continue
		}
		for name := range l.values {
			if declared[name] || seen[name] {
				continue
			}
			seen[name] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// loadFile parses a dotenv-style file, reporting whether it existed.
//
// A missing file is not an error: in CI the environment supplies everything.
func loadFile(path string) (map[string]string, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("reading %s: %w", path, err)
	}
	defer f.Close()

	out := map[string]string{}
	scanner := bufio.NewScanner(f)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Support an optional `export ` prefix so the file can also be sourced by
		// a shell.
		line = strings.TrimPrefix(line, "export ")

		name, value, found := strings.Cut(line, "=")
		if !found {
			return nil, true, fmt.Errorf("%s:%d: expected NAME=value", path, lineNo)
		}
		name = strings.TrimSpace(name)
		value = strings.TrimSpace(value)

		// Strip matching quotes, needed for values containing spaces.
		if len(value) >= 2 {
			if (value[0] == '"' && value[len(value)-1] == '"') ||
				(value[0] == '\'' && value[len(value)-1] == '\'') {
				value = value[1 : len(value)-1]
			}
		}

		// A value of the form $OTHER indirects through the environment. This is what
		// makes secrets-common safe to commit: it declares the binding, not the
		// secret.
		if strings.HasPrefix(value, "$") && !strings.HasPrefix(value, "$$") {
			if v, ok := os.LookupEnv(strings.TrimPrefix(value, "$")); ok {
				value = v
			} else {
				// An unresolved indirection is not a value; leaving it as the literal
				// "$FOO" would deploy a nonsense credential.
				continue
			}
		}

		out[name] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, true, fmt.Errorf("reading %s: %w", path, err)
	}
	return out, true, nil
}

// PermissionWarnings returns warnings for secrets files readable by other users.
//
// secrets-common is excluded: it is meant to be committed and contains no values,
// so world-readable is correct for it.
func PermissionWarnings(root, environment string) []string {
	var out []string
	for _, rel := range []string{DefaultFile, EnvironmentFile(environment)} {
		path := filepath.Join(root, rel)
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		if mode := info.Mode().Perm(); mode&0o077 != 0 {
			out = append(out, fmt.Sprintf("%s is readable by other users (mode %04o); run `chmod 600 %s`", rel, mode, path))
		}
	}
	return out
}
