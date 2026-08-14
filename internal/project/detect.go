// Package project infers how to build and run an application from the files in
// its directory.
//
// This is the zero-config path: `buidl init` uses detection to write a working
// buidl.yaml and, when the project has no Dockerfile, a reasonable multi-stage
// one. Detection is a starting point that users then edit — it is never applied
// implicitly at deploy time, because a deploy tool guessing differently between
// runs is far worse than one that makes you commit your choice.
package project

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Kind identifies a detected stack.
type Kind string

const (
	KindDockerfile Kind = "dockerfile"
	KindGo         Kind = "go"
	KindNode       Kind = "node"
	KindPython     Kind = "python"
	KindRuby       Kind = "ruby"
	KindRust       Kind = "rust"
	KindStatic     Kind = "static"
	KindUnknown    Kind = "unknown"
)

// PackageManager identifies a Node or Python dependency tool, which determines
// both the install command and which lockfile to copy for cache efficiency.
type PackageManager string

const (
	PMNpm     PackageManager = "npm"
	PMPnpm    PackageManager = "pnpm"
	PMYarn    PackageManager = "yarn"
	PMBun     PackageManager = "bun"
	PMPip     PackageManager = "pip"
	PMPoetry  PackageManager = "poetry"
	PMUv      PackageManager = "uv"
	PMBundler PackageManager = "bundler"
)

// Detection is what we learned about a project.
type Detection struct {
	Kind Kind
	// Name is a suggested app name, derived from package metadata or the
	// directory name.
	Name string
	// PackageManager is set for Node/Python/Ruby projects.
	PackageManager PackageManager
	// Runtime is the detected language version, e.g. "22" for Node or "1.25" for
	// Go, used to pin the base image.
	Runtime string
	// BuildCommand and StartCommand are the inferred lifecycle commands.
	BuildCommand string
	StartCommand string
	// Port is the port the app most likely listens on.
	Port int32
	// Framework is a recognized framework name ("next", "rails", "django", ...),
	// which can change the run command and the health endpoint.
	Framework string
	// HealthPath is a health endpoint the framework is known to expose.
	HealthPath string
	// Notes explain inferences to the user so they can correct them.
	Notes []string
	// HasDockerfile reports an existing Dockerfile, which always wins.
	HasDockerfile bool
	// DockerfilePath is relative to the project root.
	DockerfilePath string

	// Stack is the detected language stack even when an existing Dockerfile has
	// claimed Kind. Dockerfile generation and run-command inference key off this.
	Stack Kind
	// MainPackage is the Go package to build, e.g. "./cmd/server".
	MainPackage string
	// BinaryName is the compiled artifact name for Go and Rust builds.
	BinaryName string
}

// Detect inspects dir and returns its best guess.
func Detect(dir string) (Detection, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Detection{}, err
	}

	d := Detection{
		Kind: KindUnknown,
		Name: suggestName(abs),
		Port: 8080,
	}

	// An existing Dockerfile is authoritative: the user has already expressed
	// exactly how they want the image built.
	for _, name := range []string{"Dockerfile", "dockerfile", "docker/Dockerfile"} {
		if exists(filepath.Join(abs, name)) {
			d.HasDockerfile = true
			d.DockerfilePath = name
			d.Kind = KindDockerfile
			d.Notes = append(d.Notes, "found "+name+"; buidl will build it as-is")
			// Keep detecting so we can still suggest a port and health path.
			break
		}
	}

	switch {
	case exists(filepath.Join(abs, "go.mod")):
		detectGo(abs, &d)
	case exists(filepath.Join(abs, "package.json")):
		detectNode(abs, &d)
	case exists(filepath.Join(abs, "Gemfile")):
		detectRuby(abs, &d)
	case exists(filepath.Join(abs, "pyproject.toml")), exists(filepath.Join(abs, "requirements.txt")):
		detectPython(abs, &d)
	case exists(filepath.Join(abs, "Cargo.toml")):
		detectRust(abs, &d)
	case exists(filepath.Join(abs, "index.html")):
		d.setKind(KindStatic)
		d.Port = 80
		d.Notes = append(d.Notes, "static site; nginx will serve /livez, /readyz, and /startupz")
	}

	if d.HealthPath == "" {
		d.Notes = append(d.Notes, "serve /livez, /readyz, and /startupz — or set deploy.healthcheck.path")
	}

	return d, nil
}

// setKind records the stack unless an explicit Dockerfile already claimed it.
func (d *Detection) setKind(k Kind) {
	d.Stack = k
	if d.Kind != KindDockerfile {
		d.Kind = k
	}
}

