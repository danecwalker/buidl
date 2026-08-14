package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/danecwalker/buidl/internal/config"
	"github.com/danecwalker/buidl/internal/deploy"
	"github.com/danecwalker/buidl/internal/ui"
)

// accessoryPlan builds a plan with one restarting change and one inert one.
func accessoryPlan() *deploy.Plan {
	return &deploy.Plan{
		Environment: "production",
		Changes: []deploy.Change{
			{
				Action: deploy.ActionUpdate, Kind: "StatefulSet", Name: "web-postgres",
				Fields: []deploy.FieldChange{{Field: "memory limit", From: "512Mi", To: "1Gi"}},
				Impact: "restarts the accessory",
			},
			{Action: deploy.ActionUnchanged, Kind: "Service", Name: "web-postgres"},
		},
	}
}

func TestRenderAccessoryPlanReportsRestartImpact(t *testing.T) {
	app, out := newTestApp(t, ui.ModePlain)
	app.cfg = &config.Config{}
	app.renderAccessoryPlan(accessoryPlan(), false)

	got := out.String()
	for _, want := range []string{
		"production",
		"StatefulSet",
		"web-postgres",
		"memory limit: 512Mi → 1Gi",
		// The whole reason accessories are a separate verb: the user must be
		// able to see that this run restarts a database.
		"restarts the accessory",
		"0 to create, 1 to update, 1 unchanged",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("accessory plan output missing %q\n%s", want, got)
		}
	}
}

// A no-op run should read as a no-op rather than as an empty table, since this
// is also the command used to check for drift.
func TestRenderAccessoryPlanReportsNoChanges(t *testing.T) {
	app, out := newTestApp(t, ui.ModePlain)
	app.cfg = &config.Config{}
	app.renderAccessoryPlan(&deploy.Plan{
		Environment: "staging",
		Changes: []deploy.Change{
			{Action: deploy.ActionUnchanged, Kind: "StatefulSet", Name: "web-postgres"},
		},
	}, false)

	if got := out.String(); !strings.Contains(got, "match your configuration") {
		t.Errorf("expected a no-changes message, got:\n%s", got)
	}
}

func TestConfirmAccessoryApply(t *testing.T) {
	tests := []struct {
		name       string
		plan       *deploy.Plan
		yes        bool
		input      string
		wantErr    bool
		wantPrompt bool
	}{
		{
			// Creating an accessory that does not exist yet destroys nothing.
			name: "creation needs no confirmation",
			plan: &deploy.Plan{Changes: []deploy.Change{
				{Action: deploy.ActionCreate, Kind: "StatefulSet", Name: "web-postgres", Impact: "creates the accessory and its storage"},
			}},
		},
		{
			// An update with no runtime impact — a label edit — leaves the pod alone.
			name: "inert update needs no confirmation",
			plan: &deploy.Plan{Changes: []deploy.Change{
				{Action: deploy.ActionUpdate, Kind: "StatefulSet", Name: "web-postgres"},
			}},
		},
		{
			name:       "restart is confirmed",
			plan:       accessoryPlan(),
			input:      "y\n",
			wantPrompt: true,
		},
		{
			name:       "declining cancels",
			plan:       accessoryPlan(),
			input:      "n\n",
			wantErr:    true,
			wantPrompt: true,
		},
		{
			// No stdin is the same as declining: a restart must never happen
			// because nobody was there to say no.
			name:       "no stdin cancels",
			plan:       accessoryPlan(),
			input:      "",
			wantErr:    true,
			wantPrompt: true,
		},
		{
			name: "--yes skips the prompt",
			plan: accessoryPlan(),
			yes:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// The prompt only fires in pretty mode; newTestApp's plain mode
			// would skip it unconditionally and pass every case vacuously.
			app, _ := newTestApp(t, ui.ModePretty)
			app.cfg = &config.Config{}

			cmd := &cobra.Command{}
			out := &bytes.Buffer{}
			cmd.SetOut(out)
			cmd.SetIn(strings.NewReader(tc.input))

			err := app.confirmAccessoryApply(cmd, tc.plan, tc.yes)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error: %v", err, tc.wantErr)
			}
			if gotPrompt := strings.Contains(out.String(), "[y/N]"); gotPrompt != tc.wantPrompt {
				t.Errorf("prompted = %v, want %v (output: %q)", gotPrompt, tc.wantPrompt, out.String())
			}
		})
	}
}

// A config with no accessories must fail before any cluster contact, so the
// error names the real problem instead of a missing kubeconfig.
func TestAccessoryRequestRejectsEmptyConfig(t *testing.T) {
	app, _ := newTestApp(t, ui.ModePlain)
	app.cfg = &config.Config{Environment: "production"}
	app.path = "buidl.yaml"

	t.Setenv("KUBECONFIG", "/nonexistent/kubeconfig")
	t.Setenv("HOME", t.TempDir())

	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())
	_, _, err := app.accessoryRequest(cmd)
	if err == nil {
		t.Fatal("expected an error for a config with no accessories")
	}
	if !strings.Contains(err.Error(), "no accessories declared") {
		t.Errorf("error should name the missing accessories, got: %v", err)
	}
}
