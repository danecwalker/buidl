package watch

import "testing"

func TestHistoryKeepsAWindow(t *testing.T) {
	var h History
	h.cap = 4
	snap := sampleSnapshot()
	for i := 0; i < 10; i++ {
		snap.Apps[0].Usage.CPUMilli = int64(i + 1)
		h.Record(snap)
	}
	cpu := h.appCPU("web")
	if len(cpu) != 4 {
		t.Fatalf("capped length %d", len(cpu))
	}
	if cpu[0].v != 7 || cpu[3].v != 10 {
		t.Fatalf("window = %+v", cpu)
	}
}

func TestHistorySumsStack(t *testing.T) {
	var h History
	h.Record(sampleSnapshot())
	cpu := h.stackCPU()
	if len(cpu) != 1 || !cpu[0].known || cpu[0].v != 45+8 {
		t.Fatalf("stack cpu %+v", cpu)
	}
}
