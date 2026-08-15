package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/danecwalker/buidl/internal/ui"
)

func TestParseStaleDuration(t *testing.T) {
	tests := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{in: "", want: 0},
		{in: "7d", want: 7 * 24 * time.Hour},
		{in: "1w", want: 7 * 24 * time.Hour},
		{in: "24h", want: 24 * time.Hour},
		{in: "90m", want: 90 * time.Minute},
		{in: "0d", wantErr: true},
		{in: "bogus", wantErr: true},
		{in: "0s", wantErr: true},
	}
	for _, tt := range tests {
		got, err := parseStaleDuration(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("parseStaleDuration(%q) = %v, want error", tt.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseStaleDuration(%q): %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("parseStaleDuration(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestDestroyRequiresEnvWhenOverlaysExist(t *testing.T) {
	path := writeTempConfig(t, `
app: web
image: ghcr.io/acme/web
environments:
  staging: {}
  production: {}
`)
	app, _ := newTestApp(t, ui.ModePlain)
	app.opts.configPath = path

	cmd := newDestroyCmd(app)
	cmd.SetArgs([]string{"--dry-run", "--yes"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("destroy without -e must fail when overlays exist")
	}
	if !strings.Contains(err.Error(), "requires -e") {
		t.Errorf("error = %v, want a -e requirement", err)
	}
}

func TestDestroyAllowsOmittedEnvOnSingleTarget(t *testing.T) {
	path := writeTempConfig(t, `
app: web
image: ghcr.io/acme/web
`)
	app, _ := newTestApp(t, ui.ModePlain)
	app.opts.configPath = path
	app.opts.timeout = time.Millisecond

	cmd := newDestroyCmd(app)
	cmd.SetArgs([]string{"--dry-run", "--yes"})
	err := cmd.Execute()
	if err != nil && strings.Contains(err.Error(), "requires -e") {
		t.Fatalf("single-target destroy must not require -e: %v", err)
	}
	// Missing cluster credentials (or any later check) is fine; the point is
	// we did not stop at the environment gate.
}
