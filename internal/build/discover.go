package build

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// wellKnownBuilder is the container name we look for and the name we give a
// container we create. A reboot then becomes `docker start`, not another run.
const wellKnownBuilder = "buildkitd"

// defaultBuilderImage is created when nothing is found. The tag is the same
// version as github.com/moby/buildkit in go.mod. :latest moves without a
// buidl release; a newer daemon is usually fine, an older one is not.
// Keep this in sync with go.mod (TestDefaultBuilderImageMatchesModule).
const defaultBuilderImage = "moby/buildkit:v0.25.1"

// containerProbeTimeout bounds how long we wait on `docker inspect`. A hung
// Docker daemon should not stall a deploy for the full command timeout.
const containerProbeTimeout = 5 * time.Second

// containerCreateTimeout covers pulling the image on a first run.
const containerCreateTimeout = 3 * time.Minute

// containerCLIs is the ordered list of runtimes that can host a BuildKit
// container. The first one that is on PATH and has a builder wins.
var containerCLIs = []containerCLI{
	{bin: "docker", scheme: "docker-container"},
	{bin: "podman", scheme: "podman-container"},
	{bin: "nerdctl", scheme: "nerdctl-container"},
}

type containerCLI struct {
	bin    string
	scheme string
}

// Overridable in tests so discovery does not shell out to a real Docker.
var (
	lookPath = exec.LookPath
	runCLI   = func(ctx context.Context, name string, args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, name, args...)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		if err != nil {
			if stderr.Len() > 0 {
				return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
			}
			return "", err
		}
		return stdout.String(), nil
	}
)

// discoverContainerBuilder looks for a BuildKit daemon already running as a
// container, and creates one if the machine has Docker (or Podman/nerdctl)
// but nothing we can use. An existing container of any moby/buildkit tag is
// reused: we do not replace the user's builder. The pin applies only to
// containers we create.
func discoverContainerBuilder(ctx context.Context, log Logger) (string, error) {
	probeCtx, cancel := context.WithTimeout(ctx, containerProbeTimeout)
	defer cancel()

	var first *containerCLI
	for i := range containerCLIs {
		cli := &containerCLIs[i]
		if _, err := lookPath(cli.bin); err != nil {
			continue
		}
		if first == nil {
			first = cli
		}
		addr, err := cli.discover(probeCtx)
		if err != nil {
			if probeCtx.Err() != nil {
				return "", fmt.Errorf("checking %s for a BuildKit container: %w", cli.bin, probeCtx.Err())
			}
			// The CLI is installed but this probe failed (daemon not up,
			// permission). Try the next runtime before creating.
			continue
		}
		if addr != "" {
			return addr, nil
		}
	}
	if first == nil {
		return "", nil
	}
	if log != nil {
		log.Info("no BuildKit found; creating %s://%s (%s)", first.scheme, wellKnownBuilder, defaultBuilderImage)
	}
	return first.create(ctx)
}

func (c containerCLI) createArgs() []string {
	return []string{
		"run", "-d",
		"--name", wellKnownBuilder,
		"--restart", "unless-stopped",
		"--privileged",
		defaultBuilderImage,
	}
}

func (c containerCLI) create(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, containerCreateTimeout)
	defer cancel()
	if _, err := runCLI(ctx, c.bin, c.createArgs()...); err != nil {
		return "", fmt.Errorf("creating %s://%s: %w\n\n%s", c.scheme, wellKnownBuilder, err, buildkitHint())
	}
	return c.scheme + "://" + wellKnownBuilder, nil
}

func (c containerCLI) discover(ctx context.Context) (string, error) {
	if addr, ok, err := c.wellKnown(ctx); err != nil {
		return "", err
	} else if ok {
		return addr, nil
	}

	out, err := runCLI(ctx, c.bin, "ps", "--filter", "status=running", "--format", "{{.Names}}\t{{.Image}}")
	if err != nil {
		return "", err
	}
	if name := pickRunningBuildkit(out); name != "" {
		return c.scheme + "://" + name, nil
	}
	return "", nil
}

func (c containerCLI) wellKnown(ctx context.Context) (string, bool, error) {
	out, err := runCLI(ctx, c.bin, "inspect", "-f", "{{.State.Status}}", wellKnownBuilder)
	if err != nil {
		return "", false, nil
	}
	switch strings.TrimSpace(out) {
	case "running":
		return c.scheme + "://" + wellKnownBuilder, true, nil
	case "exited", "created":
		if _, err := runCLI(ctx, c.bin, "start", wellKnownBuilder); err != nil {
			return "", false, nil
		}
		return c.scheme + "://" + wellKnownBuilder, true, nil
	default:
		return "", false, nil
	}
}

// pickRunningBuildkit chooses a container from `docker ps` output. A container
// named buildkitd wins; otherwise the first moby/buildkit image, by name, so
// the choice is stable across runs.
func pickRunningBuildkit(ps string) string {
	var others []string
	for _, line := range strings.Split(ps, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name, image, _ := strings.Cut(line, "\t")
		name = firstContainerName(name)
		if name == "" {
			continue
		}
		if name == wellKnownBuilder {
			return name
		}
		if strings.Contains(image, "moby/buildkit") {
			others = append(others, name)
		}
	}
	sort.Strings(others)
	if len(others) == 0 {
		return ""
	}
	return others[0]
}

func firstContainerName(names string) string {
	name := strings.Split(names, ",")[0]
	return strings.TrimPrefix(strings.TrimSpace(name), "/")
}
