package project

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// scaffold creates a temp project from a map of relative path to contents.
func scaffold(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestDetectGo(t *testing.T) {
	dir := scaffold(t, map[string]string{
		"go.mod":             "module github.com/acme/storefront\n\ngo 1.25.5\n",
		"cmd/server/main.go": "package main\n",
	})

	d, err := Detect(dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}

	if d.Kind != KindGo {
		t.Errorf("Kind = %q, want go", d.Kind)
	}
	// The name should come from the module path, not the temp directory.
	if d.Name != "storefront" {
		t.Errorf("Name = %q, want storefront", d.Name)
	}
	// Patch versions must be dropped so the base image still gets security updates.
	if d.Runtime != "1.25" {
		t.Errorf("Runtime = %q, want 1.25", d.Runtime)
	}
	if d.MainPackage != "./cmd/server" {
		t.Errorf("MainPackage = %q, want ./cmd/server", d.MainPackage)
	}
}

func TestDetectNodeWithPnpmAndNext(t *testing.T) {
	dir := scaffold(t, map[string]string{
		"package.json": `{
			"name": "@acme/web",
			"engines": {"node": ">=22.1.0"},
			"scripts": {"build": "next build", "start": "next start"},
			"dependencies": {"next": "^15.0.0"}
		}`,
		"pnpm-lock.yaml": "lockfileVersion: '9.0'\n",
	})

	d, err := Detect(dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}

	if d.Kind != KindNode {
		t.Errorf("Kind = %q, want node", d.Kind)
	}
	// A scoped package name must reduce to a valid DNS label.
	if d.Name != "web" {
		t.Errorf("Name = %q, want web", d.Name)
	}
	if d.PackageManager != PMPnpm {
		t.Errorf("PackageManager = %q, want pnpm", d.PackageManager)
	}
	if d.Framework != "next" {
		t.Errorf("Framework = %q, want next", d.Framework)
	}
	if d.Runtime != "22.1" {
		t.Errorf("Runtime = %q, want 22.1", d.Runtime)
	}
	if d.Port != 3000 {
		t.Errorf("Port = %d, want 3000", d.Port)
	}
}

func TestPackageManagerFieldWinsOverLockfile(t *testing.T) {
	// corepack's packageManager field is authoritative.
	dir := scaffold(t, map[string]string{
		"package.json":      `{"name": "web", "packageManager": "yarn@4.1.0"}`,
		"package-lock.json": "{}",
	})

	d, _ := Detect(dir)
	if d.PackageManager != PMYarn {
		t.Errorf("PackageManager = %q, want yarn", d.PackageManager)
	}
}

func TestDetectNodeDefaultsToNpm(t *testing.T) {
	dir := scaffold(t, map[string]string{
		"package.json": `{"name": "web", "scripts": {"start": "node index.js"}}`,
	})
	d, _ := Detect(dir)
	if d.PackageManager != PMNpm {
		t.Errorf("PackageManager = %q, want npm", d.PackageManager)
	}
}

func TestDetectRails(t *testing.T) {
	dir := scaffold(t, map[string]string{
		"Gemfile":       "source 'https://rubygems.org'\nruby \"3.4.1\"\ngem 'rails', '~> 8.0'\n",
		".ruby-version": "3.4.1\n",
	})

	d, err := Detect(dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}

	if d.Kind != KindRuby {
		t.Errorf("Kind = %q, want ruby", d.Kind)
	}
	if d.Framework != "rails" {
		t.Errorf("Framework = %q, want rails", d.Framework)
	}
	// Rails 7.1+ ships /up, which is also Kamal's default health endpoint.
	if d.HealthPath != "/up" {
		t.Errorf("HealthPath = %q, want /up", d.HealthPath)
	}
	if d.Runtime != "3.4" {
		t.Errorf("Runtime = %q, want 3.4", d.Runtime)
	}
}

func TestDetectPythonUv(t *testing.T) {
	dir := scaffold(t, map[string]string{
		"pyproject.toml": "[project]\nname = \"api\"\nrequires-python = \">=3.13\"\ndependencies = [\"fastapi\"]\n",
		"uv.lock":        "version = 1\n",
	})

	d, err := Detect(dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}

	if d.Kind != KindPython {
		t.Errorf("Kind = %q, want python", d.Kind)
	}
	if d.PackageManager != PMUv {
		t.Errorf("PackageManager = %q, want uv", d.PackageManager)
	}
	if d.Framework != "fastapi" {
		t.Errorf("Framework = %q, want fastapi", d.Framework)
	}
	if d.Name != "api" {
		t.Errorf("Name = %q, want api", d.Name)
	}
	if d.Runtime != "3.13" {
		t.Errorf("Runtime = %q, want 3.13", d.Runtime)
	}
}

func TestDetectDjangoViaManagePy(t *testing.T) {
	dir := scaffold(t, map[string]string{
		"requirements.txt": "django\ngunicorn\n",
		"manage.py":        "#!/usr/bin/env python\n",
	})
	d, _ := Detect(dir)
	if d.Framework != "django" {
		t.Errorf("Framework = %q, want django", d.Framework)
	}
	if d.PackageManager != PMPip {
		t.Errorf("PackageManager = %q, want pip", d.PackageManager)
	}
}

func TestDetectRust(t *testing.T) {
	dir := scaffold(t, map[string]string{
		"Cargo.toml": "[package]\nname = \"edge-router\"\nversion = \"0.1.0\"\n",
	})
	d, _ := Detect(dir)
	if d.Kind != KindRust {
		t.Errorf("Kind = %q, want rust", d.Kind)
	}
	if d.BinaryName != "edge-router" {
		t.Errorf("BinaryName = %q, want edge-router", d.BinaryName)
	}
}

func TestDetectStatic(t *testing.T) {
	dir := scaffold(t, map[string]string{"index.html": "<html></html>"})
	d, _ := Detect(dir)
	if d.Kind != KindStatic {
		t.Errorf("Kind = %q, want static", d.Kind)
	}
	if d.Port != 80 {
		t.Errorf("Port = %d, want 80", d.Port)
	}
}

func TestExistingDockerfileWins(t *testing.T) {
	dir := scaffold(t, map[string]string{
		"Dockerfile": "FROM scratch\n",
		"go.mod":     "module github.com/acme/api\n\ngo 1.25\n",
	})

	d, err := Detect(dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}

	// The user has already said how to build; do not override that.
	if d.Kind != KindDockerfile {
		t.Errorf("Kind = %q, want dockerfile", d.Kind)
	}
	if !d.HasDockerfile {
		t.Error("HasDockerfile should be true")
	}
	// The underlying stack is still recorded, so port and health guesses work.
	if d.Stack != KindGo {
		t.Errorf("Stack = %q, want go", d.Stack)
	}
}

func TestDetectUnknownStillUsable(t *testing.T) {
	dir := scaffold(t, map[string]string{"README.md": "# hi"})
	d, err := Detect(dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if d.Kind != KindUnknown {
		t.Errorf("Kind = %q, want unknown", d.Kind)
	}
	// Even with nothing recognized, the result must be a valid starting point.
	// HealthPath stays empty so the platform default (/livez, /readyz, /startupz)
	// applies unless a framework is known to expose something else.
	if d.Name == "" || d.Port == 0 {
		t.Errorf("unknown detection should still yield defaults: %+v", d)
	}
	if d.HealthPath != "" {
		t.Errorf("HealthPath = %q, want empty for unknown stacks", d.HealthPath)
	}
}

func TestSanitizeName(t *testing.T) {
	tests := []struct{ in, want string }{
		{"@acme/My_App", "my-app"},
		{"My App!", "my-app"},
		{"---weird---", "weird"},
		{"UPPER", "upper"},
		{"", "app"},
		{"!!!", "app"},
		{strings.Repeat("a", 80), strings.Repeat("a", 63)},
	}
	for _, tt := range tests {
		if got := sanitizeName(tt.in); got != tt.want {
			t.Errorf("sanitizeName(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestMajorMinor(t *testing.T) {
	tests := []struct{ in, want string }{
		{"1.25.5", "1.25"},
		{">=22.1.0", ">=22.1"}, // callers trim range operators first
		{"3.13", "3.13"},
		{"v20.0.0", "20.0"},
		{"22", "22"},
	}
	for _, tt := range tests {
		if got := majorMinor(tt.in); got != tt.want {
			t.Errorf("majorMinor(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestGenerateDockerfileForEveryStack asserts the invariants that matter for a
// production image, across every template.
func TestGenerateDockerfileForEveryStack(t *testing.T) {
	stacks := []struct {
		kind Kind
		det  Detection
	}{
		{KindGo, Detection{Stack: KindGo, Name: "api", Port: 8080, MainPackage: "./cmd/api", Runtime: "1.25"}},
		{KindNode, Detection{Stack: KindNode, Name: "web", Port: 3000, PackageManager: PMNpm, Runtime: "22", BuildCommand: "npm run build", StartCommand: "npm run start"}},
		{KindNode, Detection{Stack: KindNode, Name: "spa", Port: 80, PackageManager: PMPnpm, Framework: "vite", Runtime: "22"}},
		{KindPython, Detection{Stack: KindPython, Name: "api", Port: 8000, PackageManager: PMUv, Runtime: "3.13", StartCommand: "uvicorn main:app --host 0.0.0.0"}},
		{KindRuby, Detection{Stack: KindRuby, Name: "web", Port: 3000, Framework: "rails", Runtime: "3.4", StartCommand: "./bin/rails server -b 0.0.0.0"}},
		{KindRust, Detection{Stack: KindRust, Name: "svc", Port: 8080, BinaryName: "svc"}},
		{KindStatic, Detection{Stack: KindStatic, Name: "site", Port: 80}},
	}

	for _, s := range stacks {
		t.Run(string(s.kind)+"-"+s.det.Name, func(t *testing.T) {
			content, err := GenerateDockerfile(s.det)
			if err != nil {
				t.Fatalf("GenerateDockerfile: %v", err)
			}

			// Multi-stage: the runtime image must not carry a compiler toolchain.
			if !strings.Contains(content, "AS runtime") {
				t.Error("expected a distinct runtime stage")
			}
			if !strings.Contains(content, "# syntax=docker/dockerfile:1") {
				t.Error("expected a syntax directive for BuildKit features")
			}

			// Exec form is required for signal delivery; shell form breaks graceful
			// shutdown and therefore zero-downtime rollouts.
			for _, line := range strings.Split(content, "\n") {
				trimmed := strings.TrimSpace(line)
				for _, verb := range []string{"CMD ", "ENTRYPOINT "} {
					if rest, ok := strings.CutPrefix(trimmed, verb); ok {
						if !strings.HasPrefix(rest, "[") {
							t.Errorf("%s must use exec form for signal handling, got: %s", verb, trimmed)
						}
					}
				}
			}

			// Non-root, unless the base image already guarantees it.
			hasUser := strings.Contains(content, "USER ")
			usesUnprivilegedBase := strings.Contains(content, "nginx-unprivileged")
			if !hasUser && !usesUnprivilegedBase {
				t.Error("runtime must not run as root")
			}
		})
	}
}

func TestGenerateDockerfileNodeInstallsProdDepsAfterCopy(t *testing.T) {
	content, err := GenerateDockerfile(Detection{
		Stack: KindNode, Name: "web", Port: 3000,
		PackageManager: PMNpm, Runtime: "22",
		BuildCommand: "npm run build", StartCommand: "npm run start",
	})
	if err != nil {
		t.Fatal(err)
	}

	// The production install must come after the source copy, or the copy would
	// overwrite node_modules with the dev-inclusive tree from the build stage.
	copyIdx := strings.Index(content, "COPY --from=build /app .")
	prodIdx := strings.Index(content, "--omit=dev")
	if copyIdx < 0 || prodIdx < 0 {
		t.Fatalf("expected both a source copy and a production install:\n%s", content)
	}
	if prodIdx < copyIdx {
		t.Error("production install must run after COPY --from=build, or it is overwritten")
	}
}

// TestGenerateDockerfileGoCrossCompiles covers the case that motivated the
// change: an arm64 laptop building for linux/amd64. Go must cross-compile on
// the builder's own architecture rather than run the toolchain under QEMU.
func TestGenerateDockerfileGoCrossCompiles(t *testing.T) {
	content, err := GenerateDockerfile(Detection{
		Stack: KindGo, Name: "api", Port: 8080, MainPackage: "./cmd/api", Runtime: "1.25",
	})
	if err != nil {
		t.Fatal(err)
	}

	// The compiler stage runs natively; anything else means emulation.
	if !strings.Contains(content, "FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build") {
		t.Errorf("build stage must be pinned to the builder's platform:\n%s", content)
	}

	// BuildKit's platform args are global-scope only. Undeclared, they expand to
	// empty and go build silently targets the builder's own architecture.
	for _, arg := range []string{"ARG TARGETOS", "ARG TARGETARCH"} {
		if !strings.Contains(content, arg+"\n") {
			t.Errorf("missing %q; the platform arg is unusable inside a stage without it", arg)
		}
	}
	if !strings.Contains(content, "GOOS=$TARGETOS GOARCH=$TARGETARCH") {
		t.Errorf("go build must be told the target platform:\n%s", content)
	}

	// Declaration has to precede the build that consumes it.
	if strings.Index(content, "ARG TARGETARCH") > strings.Index(content, "GOOS=$TARGETOS") {
		t.Error("ARG TARGETARCH must be declared before the go build that uses it")
	}

	// The runtime stage must stay unqualified so BuildKit resolves it to the
	// target platform; pinning it would ship a builder-architecture base image.
	if strings.Contains(content, "FROM --platform=$BUILDPLATFORM gcr.io/distroless") {
		t.Error("runtime stage must resolve to the target platform, not the builder's")
	}

	// Cross-compiling with cgo would need a target C toolchain that is not here.
	if !strings.Contains(content, "CGO_ENABLED=0") {
		t.Error("cross-compilation requires cgo to stay off")
	}
}

// TestGenerateDockerfileRuntimeStagesMatchTargetPlatform guards the invariant
// that produces "exec format error" when it is broken: any stage whose output
// is copied into the runtime image must be built for the target architecture.
func TestGenerateDockerfileRuntimeStagesMatchTargetPlatform(t *testing.T) {
	// Native modules, compiled Python extensions and native gems are all copied
	// into the runtime image, so their build stages must not be pinned.
	stacks := []Detection{
		{Stack: KindNode, Name: "web", Port: 3000, PackageManager: PMNpm, Runtime: "22", StartCommand: "node index.js"},
		{Stack: KindPython, Name: "api", Port: 8000, PackageManager: PMUv, Runtime: "3.13"},
		{Stack: KindRuby, Name: "web", Port: 3000, Framework: "rails", Runtime: "3.4"},
		{Stack: KindRust, Name: "svc", Port: 8080, BinaryName: "svc"},
	}

	for _, d := range stacks {
		t.Run(string(d.Stack)+"-"+d.Name, func(t *testing.T) {
			content, err := GenerateDockerfile(d)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(content, "--platform=$BUILDPLATFORM") {
				t.Errorf("%s produces target-architecture artifacts; pinning a stage to the builder would ship unloadable binaries:\n%s", d.Stack, content)
			}
		})
	}

	// A Vite build is the exception: only bundled assets cross into nginx, so
	// the bundler can run natively.
	content, err := GenerateDockerfile(Detection{
		Stack: KindNode, Name: "spa", Port: 80, PackageManager: PMPnpm, Framework: "vite", Runtime: "22",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content, "FROM --platform=$BUILDPLATFORM node:22-alpine AS build") {
		t.Errorf("SPA asset build emits architecture-independent output and should run natively:\n%s", content)
	}
	if strings.Contains(content, "FROM --platform=$BUILDPLATFORM nginx") {
		t.Error("the nginx runtime must resolve to the target platform")
	}
}

func TestGenerateDockerfileUnknownStackFails(t *testing.T) {
	_, err := GenerateDockerfile(Detection{Stack: KindUnknown})
	if err == nil {
		t.Fatal("expected an error for an unsupported stack")
	}
	if !strings.Contains(err.Error(), "Dockerfile") {
		t.Errorf("error should tell the user to write a Dockerfile, got: %v", err)
	}
}

func TestJSONArgs(t *testing.T) {
	if got := jsonArgs("npm run start"); got != `["npm", "run", "start"]` {
		t.Errorf("jsonArgs = %s", got)
	}
	if got := jsonArgs("/app"); got != `["/app"]` {
		t.Errorf("jsonArgs = %s", got)
	}
}
