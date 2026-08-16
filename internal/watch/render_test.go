package watch

import (
	"strings"
	"testing"
	"time"
)

func sampleSnapshot() Snapshot {
	started := time.Now().Add(-4 * time.Hour)
	return Snapshot{
		Time:        time.Now().Add(-2 * time.Second),
		Stack:       "web",
		Environment: "production",
		Namespace:   "web",
		Context:     "web-production",
		Metrics:     MetricsOK,
		Apps: []App{
			{
				Name:      "web",
				Type:      "app",
				Health:    HealthHealthy,
				Ready:     2,
				Desired:   2,
				Restarts:  0,
				Release:   "c653135-tjnz3d",
				URL:       "https://example.com",
				StartedAt: started,
				Uptime:    4 * time.Hour,
				Usage:     Usage{Known: true, CPUMilli: 45, Memory: 128 * 1024 * 1024},
				Instances: []Instance{
					{
						Name:      "web-aaaa",
						Phase:     "Running",
						Ready:     true,
						StartedAt: started,
						Uptime:    4 * time.Hour,
						Node:      "node-1",
						Usage:     Usage{Known: true, CPUMilli: 22, Memory: 64 * 1024 * 1024},
					},
					{
						Name:      "web-bbbb",
						Phase:     "Running",
						Ready:     true,
						StartedAt: started,
						Uptime:    4 * time.Hour,
						Node:      "node-2",
						Usage:     Usage{Known: true, CPUMilli: 23, Memory: 64 * 1024 * 1024},
					},
				},
			},
			{
				Name:      "postgres",
				Type:      "postgres",
				Health:    HealthHealthy,
				Ready:     1,
				Desired:   1,
				Usage:     Usage{Known: true, CPUMilli: 8, Memory: 256 * 1024 * 1024},
				StartedAt: started,
				Uptime:    4 * time.Hour,
			},
		},
		Nodes: []Node{
			{
				Name:        "node-1",
				Ready:       true,
				Schedulable: true,
				Roles:       "control-plane",
				Age:         12 * 24 * time.Hour,
				CPUAlloc:    2000,
				MemAlloc:    4 * 1024 * 1024 * 1024,
				Usage:       Usage{Known: true, CPUMilli: 500, Memory: 1200 * 1024 * 1024},
			},
		},
	}
}

func TestRenderShowsWhatYouCameToWatch(t *testing.T) {
	out := Render(View{
		Snapshot:    sampleSnapshot(),
		Selected:    0,
		Now:         time.Now(),
		Width:       120,
		Interactive: true,
	})

	for _, want := range []string{
		"buidl watch",
		"web",
		"production",
		"stack",
		"postgres",
		"healthy",
		"2/2",
		"45m",
		"128Mi",
		"4h",
		"web-aaaa",
		"cluster",
		"node-1",
		"500m/2",
		"q quit",
		"┌",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q\n%s", want, out)
		}
	}
}

func TestRenderMissingMetricsHint(t *testing.T) {
	snap := sampleSnapshot()
	snap.Metrics = MetricsMissing
	snap.Apps[0].Usage = Usage{}
	out := Report(snap, false, 120)
	if !strings.Contains(out, "metrics-server missing") {
		t.Errorf("missing metrics-server hint:\n%s", out)
	}
	if !strings.Contains(out, "RAM and CPU need metrics-server") {
		t.Errorf("missing once-mode hint:\n%s", out)
	}
	if !strings.Contains(out, "—") {
		t.Errorf("unknown usage should render as —:\n%s", out)
	}
}

func TestRenderAlertsAndDegraded(t *testing.T) {
	snap := sampleSnapshot()
	snap.Apps[0].Health = HealthDegraded
	snap.Apps[0].Ready = 0
	snap.Apps[0].Instances[0].Ready = false
	snap.Apps[0].Instances[0].Message = "CrashLoopBackOff"
	snap.Alerts = []Alert{{Level: "crit", Text: "web is degraded (0/2 ready) — CrashLoopBackOff"}}
	out := Render(View{Snapshot: snap, Width: 120})
	if !strings.Contains(out, "alerts") {
		t.Errorf("missing alerts:\n%s", out)
	}
	if !strings.Contains(out, "degraded") {
		t.Errorf("missing degraded:\n%s", out)
	}
	if !strings.Contains(out, "CrashLoopBackOff") {
		t.Errorf("missing instance message:\n%s", out)
	}
}

func TestRenderDoesNotLeakEscapeCodesWithoutColor(t *testing.T) {
	out := Render(View{Snapshot: sampleSnapshot(), Width: 120, Color: false, Interactive: true})
	if strings.Contains(out, "\033[") {
		t.Errorf("plain render contains ANSI:\n%s", out)
	}
}

