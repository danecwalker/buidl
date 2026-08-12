package ui

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestStepsAreRecordedInOrderWithOutcomes(t *testing.T) {
	clearCIEnv(t)
	p := New(Options{Out: &bytes.Buffer{}, ErrOut: &bytes.Buffer{}, Mode: ModePlain})

	p.Step("Building")
	p.StepDetail("sha256:abc123")

	p.Step("Applying")
	p.StepDetail("5 changed, 2 unchanged")

	p.Step("Skipped work")
	p.SkipStep("nothing to do")

	p.Step("Waiting")
	p.EndStep()

	steps := p.Steps()
	if len(steps) != 4 {
		t.Fatalf("recorded %d steps, want 4: %+v", len(steps), steps)
	}

	// Order must match execution order so the summary reads as a timeline.
	wantNames := []string{"Building", "Applying", "Skipped work", "Waiting"}
	for i, want := range wantNames {
		if steps[i].Name != want {
			t.Errorf("step %d = %q, want %q", i, steps[i].Name, want)
		}
	}

	// A step that closes without an explicit outcome succeeded.
	if steps[0].Status != StepOK {
		t.Errorf("Building status = %q, want ok", steps[0].Status)
	}
	if steps[0].Detail != "sha256:abc123" {
		t.Errorf("Building detail = %q", steps[0].Detail)
	}
	if steps[1].Detail != "5 changed, 2 unchanged" {
		t.Errorf("Applying detail = %q", steps[1].Detail)
	}
	if steps[2].Status != StepSkipped {
		t.Errorf("Skipped status = %q, want skipped", steps[2].Status)
	}
	if steps[2].Detail != "nothing to do" {
		t.Errorf("skip reason = %q", steps[2].Detail)
	}
}

func TestFailStepRecordsTheError(t *testing.T) {
	clearCIEnv(t)
	p := New(Options{Out: &bytes.Buffer{}, ErrOut: &bytes.Buffer{}, Mode: ModePlain})

	failure := errors.New("image pull failed")
	p.Step("Waiting for health checks")
	p.FailStep(failure)

	steps := p.Steps()
	if len(steps) != 1 {
		t.Fatalf("recorded %d steps, want 1", len(steps))
	}
	if steps[0].Status != StepFailed {
		t.Errorf("status = %q, want failed", steps[0].Status)
	}
	if !errors.Is(steps[0].Err, failure) {
		t.Errorf("recorded error = %v, want %v", steps[0].Err, failure)
	}
}

// TestErrorDuringStepMarksThatStepFailed checks that a failure is attributed to
// the phase it happened in, so the summary points at the right place.
func TestErrorDuringStepMarksThatStepFailed(t *testing.T) {
	clearCIEnv(t)
	var out bytes.Buffer
	p := New(Options{Out: &out, ErrOut: &out, Mode: ModePlain})

	p.Step("Preflight checks")
	p.Error("namespace does not exist")
	p.Step("Applying manifests")
	p.EndStep()

	steps := p.Steps()
	if len(steps) != 2 {
		t.Fatalf("recorded %d steps, want 2", len(steps))
	}
	if steps[0].Status != StepFailed {
		t.Errorf("the step containing the error should be failed, got %q", steps[0].Status)
	}
	// A later step must not inherit the earlier failure.
	if steps[1].Status != StepOK {
		t.Errorf("subsequent step status = %q, want ok", steps[1].Status)
	}
}

func TestStepsAreTimed(t *testing.T) {
	clearCIEnv(t)
	p := New(Options{Out: &bytes.Buffer{}, Mode: ModePlain})

	p.Step("Slow thing")
	time.Sleep(15 * time.Millisecond)
	p.EndStep()

	steps := p.Steps()
	if len(steps) != 1 {
		t.Fatalf("recorded %d steps", len(steps))
	}
	// Timing is what answers "where did the deploy spend its time".
	if steps[0].Duration < 10*time.Millisecond {
		t.Errorf("duration = %v, expected at least 10ms", steps[0].Duration)
	}
}

