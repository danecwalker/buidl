package ui

import (
	"bytes"
	"strings"
	"testing"
)

func TestProgressPlainPrintsBuckets(t *testing.T) {
	clearCIEnv(t)
	buf := &bytes.Buffer{}
	p := New(Options{Out: buf, ErrOut: buf, Mode: ModePlain})

	const total int64 = 100
	p.Progress(0, total)
	p.Progress(10, total) // same 0–24 bucket, skipped
	p.Progress(25, total)
	p.Progress(100, total)

	got := buf.String()
	if strings.Count(got, "B") < 3 {
		t.Fatalf("progress lines = %q, want 0/25/100 buckets", got)
	}
	if !strings.Contains(got, "100%") {
		t.Errorf("missing 100%%:\n%s", got)
	}
}

func TestProgressJSONSilent(t *testing.T) {
	clearCIEnv(t)
	buf := &bytes.Buffer{}
	p := New(Options{Out: buf, ErrOut: buf, Mode: ModeJSON})
	p.Progress(50, 100)
	if buf.Len() != 0 {
		t.Errorf("JSON mode emitted progress: %s", buf.Bytes())
	}
}

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{2048, "2.0 KB"},
		{2 * 1024 * 1024, "2.0 MB"},
	}
	for _, tt := range tests {
		if got := formatBytes(tt.n); got != tt.want {
			t.Errorf("formatBytes(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}