func detectGo(dir string, d *Detection) {
	d.setKind(KindGo)
	d.Port = 8080

	if data, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if rest, ok := strings.CutPrefix(line, "module "); ok && d.Name == suggestName(dir) {
				parts := strings.Split(strings.TrimSpace(rest), "/")
				if last := parts[len(parts)-1]; last != "" {
					d.Name = sanitizeName(last)
				}
			}
			// "go 1.25.5" pins the toolchain; use major.minor for the base image.
			if rest, ok := strings.CutPrefix(line, "go "); ok {
				d.Runtime = majorMinor(strings.TrimSpace(rest))
			}
		}
	}

	// A main package under ./cmd/<name> is the dominant Go layout.
	d.MainPackage = "."
	if entries, err := os.ReadDir(filepath.Join(dir, "cmd")); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				d.MainPackage = "./cmd/" + e.Name()
				d.Notes = append(d.Notes, "building ./cmd/"+e.Name()+" as the main package")
				break
			}
		}
	}
	d.BuildCommand = "go build -o /out/app " + d.MainPackage
	d.StartCommand = "/app"
}

// nodeManifest is the subset of package.json we care about.
type nodeManifest struct {
	Name         string            `json:"name"`
	Scripts      map[string]string `json:"scripts"`
	Dependencies map[string]string `json:"dependencies"`
	DevDeps      map[string]string `json:"devDependencies"`
	Engines      struct {
		Node string `json:"node"`
	} `json:"engines"`
	PackageManager string `json:"packageManager"`
}

func detectNode(dir string, d *Detection) {
	d.setKind(KindNode)
	d.Port = 3000

	var m nodeManifest
	if data, err := os.ReadFile(filepath.Join(dir, "package.json")); err == nil {
		_ = json.Unmarshal(data, &m)
	}
	if m.Name != "" {
		d.Name = sanitizeName(m.Name)
	}
	if m.Engines.Node != "" {
		d.Runtime = majorMinor(strings.TrimLeft(m.Engines.Node, "^~>=v "))
	}

	// Lockfiles are the most reliable signal, and the "packageManager" field
	// (corepack) is authoritative when present.
	switch {
	case strings.HasPrefix(m.PackageManager, "pnpm"):
		d.PackageManager = PMPnpm
	case strings.HasPrefix(m.PackageManager, "yarn"):
		d.PackageManager = PMYarn
	case exists(filepath.Join(dir, "bun.lockb")), exists(filepath.Join(dir, "bun.lock")):
		d.PackageManager = PMBun
	case exists(filepath.Join(dir, "pnpm-lock.yaml")):
		d.PackageManager = PMPnpm
	case exists(filepath.Join(dir, "yarn.lock")):
		d.PackageManager = PMYarn
	default:
		d.PackageManager = PMNpm
	}

	deps := map[string]string{}
	for k, v := range m.Dependencies {
		deps[k] = v
	}
	for k, v := range m.DevDeps {
		deps[k] = v
	}

	switch {
	case has(deps, "next"):
		d.Framework = "next"
		d.Port = 3000
		d.Notes = append(d.Notes, "Next.js detected; set output:'standalone' in next.config for the smallest image")
	case has(deps, "nuxt"):
		d.Framework = "nuxt"
	case has(deps, "@nestjs/core"):
		d.Framework = "nest"
	case has(deps, "vite") && !has(deps, "express"):
		d.Framework = "vite"
		d.Port = 80
		d.Notes = append(d.Notes, "Vite SPA detected; built assets will be served by nginx")
	case has(deps, "express"), has(deps, "fastify"), has(deps, "koa"), has(deps, "hono"):
		d.Framework = "node-server"
	}

	if _, ok := m.Scripts["build"]; ok {
		d.BuildCommand = string(d.PackageManager) + " run build"
	}
	if _, ok := m.Scripts["start"]; ok {
		d.StartCommand = string(d.PackageManager) + " run start"
	} else {
		d.StartCommand = "node index.js"
		d.Notes = append(d.Notes, "no start script in package.json; assuming `node index.js`")
	}
}

func detectRuby(dir string, d *Detection) {
	d.setKind(KindRuby)
	d.PackageManager = PMBundler
	d.Port = 3000

	if data, err := os.ReadFile(filepath.Join(dir, ".ruby-version")); err == nil {
		d.Runtime = majorMinor(strings.TrimSpace(string(data)))
	}
	if data, err := os.ReadFile(filepath.Join(dir, "Gemfile")); err == nil {
		content := string(data)
		if strings.Contains(content, "rails") {
			d.Framework = "rails"
			// Rails 7.1+ ships this endpoint, which is also Kamal's default.
			d.HealthPath = "/up"
			d.StartCommand = "./bin/rails server -b 0.0.0.0"
			d.Notes = append(d.Notes, "Rails detected; ensure RAILS_MASTER_KEY is in env.secret")
		}
		if d.Runtime == "" {
			if i := strings.Index(content, "ruby \""); i >= 0 {
				rest := content[i+6:]
				if j := strings.Index(rest, "\""); j > 0 {
					d.Runtime = majorMinor(rest[:j])
				}
			}
		}
	}
	if d.StartCommand == "" {
		d.StartCommand = "bundle exec rackup -o 0.0.0.0 -p 3000"
	}
}

