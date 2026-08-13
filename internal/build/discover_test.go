package build

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/danecwalker/buidl/internal/config"
)

func TestPickRunningBuildkitPrefersWellKnownName(t *testing.T) {
	ps := "buildx_buildkit_default\tmoby/buildkit:buildx-stable-1\nbuildkitd\tmoby/buildkit:latest\n"
	if got := pickRunningBuildkit(ps); got != "buildkitd" {
		t.Fatalf("pickRunningBuildkit = %q, want buildkitd", got)
	}
}

func TestPickRunningBuildkitUsesBuildx(t *testing.T) {
	ps := "buildx_buildkit_desktop-linux\tmoby/buildkit:buildx-stable-1\n"
	if got := pickRunningBuildkit(ps); got != "buildx_buildkit_desktop-linux" {
		t.Fatalf("pickRunningBuildkit = %q", got)
	}
}

func TestPickRunningBuildkitIgnoresUnrelatedContainers(t *testing.T) {
	ps := "web\tnginx:latest\nredis\tredis:7\n"
	if got := pickRunningBuildkit(ps); got != "" {
		t.Fatalf("pickRunningBuildkit = %q, want empty", got)
	}
}

func TestDiscoverNamedRunningContainer(t *testing.T) {
	restore := stubCLI(t, map[string]cliResult{
		"docker inspect -f {{.State.Status}} buildkitd": {out: "running\n"},
	})
	defer restore()

	addr, err := discoverContainerBuilder(context.Background(), testLogger{})
	if err != nil {
		t.Fatalf("discoverContainerBuilder: %v", err)
	}
	if addr != "docker-container://buildkitd" {
		t.Fatalf("addr = %q", addr)
	}
}

func TestDiscoverStartsStoppedBuildkitd(t *testing.T) {
	var started bool
	restore := stubCLI(t, map[string]cliResult{
		"docker inspect -f {{.State.Status}} buildkitd": {out: "exited\n"},
		"docker start buildkitd":                        {out: "buildkitd\n", hook: func() { started = true }},
	})
	defer restore()

	addr, err := discoverContainerBuilder(context.Background(), testLogger{})
	if err != nil {
		t.Fatalf("discoverContainerBuilder: %v", err)
	}
	if addr != "docker-container://buildkitd" {
		t.Fatalf("addr = %q", addr)
	}
	if !started {
		t.Fatal("expected docker start buildkitd")
	}
}

func TestDiscoverFallsBackToBuildxContainer(t *testing.T) {
	restore := stubCLI(t, map[string]cliResult{
		"docker inspect -f {{.State.Status}} buildkitd": {err: errors.New("no such container")},
		"docker ps --filter status=running --format {{.Names}}\t{{.Image}}": {
			out: "buildx_buildkit_default\tmoby/buildkit:buildx-stable-1\n",
		},
	})
	defer restore()

	addr, err := discoverContainerBuilder(context.Background(), testLogger{})
	if err != nil {
		t.Fatalf("discoverContainerBuilder: %v", err)
	}
	if addr != "docker-container://buildx_buildkit_default" {
		t.Fatalf("addr = %q", addr)
	}
}

func TestDiscoverSkipsMissingRuntime(t *testing.T) {
	origLook, origRun := lookPath, runCLI
	t.Cleanup(func() { lookPath, runCLI = origLook, origRun })
	lookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	runCLI = func(context.Context, string, ...string) (string, error) {
		t.Fatal("runCLI should not be called when no runtime is on PATH")
		return "", nil
	}

	addr, err := discoverContainerBuilder(context.Background(), testLogger{})
	if err != nil {
		t.Fatalf("discoverContainerBuilder: %v", err)
	}
	if addr != "" {
		t.Fatalf("addr = %q, want empty", addr)
	}
}

func TestDiscoverCreatesPinnedImageWhenNoneExists(t *testing.T) {
	cli := containerCLI{bin: "docker", scheme: "docker-container"}
	var created []string
	restore := stubCLI(t, map[string]cliResult{
		"docker inspect -f {{.State.Status}} buildkitd":                     {err: errors.New("no such container")},
		"docker ps --filter status=running --format {{.Names}}\t{{.Image}}": {out: ""},
		"docker " + strings.Join(cli.createArgs(), " "): {
			out:  "abc123\n",
			hook: func() { created = cli.createArgs() },
		},
	})
	defer restore()

	addr, err := discoverContainerBuilder(context.Background(), testLogger{})
	if err != nil {
		t.Fatalf("discoverContainerBuilder: %v", err)
	}
	if addr != "docker-container://buildkitd" {
		t.Fatalf("addr = %q", addr)
	}
	if len(created) == 0 {
		t.Fatal("expected docker run")
	}
	joined := strings.Join(created, " ")
	if !strings.Contains(joined, defaultBuilderImage) {
		t.Fatalf("created %q, want image %s", joined, defaultBuilderImage)
	}
	if strings.Contains(joined, ":latest") {
		t.Fatalf("created %q, must not use :latest", joined)
	}
}