func TestRenderKeepsRAMAndUptimeWhenNarrow(t *testing.T) {
	out := Render(View{Snapshot: sampleSnapshot(), Width: 56, Interactive: false})
	if !strings.Contains(out, "128Mi") {
		t.Errorf("narrow view dropped RAM:\n%s", out)
	}
	if !strings.Contains(out, "4h") {
		t.Errorf("narrow view dropped uptime:\n%s", out)
	}
	if strings.Contains(strings.ToUpper(out), "RELEASE") {
		t.Errorf("narrow view should drop release before RAM:\n%s", out)
	}
}

func TestRenderLoadingFrame(t *testing.T) {
	out := Render(View{Interactive: true, Width: 80})
	if !strings.Contains(out, "loading") {
		t.Errorf("empty snapshot should say loading:\n%s", out)
	}
}

func TestRenderColorPaintsHealth(t *testing.T) {
	out := Render(View{Snapshot: sampleSnapshot(), Width: 120, Color: true})
	if !strings.Contains(out, attrGreen) {
		t.Errorf("colored healthy row should use green:\n%s", out)
	}
}

func TestRenderLinesStayInsideWidth(t *testing.T) {
	for _, width := range []int{56, 80, 100, 120} {
		out := Render(View{Snapshot: sampleSnapshot(), Width: width, Interactive: true, Height: 40})
		for i, line := range strings.Split(out, "\n") {
			if n := visibleLen(line); n > width {
				t.Errorf("width %d line %d is %d: %q", width, i, n, line)
			}
		}
	}
}

func TestRenderDrawsBoxesAndSparks(t *testing.T) {
	snap := sampleSnapshot()
	var hist History
	for i := 0; i < 16; i++ {
		s := snap
		s.Apps[0].Usage.CPUMilli = 20 + int64(i*3)
		s.Apps[0].Usage.Memory = 64*1024*1024 + int64(i)*1024*1024
		hist.Record(s)
	}
	out := Render(View{Snapshot: snap, History: hist, Width: 100, Interactive: true})
	if !strings.Contains(out, "┌") || !strings.Contains(out, "┐") {
		t.Errorf("missing box corners:\n%s", out)
	}
	if !strings.ContainsAny(out, "▁▂▃▄▅▆▇█") {
		t.Errorf("missing sparkline:\n%s", out)
	}
	if !strings.Contains(out, "░") && !strings.Contains(out, "█") {
		t.Errorf("missing cluster gauge:\n%s", out)
	}
}

func TestRenderTruncatesLongNamesToTerminalWidth(t *testing.T) {
	snap := sampleSnapshot()
	snap.Apps[0].Name = "community-counter"
	snap.Apps[0].Release = "e86ff6a-dirty-tjvak5"
	snap.Apps[0].Instances[0].Name = "community-counter-e86ff6a-dirty-tjvak5-7d8f9c4b6-xk2n4"
	out := Render(View{Snapshot: snap, Width: 56, Interactive: true})
	for _, line := range strings.Split(out, "\n") {
		if n := len([]rune(line)); n > 56 {
			t.Errorf("line wider than terminal (%d): %q", n, line)
		}
	}
	if !strings.Contains(out, "…") {
		t.Errorf("expected a truncated name on a narrow terminal:\n%s", out)
	}
}

func TestDumpDashboard(t *testing.T) {
	if !testing.Verbose() {
		t.Skip("use -v to print a frame")
	}
	snap := sampleSnapshot()
	snap.Stack = "community-counter"
	snap.Namespace = "community-counter"
	snap.Context = "community-counter"
	snap.Apps[0].Name = "community-counter"
	snap.Apps[0].Release = "e86ff6a-dirty-tjvak5"
	snap.Apps[0].Instances[0].Name = "community-counter-e86ff6a-dirty-tjvak5-7d8f9c4b6-xk2n4"
	var hist History
	for i := 0; i < 24; i++ {
		s := snap
		s.Apps[0].Usage.CPUMilli = 20 + int64(i%9)
		s.Apps[0].Usage.Memory = 64*1024*1024 + int64(i)*1024*1024
		s.Apps[1].Usage.CPUMilli = 6 + int64(i%4)
		s.Nodes[0].Usage.CPUMilli = 400 + int64(i*10)
		hist.Record(s)
	}
	t.Log("\n" + Render(View{Snapshot: snap, History: hist, Selected: 0, Width: 100, Height: 36, Interactive: true, Interval: 2 * time.Second, Now: time.Now()}))
}
