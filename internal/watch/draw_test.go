package watch

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSparklineScalesToWindow(t *testing.T) {
	samples := []sample{
		{v: 1, known: true},
		{v: 5, known: true},
		{v: 10, known: true},
		{v: 5, known: true},
	}
	got := sparkline(samples, 4)
	if utf8.RuneCountInString(got) != 4 {
		t.Fatalf("spark width %d: %q", utf8.RuneCountInString(got), got)
	}
	runes := []rune(got)
	if runes[2] <= runes[0] {
		t.Errorf("peak should be the tallest glyph: %q", got)
	}
}

func TestSparklineUnknownIsGap(t *testing.T) {
	got := sparkline([]sample{{known: false}, {v: 4, known: true}}, 2)
	if !strings.HasPrefix(got, "·") {
		t.Errorf("unknown sample should be a gap: %q", got)
	}
}

func TestBarFills(t *testing.T) {
	got := bar(0.5, 10)
	if got != "█████░░░░░" {
		t.Errorf("50%% of 10: %q", got)
	}
	if bar(0, 4) != "░░░░" {
		t.Errorf("empty: %q", bar(0, 4))
	}
	if bar(1, 4) != "████" {
		t.Errorf("full: %q", bar(1, 4))
	}
}

func TestBoxWidthIsExact(t *testing.T) {
	v := View{Width: 40}
	lines := v.box("stack", []string{" hello", " world"}, 40, "")
	if len(lines) != 4 {
		t.Fatalf("box lines: %d", len(lines))
	}
	for _, line := range lines {
		if n := visibleLen(line); n != 40 {
			t.Errorf("line width %d: %q", n, line)
		}
	}
	if !strings.Contains(lines[0], "stack") {
		t.Errorf("title missing: %q", lines[0])
	}
}
