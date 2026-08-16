package watch

import (
	"testing"
	"time"
)

func TestFormatCPUMemory(t *testing.T) {
	if got := FormatCPU(Usage{}); got != "—" {
		t.Errorf("unknown CPU = %q, want —", got)
	}
	if got := FormatMemory(Usage{}); got != "—" {
		t.Errorf("unknown memory = %q, want —", got)
	}
	if got := FormatCPU(Usage{Known: true, CPUMilli: 45}); got != "45m" {
		t.Errorf("45m = %q", got)
	}
	if got := FormatCPU(Usage{Known: true, CPUMilli: 1200}); got != "1.2" {
		t.Errorf("1.2 = %q", got)
	}
	if got := FormatCPU(Usage{Known: true, CPUMilli: 2000}); got != "2" {
		t.Errorf("2 = %q", got)
	}
	if got := FormatMemory(Usage{Known: true, Memory: 128 * 1024 * 1024}); got != "128Mi" {
		t.Errorf("128Mi = %q", got)
	}
	if got := FormatMemory(Usage{Known: true, Memory: 1536 * 1024 * 1024}); got != "1.5Gi" {
		t.Errorf("1.5Gi = %q", got)
	}
}

func TestFormatAgeUptime(t *testing.T) {
	if got := FormatAge(12 * time.Second); got != "12s" {
		t.Errorf("12s = %q", got)
	}
	if got := FormatAge(4 * time.Minute); got != "4m" {
		t.Errorf("4m = %q", got)
	}
	if got := FormatAge(3 * time.Hour); got != "3h" {
		t.Errorf("3h = %q", got)
	}
	if got := FormatAge(50 * time.Hour); got != "2D" {
		t.Errorf("2D = %q", got)
	}
	if got := FormatUptime(time.Time{}, 0); got != "—" {
		t.Errorf("not started = %q, want —", got)
	}
	if got := FormatUptime(time.Now().Add(-2*time.Hour), 2*time.Hour); got != "2h" {
		t.Errorf("uptime = %q", got)
	}
}

func TestFormatNodeUsage(t *testing.T) {
	n := Node{
		CPUAlloc: 2000,
		MemAlloc: 4 * 1024 * 1024 * 1024,
		Usage:    Usage{Known: true, CPUMilli: 500, Memory: 1200 * 1024 * 1024},
	}
	if got := FormatNodeCPU(n); got != "500m/2" {
		t.Errorf("node cpu = %q", got)
	}
	if got := FormatNodeMemory(n); got != "1.2Gi/4Gi" {
		t.Errorf("node mem = %q", got)
	}
	if got := FormatNodeCPU(Node{}); got != "—" {
		t.Errorf("empty node cpu = %q", got)
	}
}

func TestAddUsage(t *testing.T) {
	a := Usage{Known: true, CPUMilli: 10, Memory: 100}
	b := Usage{Known: true, CPUMilli: 5, Memory: 50}
	sum := AddUsage(a, b)
	if !sum.Known || sum.CPUMilli != 15 || sum.Memory != 150 {
		t.Errorf("sum = %+v", sum)
	}
	if got := AddUsage(Usage{}, a); got != a {
		t.Errorf("unknown+a = %+v", got)
	}
}

func TestClampAndSelect(t *testing.T) {
	if got := ClampSelected(5, 3); got != 2 {
		t.Errorf("clamp high = %d", got)
	}
	if got := ClampSelected(-1, 3); got != 0 {
		t.Errorf("clamp low = %d", got)
	}
	if got := ClampSelected(1, 0); got != 0 {
		t.Errorf("clamp empty = %d", got)
	}

	s := Snapshot{Apps: []App{{Name: "web"}, {Name: "api"}, {Name: "postgres"}}}
	if got := s.SelectIndex("api"); got != 1 {
		t.Errorf("select api = %d", got)
	}
	if got := s.SelectIndex("missing"); got != 0 {
		t.Errorf("select missing = %d", got)
	}
}
