package ui

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// clearCIEnv removes every CI marker so detection tests start from a clean slate.
// Go test runs inherit the developer's or the runner's environment, which would
// otherwise make these tests pass or fail depending on where they run.
func clearCIEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"CI", "GITHUB_ACTIONS", "GITLAB_CI", "BUILDKITE", "CIRCLECI",
		"TF_BUILD", "JENKINS_URL", "GITHUB_ACTOR", "GITHUB_REF",
		"GITHUB_REPOSITORY", "GITHUB_RUN_ID", "GITHUB_SERVER_URL",
		"GITHUB_OUTPUT", "GITHUB_HEAD_REF", "GITHUB_REF_NAME",
	} {
		t.Setenv(key, "")
		// t.Setenv cannot unset, so clear via an explicit empty value and rely on
		// detection treating "" as absent.
	}
}

func TestDetectCIOutsideCI(t *testing.T) {
	clearCIEnv(t)
	if ci := DetectCI(); ci.Detected {
		t.Errorf("expected no CI detected, got %+v", ci)
	}
}

func TestDetectGitHubActions(t *testing.T) {
	clearCIEnv(t)
	t.Setenv("GITHUB_ACTIONS", "true")
	t.Setenv("GITHUB_ACTOR", "danewalker")
	t.Setenv("GITHUB_SERVER_URL", "https://github.com")
	t.Setenv("GITHUB_REPOSITORY", "acme/web")
	t.Setenv("GITHUB_RUN_ID", "42")
	t.Setenv("GITHUB_REF", "refs/pull/7/merge")

	ci := DetectCI()

	if !ci.Detected || ci.Provider != "github-actions" {
		t.Fatalf("Provider = %q, want github-actions", ci.Provider)
	}
	if ci.Actor != "danewalker" {
		t.Errorf("Actor = %q", ci.Actor)
	}
	// A PR number gives preview environments a stable name across branch renames.
	if ci.PullRequest != "7" {
		t.Errorf("PullRequest = %q, want 7", ci.PullRequest)
	}
	if ci.RunURL != "https://github.com/acme/web/actions/runs/42" {
		t.Errorf("RunURL = %q", ci.RunURL)
	}
}

func TestGitHubGroupingAndAnnotations(t *testing.T) {
	ci := CI{Provider: "github-actions", Detected: true}

	if got := ci.GroupStart("Building"); got != "::group::Building" {
		t.Errorf("GroupStart = %q", got)
	}
	if got := ci.GroupEnd(); got != "::endgroup::" {
		t.Errorf("GroupEnd = %q", got)
	}
	if got := ci.Annotate(LevelWarn, "careful"); got != "::warning::careful" {
		t.Errorf("Annotate warn = %q", got)
	}
	if got := ci.Annotate(LevelError, "broken"); got != "::error::broken" {
		t.Errorf("Annotate error = %q", got)
	}
	// Info must not become an annotation, or every log line would appear in the
	// run summary.
	if got := ci.Annotate(LevelInfo, "fyi"); got != "" {
		t.Errorf("Annotate info = %q, want empty", got)
	}
}

func TestAnnotationsAreSingleLine(t *testing.T) {
	ci := CI{Provider: "github-actions", Detected: true}
	// A multi-line message would break the workflow-command directive.
	got := ci.Annotate(LevelError, "line one\nline two\r\nline three")

	if strings.ContainsAny(got, "\n\r") {
		t.Errorf("annotation must be single-line, got %q", got)
	}
	// Percent is special inside workflow commands and must be escaped.
	if got := ci.Annotate(LevelError, "50% failed"); !strings.Contains(got, "%25") {
		t.Errorf("expected %% to be escaped, got %q", got)
	}
}

func TestUnknownProviderHasNoGroupingOrAnnotations(t *testing.T) {
	ci := CI{Provider: "generic", Detected: true}
	if got := ci.GroupStart("x"); got != "" {
		t.Errorf("GroupStart = %q, want empty for a generic provider", got)
	}
	// Callers fall back to plain text when this returns empty.
	if got := ci.Annotate(LevelError, "x"); got != "" {
		t.Errorf("Annotate = %q, want empty", got)
	}
}