func TestSummaryClosesAnOpenStep(t *testing.T) {
	clearCIEnv(t)
	var out bytes.Buffer
	p := New(Options{Out: &out, Mode: ModePlain})

	p.Step("Still running")
	// Summary must account for an unclosed step rather than dropping it.
	p.Summary("Summary")

	if len(p.Steps()) != 1 {
		t.Errorf("Summary should close and record the open step, got %+v", p.Steps())
	}
	if !strings.Contains(out.String(), "Still running") {
		t.Errorf("summary output missing the open step:\n%s", out.String())
	}
}

func TestSummaryRendersEveryStep(t *testing.T) {
	clearCIEnv(t)
	var out bytes.Buffer
	p := New(Options{Out: &out, ErrOut: &out, Mode: ModePlain})

	p.Step("Building")
	p.StepDetail("sha256:abc")
	p.Step("Applying")
	p.Step("Failing")
	p.FailStep(errors.New("nope"))
	p.Summary("Deploy summary")

	got := out.String()
	for _, want := range []string{"Deploy summary", "Building", "sha256:abc", "Applying", "Failing", "FAIL", "total"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary missing %q:\n%s", want, got)
		}
	}
}

func TestSummaryIsSilentWithNoSteps(t *testing.T) {
	clearCIEnv(t)
	var out bytes.Buffer
	p := New(Options{Out: &out, Mode: ModePlain})

	// A command with no phases should not print an empty table.
	p.Summary("Summary")
	if out.Len() != 0 {
		t.Errorf("expected no output, got %q", out.String())
	}
}

