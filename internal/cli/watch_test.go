package cli

import (
	"strings"
	"testing"

	"github.com/danecwalker/buidl/internal/ui"
	"github.com/danecwalker/buidl/internal/watch"
)

func TestWatchCommandIsOnTheFrontDoor(t *testing.T) {
	app, _ := newTestApp(t, ui.ModePlain)
	cmd := newWatchCmd(app)
	if cmd.Hidden {
		t.Fatal("watch should stay on the front door")
	}
	if cmd.Flags().Lookup("once") == nil {
		t.Fatal("watch is missing --once")
	}
	if cmd.Flags().Lookup("interval") == nil {
		t.Fatal("watch is missing --interval")
	}
}

func TestPrintWatchJSONUsesHumanUnits(t *testing.T) {
	app, buf := newTestApp(t, ui.ModeJSON)
	snap := watch.Snapshot{
		Stack:       "web",
		Environment: "production",
		Namespace:   "web",
		Metrics:     watch.MetricsOK,
		Apps: []watch.App{{
			Name:    "web",
			Type:    "app",
			Health:  watch.HealthHealthy,
			Ready:   1,
			Desired: 1,
			Usage:   watch.Usage{Known: true, CPUMilli: 45, Memory: 128 * 1024 * 1024},
			Release: "rel-1",
			Instances: []watch.Instance{{
				Name:  "web-aaaa",
				Phase: "Running",
				Ready: true,
				Usage: watch.Usage{Known: true, CPUMilli: 45, Memory: 128 * 1024 * 1024},
			}},
		}},
	}
	if err := app.printWatch(newWatchCmd(app), snap, ""); err != nil {
		t.Fatalf("printWatch: %v", err)
	}
	got := buf.String()
	for _, want := range []string{`"45m"`, `"128Mi"`, `"healthy"`, `"web"`} {
		if !strings.Contains(got, want) {
			t.Errorf("json missing %s\n%s", want, got)
		}
	}
}