func TestAutoModeUsesPlainInCI(t *testing.T) {
	clearCIEnv(t)
	t.Setenv("GITHUB_ACTIONS", "true")

	p := New(Options{Out: &bytes.Buffer{}, Mode: ModeAuto})
	// Escape codes and cursor tricks make CI logs unreadable.
	if p.Mode() != ModePlain {
		t.Errorf("Mode = %q, want plain in CI", p.Mode())
	}
}

func TestAutoModeUsesPlainForNonTerminal(t *testing.T) {
	clearCIEnv(t)
	p := New(Options{Out: &bytes.Buffer{}, Mode: ModeAuto})
	if p.Mode() != ModePlain {
		t.Errorf("Mode = %q, want plain when piped", p.Mode())
	}
}

func TestPlainOutputHasNoEscapeCodes(t *testing.T) {
	clearCIEnv(t)
	var out, errOut bytes.Buffer
	p := New(Options{Out: &out, ErrOut: &errOut, Mode: ModePlain})

	p.Step("Building")
	p.Info("compiling")
	p.Success("done")
	p.EndStep()

	combined := out.String() + errOut.String()
	if strings.Contains(combined, "\033[") {
		t.Errorf("plain output must contain no ANSI codes:\n%q", combined)
	}
	for _, want := range []string{"Building", "compiling", "done"} {
		if !strings.Contains(combined, want) {
			t.Errorf("output missing %q:\n%s", want, combined)
		}
	}
}

func TestJSONModeEmitsOneEventPerLine(t *testing.T) {
	clearCIEnv(t)
	var out bytes.Buffer
	p := New(Options{Out: &out, ErrOut: &out, Mode: ModeJSON})

	p.Step("Deploying")
	p.Info("applying manifests")
	p.Fields("complete", map[string]any{"release": "abc123"})

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 events, got %d:\n%s", len(lines), out.String())
	}

	for i, line := range lines {
		var ev Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("line %d is not valid JSON: %v\n%s", i, err, line)
		}
		if ev.Time.IsZero() {
			t.Errorf("line %d has no timestamp", i)
		}
	}

	// The step name must be attached to subsequent events so consumers can group.
	var second Event
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatal(err)
	}
	if second.Step != "Deploying" {
		t.Errorf("Step = %q, want Deploying", second.Step)
	}

	var third Event
	if err := json.Unmarshal([]byte(lines[2]), &third); err != nil {
		t.Fatal(err)
	}
	if third.Fields["release"] != "abc123" {
		t.Errorf("Fields = %v", third.Fields)
	}
}

func TestErrorMarksPrinterFailed(t *testing.T) {
	clearCIEnv(t)
	var out bytes.Buffer
	p := New(Options{Out: &out, ErrOut: &out, Mode: ModePlain})

	if p.Failed() {
		t.Error("a new printer must not be failed")
	}
	p.Warn("just a warning")
	if p.Failed() {
		t.Error("a warning must not mark the printer failed")
	}
	p.Error("a real failure")
	// The process exit code depends on this.
	if !p.Failed() {
		t.Error("Error must mark the printer failed")
	}
}

func TestDetailRequiresVerbose(t *testing.T) {
	clearCIEnv(t)
	var quiet, loud bytes.Buffer

	New(Options{Out: &quiet, ErrOut: &quiet, Mode: ModePlain}).Detail("noise")
	if quiet.Len() != 0 {
		t.Errorf("Detail should be suppressed without --verbose, got %q", quiet.String())
	}

	New(Options{Out: &loud, ErrOut: &loud, Mode: ModePlain, Verbose: true}).Detail("noise")
	if !strings.Contains(loud.String(), "noise") {
		t.Errorf("Detail should appear with --verbose, got %q", loud.String())
	}
}

