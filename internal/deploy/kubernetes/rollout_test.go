package kubernetes

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/danecwalker/buidl/internal/config"
)

func TestProbeTimeoutHintNamesConfiguredPaths(t *testing.T) {
	hint := probeTimeoutHint(&config.Config{
		Deploy: config.Deploy{
			Healthcheck: config.Healthcheck{
				Readiness: "/readyz",
				Liveness:  "/livez",
				Startup:   "/startupz",
			},
		},
	})
	for _, want := range []string{"GET /readyz", "/startupz", "/livez", "deploy.healthcheck.path"} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint missing %q:\n%s", want, hint)
		}
	}
}

func TestProbeTimeoutHintSinglePath(t *testing.T) {
	hint := probeTimeoutHint(&config.Config{
		Deploy: config.Deploy{
			Healthcheck: config.Healthcheck{
				Path:      "/up",
				Readiness: "/up",
				Liveness:  "/up",
				Startup:   "/up",
			},
		},
	})
	if !strings.Contains(hint, "GET /up") {
		t.Errorf("hint should name the configured path:\n%s", hint)
	}
	if strings.Contains(hint, "startup") || strings.Contains(hint, "liveness") {
		t.Errorf("a single-path healthcheck should not list three probes:\n%s", hint)
	}
}

func TestProbeTimeoutHintExec(t *testing.T) {
	hint := probeTimeoutHint(&config.Config{
		Deploy: config.Deploy{
			Healthcheck: config.Healthcheck{
				Command: []string{"pgrep", "sidekiq"},
			},
		},
	})
	if !strings.Contains(hint, "pgrep sidekiq") {
		t.Errorf("exec hint = %q", hint)
	}
	if strings.Contains(hint, "GET") {
		t.Errorf("exec healthcheck must not mention HTTP paths:\n%s", hint)
	}
}

func TestPodStatusDistinguishesStartupFromReadiness(t *testing.T) {
	started := false
	pod := corev1.Pod{
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Started: &started,
			}},
		},
	}
	if got := podStatus(pod).Message; !strings.Contains(got, "startup probe") {
		t.Errorf("unstarted container = %q, want startup probe", got)
	}

	yes := true
	pod.Status.ContainerStatuses[0].Started = &yes
	if got := podStatus(pod).Message; !strings.Contains(got, "readiness probe") {
		t.Errorf("started but unready = %q, want readiness probe", got)
	}
}
