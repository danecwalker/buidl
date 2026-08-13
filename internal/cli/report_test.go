package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/danecwalker/buidl/internal/deploy"
	"github.com/danecwalker/buidl/internal/release"
	"github.com/danecwalker/buidl/internal/ui"
)

// newTestApp builds an App whose output is captured, so rendering can be asserted
// on directly rather than eyeballed.
func newTestApp(t *testing.T, mode ui.Mode) (*App, *bytes.Buffer) {
	t.Helper()
	// Clear CI markers so mode resolution and annotation behavior are stable
	// regardless of where the tests run.
	for _, key := range []string{"CI", "GITHUB_ACTIONS", "GITHUB_OUTPUT", "GITLAB_CI", "BUILDKITE"} {
		t.Setenv(key, "")
	}

	buf := &bytes.Buffer{}
	printer := ui.New(ui.Options{Out: buf, ErrOut: buf, Mode: mode})
	return &App{opts: &globalOptions{}, log: printer}, buf
}

func samplePlan() *deploy.Plan {
	rel := release.Release{
		ID:     "a1b2c3d-tjnz3d",
		Repo:   "ghcr.io/acme/web",
		Digest: "sha256:" + strings.Repeat("b", 64),
	}
	return &deploy.Plan{
		Environment: "production",
		Release:     rel,
		Changes: []deploy.Change{
			{
				Action: deploy.ActionUpdate, Kind: "Deployment", Name: "web",
				Fields: []deploy.FieldChange{
					{Field: "image", From: "sha256:aaaaaaaaaaaa", To: "sha256:bbbbbbbbbbbb"},
					{Field: "replicas", From: "3", To: "5"},
				},
				Impact: "replaces 5 instances",
				Diff:   "-   replicas: 3\n+   replicas: 5\n",
			},
			{Action: deploy.ActionCreate, Kind: "Ingress", Name: "web", Impact: "publishes externally"},
			{Action: deploy.ActionUnchanged, Kind: "Service", Name: "web"},
		},
		Warnings: []string{"deploying from a dirty working tree"},
	}
}

func TestRenderPlanShowsFieldsAndImpact(t *testing.T) {
	app, out := newTestApp(t, ui.ModePlain)
	app.renderPlan(samplePlan(), false)

	got := out.String()

	// The point of the plan table: what changed, and what it will do.
	for _, want := range []string{
		"production",
		"a1b2c3d-tjnz3d",
		"Deployment",
		"image: sha256:aaaaaaaaaaaa → sha256:bbbbbbbbbbbb",
		"replicas: 3 → 5",
		"replaces 5 instances",
		"publishes externally",
		"1 to create, 1 to update, 1 unchanged",
		"deploying from a dirty working tree",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("plan output missing %q:\n%s", want, got)
		}
	}

	// Without --detailed, the raw YAML diff stays out of the way.
	if strings.Contains(got, "+   replicas: 5") {
		t.Errorf("raw diff should be hidden without --detailed:\n%s", got)
	}
}

func TestRenderPlanDetailedIncludesDiff(t *testing.T) {
	app, out := newTestApp(t, ui.ModePlain)
	app.renderPlan(samplePlan(), true)

	got := out.String()
	if !strings.Contains(got, "+   replicas: 5") {
		t.Errorf("--detailed should include the raw diff:\n%s", got)
	}
}

func TestRenderPlanNoChanges(t *testing.T) {
	app, out := newTestApp(t, ui.ModePlain)
	app.renderPlan(&deploy.Plan{
		Environment: "staging",
		Changes: []deploy.Change{
			{Action: deploy.ActionUnchanged, Kind: "Deployment", Name: "web"},
		},
	}, false)

	got := out.String()
	if !strings.Contains(got, "already up to date") {
		t.Errorf("expected an up-to-date message:\n%s", got)
	}
}

func sampleOutcome() *deploy.Outcome {
	return &deploy.Outcome{
		Release: release.Release{
			ID:     "a1b2c3d-tjnz3d",
			Repo:   "ghcr.io/acme/web",
			Digest: "sha256:" + strings.Repeat("b", 64),
		},
		PreviousRelease: "9f8e7d6-tjnyy0",
		URL:             "https://acme.com",
		Duration:        42 * time.Second,
		Changes: []deploy.Change{
			{
				Action: deploy.ActionUpdate, Kind: "Deployment", Name: "web", Applied: true,
				Fields: []deploy.FieldChange{{Field: "image", From: "sha256:aaa", To: "sha256:bbb"}},
				Impact: "replaces 2 instances",
			},
			{Action: deploy.ActionUnchanged, Kind: "Service", Name: "web", Applied: true},
		},
		Instances: []deploy.PodStatus{
			{Name: "web-7d9f-abcde", Phase: "Running", Ready: true, Age: 30 * time.Second, Node: "node-1"},
			{Name: "web-7d9f-fghij", Phase: "Running", Ready: true, Age: 25 * time.Second, Node: "node-2"},
		},
	}
}