func TestTableAligns(t *testing.T) {
	clearCIEnv(t)
	var out bytes.Buffer
	p := New(Options{Out: &out, Mode: ModePlain})

	p.Table([]string{"release", "status"}, [][]string{
		{"short", "ok"},
		{"a-much-longer-release-id", "failed"},
	})

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected a header and two rows, got:\n%s", out.String())
	}
	// The status column must start at the same offset on every row.
	col := strings.Index(lines[1], "ok")
	if col != strings.Index(lines[2], "failed") {
		t.Errorf("columns are not aligned:\n%s", out.String())
	}
}

// TestTableEmitsAnEventInJSONMode pins the JSON contract: every line decodes
// into an Event. A bare array would break a consumer doing line-by-line
// json.Unmarshal into a single type.
func TestTableEmitsAnEventInJSONMode(t *testing.T) {
	clearCIEnv(t)
	var out bytes.Buffer
	p := New(Options{Out: &out, Mode: ModeJSON})

	p.Table([]string{"Release", "Status"}, [][]string{{"abc", "ok"}})

	var event Event
	if err := json.Unmarshal(out.Bytes(), &event); err != nil {
		t.Fatalf("expected an Event: %v\n%s", err, out.String())
	}

	rows, ok := event.Fields["rows"].([]any)
	if !ok || len(rows) != 1 {
		t.Fatalf("expected one row in fields, got %v", event.Fields)
	}
	row, ok := rows[0].(map[string]any)
	if !ok {
		t.Fatalf("row = %v", rows[0])
	}
	// Headers become lowercase keys so consumers need not guess casing.
	if row["release"] != "abc" || row["status"] != "ok" {
		t.Errorf("row = %v", row)
	}
}

// TestEveryJSONLineIsAnEvent is the contract test for JSON mode across all the
// output primitives a command actually uses.
func TestEveryJSONLineIsAnEvent(t *testing.T) {
	clearCIEnv(t)
	var out bytes.Buffer
	p := New(Options{Out: &out, ErrOut: &out, Mode: ModeJSON, Verbose: true})

	p.Step("Applying")
	p.Info("applying manifests")
	p.Detail("wrote config")
	p.Success("done")
	p.Warn("careful")
	p.Error("broken")
	p.Table([]string{"a"}, [][]string{{"1"}})
	p.KeyValues([][2]string{{"key", "value"}})
	p.Bullets("items", []string{"one", "two"})
	p.Raw("raw line")
	p.Fields("result", map[string]any{"n": 1})
	p.Summary("Summary")

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) < 10 {
		t.Fatalf("expected many events, got %d:\n%s", len(lines), out.String())
	}
	for i, line := range lines {
		var event Event
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Errorf("line %d is not a valid Event: %v\n%s", i, err, line)
		}
		if event.Level == "" {
			t.Errorf("line %d has no level: %s", i, line)
		}
	}
}

func TestNoColorSuppressesEscapeCodes(t *testing.T) {
	clearCIEnv(t)
	var out bytes.Buffer
	// Pretty mode with NoColor must still avoid escape codes.
	p := New(Options{Out: &out, ErrOut: &out, Mode: ModePretty, NoColor: true})

	p.Step("Building")
	p.Success("done")

	if strings.Contains(out.String(), "\033[") {
		t.Errorf("NoColor must suppress ANSI codes, got %q", out.String())
	}
}

func TestSetOutputForGitHub(t *testing.T) {
	ci := CI{Provider: "github-actions", Detected: true}
	// The modern mechanism is a key=value line appended to $GITHUB_OUTPUT; the
	// old ::set-output:: directive is disabled on current runners.
	if got := ci.SetOutput("release", "abc123"); got != "release=abc123" {
		t.Errorf("SetOutput = %q", got)
	}
	if got := (CI{Provider: "generic"}).SetOutput("k", "v"); got != "" {
		t.Errorf("SetOutput = %q, want empty for a generic provider", got)
	}
}