func TestSummaryInJSONModeIsOneStructuredEvent(t *testing.T) {
	clearCIEnv(t)
	var out bytes.Buffer
	p := New(Options{Out: &out, ErrOut: &out, Mode: ModeJSON})

	p.Step("Building")
	p.StepDetail("sha256:abc")
	p.Step("Applying")
	p.FailStep(errors.New("forbidden"))
	p.Summary("Deploy summary")

	// The summary must be the last event and carry the full step list, so a CI job
	// can assert on the shape of a run without scraping prose.
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	var event Event
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &event); err != nil {
		t.Fatalf("summary line is not valid JSON: %v\n%s", err, lines[len(lines)-1])
	}
	if event.Message != "Deploy summary" {
		t.Errorf("message = %q", event.Message)
	}

	steps, ok := event.Fields["steps"].([]any)
	if !ok || len(steps) != 2 {
		t.Fatalf("expected 2 steps in fields, got %v", event.Fields["steps"])
	}
	first, _ := steps[0].(map[string]any)
	if first["step"] != "Building" || first["status"] != "ok" || first["detail"] != "sha256:abc" {
		t.Errorf("first step = %v", first)
	}
	second, _ := steps[1].(map[string]any)
	if second["status"] != "failed" || second["error"] != "forbidden" {
		t.Errorf("second step = %v", second)
	}
	if event.Fields["total_duration"] == nil {
		t.Error("expected a total_duration field")
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{0, "-"},
		{250 * time.Millisecond, "250ms"},
		{2500 * time.Millisecond, "2.5s"},
		{45 * time.Second, "45.0s"},
		{90 * time.Second, "1m30s"},
		{3725 * time.Second, "62m05s"},
	}
	for _, tt := range tests {
		if got := formatDuration(tt.d); got != tt.want {
			t.Errorf("formatDuration(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

func TestKeyValuesAlignsAndSkipsEmpty(t *testing.T) {
	clearCIEnv(t)
	var out bytes.Buffer
	p := New(Options{Out: &out, Mode: ModePlain})

	p.KeyValues([][2]string{
		{"release", "abc123"},
		{"url", ""},
		{"environment", "production"},
	})

	got := out.String()
	if strings.Contains(got, "url") {
		t.Errorf("an empty value should be omitted entirely:\n%s", got)
	}
	// Values line up because the key column is padded to the widest key.
	releaseCol := strings.Index(got, "abc123")
	envCol := strings.Index(got, "production")
	lineStart := strings.LastIndex(got[:envCol], "\n") + 1
	if releaseCol-1 != envCol-lineStart+strings.Index(got, "release") {
		// Compare offsets within their own lines instead.
		relLineStart := strings.LastIndex(got[:releaseCol], "\n") + 1
		if releaseCol-relLineStart != envCol-lineStart {
			t.Errorf("values are not aligned:\n%s", got)
		}
	}
}

func TestBulletsSkipsEmptyList(t *testing.T) {
	clearCIEnv(t)
	var out bytes.Buffer
	p := New(Options{Out: &out, Mode: ModePlain})

	p.Bullets("nothing", nil)
	if out.Len() != 0 {
		t.Errorf("expected no output for an empty list, got %q", out.String())
	}

	p.Bullets("applied", []string{"Secret/web-env", "Deployment/web"})
	got := out.String()
	for _, want := range []string{"applied:", "- Secret/web-env", "- Deployment/web"} {
		if !strings.Contains(got, want) {
			t.Errorf("bullets missing %q:\n%s", want, got)
		}
	}
}

// TestTableAlignsMultiByteCells guards the alignment bug that byte-length padding
// causes: field changes contain "→", and truncation adds "…".
func TestTableAlignsMultiByteCells(t *testing.T) {
	clearCIEnv(t)
	var out bytes.Buffer
	p := New(Options{Out: &out, Mode: ModePlain})

	p.Table([]string{"name", "change", "effect"}, [][]string{
		{"web", "image: a → b", "replaces 2"},
		{"api", "plain ascii", "no restart"},
	})

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected header plus two rows, got:\n%s", out.String())
	}
	// The final column must start at the same *visual* offset on both data rows.
	// Rune offsets, not byte offsets: "→" is three bytes, so byte positions would
	// legitimately differ on an correctly aligned row.
	first := runeOffsetOf(lines[1], "replaces 2")
	second := runeOffsetOf(lines[2], "no restart")
	if first != second {
		t.Errorf("multi-byte cell broke alignment (%d vs %d):\n%s", first, second, out.String())
	}
}

// runeOffsetOf returns the rune index at which substr begins in s.
func runeOffsetOf(s, substr string) int {
	byteIndex := strings.Index(s, substr)
	if byteIndex < 0 {
		return -1
	}
	return utf8.RuneCountInString(s[:byteIndex])
}

func TestIndentedPrefixesEveryLine(t *testing.T) {
	clearCIEnv(t)
	var out bytes.Buffer
	p := New(Options{Out: &out, Mode: ModePlain})

	p.Indented("| ", "line one\nline two\n")

	got := out.String()
	if strings.Count(got, "| ") != 2 {
		t.Errorf("expected both lines prefixed:\n%s", got)
	}
	// A trailing newline must not produce an extra empty prefixed line.
	if strings.Contains(got, "| \n") {
		t.Errorf("trailing newline produced an empty line:\n%s", got)
	}
}

// TestSpinnerNeverWritesToANonTerminal guards the property that makes the
// spinner safe: carriage returns and escape codes in a CI log or a redirected
// file produce thousands of lines of garbage.
func TestSpinnerNeverWritesToANonTerminal(t *testing.T) {
	clearCIEnv(t)

	for _, mode := range []Mode{ModePretty, ModePlain, ModeJSON} {
		var out bytes.Buffer
		p := New(Options{Out: &out, ErrOut: &out, Mode: mode})

		if p.spinnerEnabled() {
			t.Errorf("%s: spinner must not enable for a non-terminal writer", mode)
		}

		p.Step("Working")
		// Long enough that a live spinner would have ticked several times.
		time.Sleep(250 * time.Millisecond)
		p.Info("still going")
		p.EndStep()

		if got := out.String(); strings.Contains(got, "\r") || strings.Contains(got, "\033[K") {
			t.Errorf("%s: spinner control codes leaked into output: %q", mode, got)
		}
	}
}