func TestReportOutcomeShowsWhatWasDoneAndWhatIsRunning(t *testing.T) {
	app, out := newTestApp(t, ui.ModePlain)

	// A step gives the summary something to account for.
	app.log.Step("Applying manifests")
	app.reportOutcome(sampleOutcome())

	got := out.String()

	for _, want := range []string{
		// What was done.
		"Changes",
		"applied",
		"Deployment",
		"image: sha256:aaa → sha256:bbb",
		"replaces 2 instances",
		"unchanged",
		// What is running.
		"Running instances (2/2 ready)",
		"web-7d9f-abcde",
		"node-1",
		// Where the time went.
		"Deploy summary",
		"Applying manifests",
		"total",
		// The outcome itself.
		"deployed a1b2c3d-tjnz3d",
		"https://acme.com",
		"9f8e7d6-tjnyy0",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("deploy report missing %q:\n%s", want, got)
		}
	}
}

func TestReportPartialFailureNamesWhatLanded(t *testing.T) {
	app, out := newTestApp(t, ui.ModePlain)

	outcome := &deploy.Outcome{
		Partial: true,
		Changes: []deploy.Change{
			{Action: deploy.ActionUpdate, Kind: "Secret", Name: "web-env", Applied: true},
			{Action: deploy.ActionCreate, Kind: "ServiceAccount", Name: "web", Applied: true},
			{Kind: "Deployment", Name: "web", Err: errForbidden{}},
		},
	}

	app.reportPartialFailure(outcome)
	got := out.String()

	// Apply is not atomic, so the user must be told exactly where things stand.
	for _, want := range []string{
		"mixed state",
		"already applied",
		"Secret/web-env",
		"ServiceAccount/web",
		"failed to apply Deployment/web",
		"idempotent",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("partial failure report missing %q:\n%s", want, got)
		}
	}
	// The object that failed must not be listed as applied.
	appliedSection := got[strings.Index(got, "already applied"):]
	if strings.Contains(appliedSection[:strings.Index(appliedSection, "failed to apply")], "Deployment/web") {
		t.Errorf("the failed object must not appear as applied:\n%s", got)
	}
}

type errForbidden struct{}

func (errForbidden) Error() string { return "forbidden: insufficient RBAC" }

func TestReportOutcomeJSONIsStructured(t *testing.T) {
	app, out := newTestApp(t, ui.ModeJSON)

	app.log.Step("Applying manifests")
	app.reportOutcome(sampleOutcome())

	// Every line must be a valid event, and one must carry the summary fields a
	// CI job would assert on.
	var sawSummary, sawComplete bool
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var event ui.Event
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("non-JSON line in JSON mode: %v\n%s", err, line)
		}
		if event.Fields["steps"] != nil {
			sawSummary = true
		}
		if event.Message == "deploy complete" {
			sawComplete = true
			if event.Fields["release"] != "a1b2c3d-tjnz3d" {
				t.Errorf("release field = %v", event.Fields["release"])
			}
			if event.Fields["url"] != "https://acme.com" {
				t.Errorf("url field = %v", event.Fields["url"])
			}
			// A CI job wants counts without parsing prose.
			if event.Fields["applied"] == nil || event.Fields["instances"] == nil {
				t.Errorf("expected applied and instances counts, got %v", event.Fields)
			}
		}
	}
	if !sawSummary {
		t.Error("expected a structured step summary event in JSON mode")
	}
	if !sawComplete {
		t.Error("expected a deploy complete event in JSON mode")
	}
}

func TestActionMarkers(t *testing.T) {
	cases := map[deploy.Action]string{
		deploy.ActionCreate:    "+",
		deploy.ActionUpdate:    "~",
		deploy.ActionDelete:    "-",
		deploy.ActionUnchanged: " ",
	}
	for action, want := range cases {
		if got := actionMarker(action); got != want {
			t.Errorf("actionMarker(%s) = %q, want %q", action, got, want)
		}
	}
}

// TestPlanOutputIsHumanReadable prints a rendered plan and deploy report so a
// reviewer can see the actual shape of the output, not just assertions about it.
func TestPlanOutputIsHumanReadable(t *testing.T) {
	app, out := newTestApp(t, ui.ModePlain)
	app.renderPlan(samplePlan(), false)

	app2, out2 := newTestApp(t, ui.ModePlain)
	app2.log.Step("Building image")
	app2.log.Step("Applying manifests")
	app2.log.Step("Waiting for health checks")
	app2.reportOutcome(sampleOutcome())

	t.Logf("\n--- buidl plan ---\n%s\n--- buidl deploy ---\n%s", out.String(), out2.String())
}