func TestDiscoverDoesNotCreateWhenBuildxExists(t *testing.T) {
	cli := containerCLI{bin: "docker", scheme: "docker-container"}
	restore := stubCLI(t, map[string]cliResult{
		"docker inspect -f {{.State.Status}} buildkitd": {err: errors.New("no such container")},
		"docker ps --filter status=running --format {{.Names}}\t{{.Image}}": {
			out: "buildx_buildkit_default\tmoby/buildkit:buildx-stable-1\n",
		},
		"docker " + strings.Join(cli.createArgs(), " "): {
			err: errors.New("docker run should not be called"),
		},
	})
	defer restore()

	addr, err := discoverContainerBuilder(context.Background(), testLogger{})
	if err != nil {
		t.Fatalf("discoverContainerBuilder: %v", err)
	}
	if addr != "docker-container://buildx_buildkit_default" {
		t.Fatalf("addr = %q", addr)
	}
}

func TestResolveAddrPrefersBuildAddr(t *testing.T) {
	bk := NewBuildKit(&config.Config{Build: config.Build{Addr: "tcp://explicit:1234"}}, testLogger{})
	addr, err := bk.resolveAddr(context.Background())
	if err != nil {
		t.Fatalf("resolveAddr: %v", err)
	}
	if addr != "tcp://explicit:1234" {
		t.Fatalf("addr = %q", addr)
	}
}

func TestResolveAddrPrefersEnvOverContainer(t *testing.T) {
	t.Setenv("BUILDKIT_HOST", "docker-container://from-env")
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	restore := stubCLI(t, map[string]cliResult{
		"docker inspect -f {{.State.Status}} buildkitd": {out: "running\n"},
	})
	defer restore()

	bk := NewBuildKit(&config.Config{}, testLogger{})
	addr, err := bk.resolveAddr(context.Background())
	if err != nil {
		t.Fatalf("resolveAddr: %v", err)
	}
	if addr != "docker-container://from-env" {
		t.Fatalf("addr = %q, want the env override", addr)
	}
}

func TestResolveAddrFindsContainerWhenUnset(t *testing.T) {
	t.Setenv("BUILDKIT_HOST", "")
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	restore := stubCLI(t, map[string]cliResult{
		"docker inspect -f {{.State.Status}} buildkitd": {out: "running\n"},
	})
	defer restore()

	bk := NewBuildKit(&config.Config{}, testLogger{})
	addr, err := bk.resolveAddr(context.Background())
	if err != nil {
		t.Fatalf("resolveAddr: %v", err)
	}
	if addr != "docker-container://buildkitd" {
		t.Fatalf("addr = %q", addr)
	}
}

func TestDefaultBuilderImageMatchesModule(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "go.mod"))
	if err != nil {
		t.Fatalf("reading go.mod: %v", err)
	}
	m := regexp.MustCompile(`(?m)^\tgithub.com/moby/buildkit v([0-9]+\.[0-9]+\.[0-9]+)$`).FindSubmatch(data)
	if m == nil {
		t.Fatal("go.mod has no github.com/moby/buildkit version")
	}
	want := "moby/buildkit:v" + string(m[1])
	if defaultBuilderImage != want {
		t.Fatalf("defaultBuilderImage = %q, want %q to match go.mod", defaultBuilderImage, want)
	}
}

type cliResult struct {
	out  string
	err  error
	hook func()
}

func stubCLI(t *testing.T, responses map[string]cliResult) (restore func()) {
	t.Helper()
	origLook, origRun := lookPath, runCLI
	lookPath = func(file string) (string, error) {
		if file == "docker" {
			return "/usr/bin/docker", nil
		}
		return "", exec.ErrNotFound
	}
	runCLI = func(_ context.Context, name string, args ...string) (string, error) {
		key := name + " " + strings.Join(args, " ")
		res, ok := responses[key]
		if !ok {
			return "", errors.New("unexpected command: " + key)
		}
		if res.hook != nil {
			res.hook()
		}
		return res.out, res.err
	}
	return func() { lookPath, runCLI = origLook, origRun }
}