func detectPython(dir string, d *Detection) {
	d.setKind(KindPython)
	d.Port = 8000

	switch {
	case exists(filepath.Join(dir, "uv.lock")):
		d.PackageManager = PMUv
	case exists(filepath.Join(dir, "poetry.lock")):
		d.PackageManager = PMPoetry
	default:
		d.PackageManager = PMPip
	}

	if data, err := os.ReadFile(filepath.Join(dir, "pyproject.toml")); err == nil {
		content := string(data)
		if strings.Contains(content, "django") || exists(filepath.Join(dir, "manage.py")) {
			d.Framework = "django"
		} else if strings.Contains(content, "fastapi") {
			d.Framework = "fastapi"
		} else if strings.Contains(content, "flask") {
			d.Framework = "flask"
		}
		if name := tomlValue(content, "name"); name != "" && d.Name == suggestName(dir) {
			d.Name = sanitizeName(name)
		}
		if rp := tomlValue(content, "requires-python"); rp != "" {
			d.Runtime = majorMinor(strings.TrimLeft(rp, "^~>=< "))
		}
	}
	if exists(filepath.Join(dir, "manage.py")) && d.Framework == "" {
		d.Framework = "django"
	}

	switch d.Framework {
	case "django":
		d.StartCommand = "gunicorn --bind 0.0.0.0:8000 config.wsgi:application"
		d.Notes = append(d.Notes, "Django detected; adjust the gunicorn WSGI path to your project module")
	case "fastapi":
		d.StartCommand = "uvicorn main:app --host 0.0.0.0 --port 8000"
	case "flask":
		d.StartCommand = "gunicorn --bind 0.0.0.0:8000 app:app"
	default:
		d.StartCommand = "python main.py"
		d.Notes = append(d.Notes, "no web framework detected; assuming `python main.py`")
	}
}

func detectRust(dir string, d *Detection) {
	d.setKind(KindRust)
	d.Port = 8080

	if data, err := os.ReadFile(filepath.Join(dir, "Cargo.toml")); err == nil {
		if name := tomlValue(string(data), "name"); name != "" {
			d.Name = sanitizeName(name)
			d.BinaryName = name
		}
	}
	if d.BinaryName == "" {
		d.BinaryName = d.Name
	}
	d.BuildCommand = "cargo build --release --locked"
	d.StartCommand = "/app"
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func has(m map[string]string, key string) bool {
	_, ok := m[key]
	return ok
}

// suggestName derives an app name from the directory.
func suggestName(dir string) string {
	return sanitizeName(filepath.Base(dir))
}

// sanitizeName coerces a string into a valid DNS label for use as an app name.
func sanitizeName(s string) string {
	s = strings.ToLower(s)
	// Scoped npm names like "@acme/web" reduce to their last segment.
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	for strings.Contains(out, "--") {
		out = strings.ReplaceAll(out, "--", "-")
	}
	if len(out) > 63 {
		out = strings.Trim(out[:63], "-")
	}
	if out == "" {
		return "app"
	}
	return out
}

// majorMinor reduces a version string to "major.minor", which is the right
// granularity for pinning a base image: specific enough to be reproducible,
// loose enough to receive patch-level security updates.
func majorMinor(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	// Strip range syntax and prerelease suffixes.
	for _, cut := range []string{" ", ",", "-", "+"} {
		if i := strings.Index(v, cut); i > 0 {
			v = v[:i]
		}
	}
	parts := strings.Split(v, ".")
	if len(parts) >= 2 {
		return parts[0] + "." + parts[1]
	}
	return v
}

// tomlValue extracts a simple top-level `key = "value"` from TOML without
// pulling in a parser. Detection output is advisory and user-editable, so a
// heuristic read is an acceptable trade for zero dependencies.
func tomlValue(content, key string) string {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		rest, ok := strings.CutPrefix(line, key)
		if !ok {
			continue
		}
		rest = strings.TrimSpace(rest)
		rest, ok = strings.CutPrefix(rest, "=")
		if !ok {
			continue
		}
		rest = strings.TrimSpace(rest)
		if len(rest) >= 2 && rest[0] == '"' {
			if j := strings.Index(rest[1:], "\""); j >= 0 {
				return rest[1 : 1+j]
			}
		}
	}
	return ""
}
