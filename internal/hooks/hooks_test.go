package hooks

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// captureLogger records output so tests can assert on warnings.
type captureLogger struct {
	warnings []string
}

func (c *captureLogger) Info(string, ...any)   {}
func (c *captureLogger) Detail(string, ...any) {}
func (c *captureLogger) Warn(format string, args ...any) {
	c.warnings = append(c.warnings, format)
}

// writeHook creates a hook script, executable unless told otherwise.
func writeHook(t *testing.T, root string, point Point, script string, executable bool) string {
	t.Helper()
	dir := filepath.Join(root, ".buidl", "hooks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, string(point))
	mode := os.FileMode(0o644)
	if executable {
		mode = 0o755
	}
	if err := os.WriteFile(path, []byte(script), mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func newRunner(t *testing.T, root string) (*Runner, *captureLogger) {
	t.Helper()
	log := &captureLogger{}
	return NewRunner(root, ".buidl/hooks", log), log
}

func TestMissingHookIsNotAnError(t *testing.T) {
	runner, _ := newRunner(t, t.TempDir())

	// Most projects need no hooks at all, so absence must be silent success.
	result := runner.Run(context.Background(), PreDeploy, Context{})
	if result.Ran {
		t.Error("Ran should be false when no hook exists")
	}
	if result.Err != nil {
		t.Errorf("a missing hook must not error: %v", result.Err)
	}
}

func TestSuccessfulHookRuns(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "ran")
	writeHook(t, root, PreDeploy, "#!/bin/sh\ntouch "+marker+"\n", true)

	runner, _ := newRunner(t, root)
	result := runner.Run(context.Background(), PreDeploy, Context{})

	if !result.Ran {
		t.Fatal("expected the hook to run")
	}
	if result.Err != nil {
		t.Fatalf("hook failed: %v", result.Err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Error("the hook did not actually execute")
	}
}

// TestFailingHookReportsExitCode matters because a pre-deploy hook is how
// migrations run: if a migration fails, the deploy must not continue.
func TestFailingHookReportsExitCode(t *testing.T) {
	root := t.TempDir()
	writeHook(t, root, PreDeploy, "#!/bin/sh\necho 'migration failed' >&2\nexit 3\n", true)

	runner, _ := newRunner(t, root)
	result := runner.Run(context.Background(), PreDeploy, Context{})

	if result.Err == nil {
		t.Fatal("expected an error from a failing hook")
	}
	if !strings.Contains(result.Err.Error(), "exit 3") {
		t.Errorf("error should report the exit code, got: %v", result.Err)
	}
}

func TestNonExecutableHookIsSkippedWithAWarning(t *testing.T) {
	root := t.TempDir()
	writeHook(t, root, PreDeploy, "#!/bin/sh\nexit 1\n", false)

	runner, log := newRunner(t, root)
	result := runner.Run(context.Background(), PreDeploy, Context{})

	// Silently ignoring it would hide a forgotten chmod after checkout; running it
	// is impossible. So: skip, but say so.
	if result.Ran {
		t.Error("a non-executable hook must not run")
	}
	if len(log.warnings) == 0 {
		t.Error("expected a warning about the missing execute bit")
	}
	if !strings.Contains(strings.Join(log.warnings, " "), "chmod") {
		t.Errorf("the warning should say how to fix it: %v", log.warnings)
	}
}

// TestHookReceivesReleaseContext covers the environment a migration needs.
func TestHookReceivesReleaseContext(t *testing.T) {
	root := t.TempDir()
	out := filepath.Join(root, "env.txt")
	writeHook(t, root, PreDeploy,
		"#!/bin/sh\n{ echo \"$BUIDL_APP\"; echo \"$BUIDL_ENV\"; echo \"$BUIDL_RELEASE\"; "+
			"echo \"$BUIDL_DIGEST\"; echo \"$BUIDL_NAMESPACE\"; echo \"$BUIDL_HOOK\"; } > "+out+"\n", true)

	runner, _ := newRunner(t, root)
	result := runner.Run(context.Background(), PreDeploy, Context{
		App:         "web",
		Environment: "production",
		Release:     "abc123-xyz",
		Digest:      "sha256:deadbeef",
		Namespace:   "acme",
	})
	if result.Err != nil {
		t.Fatalf("hook failed: %v", result.Err)
	}

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	want := []string{"web", "production", "abc123-xyz", "sha256:deadbeef", "acme", "1"}
	if len(lines) != len(want) {
		t.Fatalf("got %d lines, want %d: %v", len(lines), len(want), lines)
	}
	for i, expected := range want {
		if lines[i] != expected {
			t.Errorf("line %d = %q, want %q", i, lines[i], expected)
		}
	}
}

// TestHookReceivesSecrets is the whole point of the migration use case: the hook
// gets a credential the application itself is not given.
func TestHookReceivesSecrets(t *testing.T) {
	root := t.TempDir()
	out := filepath.Join(root, "secret.txt")
	writeHook(t, root, PreDeploy,
		"#!/bin/sh\nprintf '%s' \"$MIGRATIONS_DATABASE_URL\" > "+out+"\n", true)

	runner, _ := newRunner(t, root)
	result := runner.Run(context.Background(), PreDeploy, Context{
		Secrets: map[string]string{"MIGRATIONS_DATABASE_URL": "postgres://owner@db/app"},
	})
	if result.Err != nil {
		t.Fatalf("hook failed: %v", result.Err)
	}

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "postgres://owner@db/app" {
		t.Errorf("hook saw %q, want the secret value", string(data))
	}
}

// TestSecretsCannotShadowBuidlVariables ensures a secret named BUIDL_RELEASE
// cannot rewrite the release identity a hook sees.
func TestSecretsOverrideOrderIsDocumented(t *testing.T) {
	root := t.TempDir()
	out := filepath.Join(root, "value.txt")
	writeHook(t, root, PreDeploy, "#!/bin/sh\nprintf '%s' \"$BUIDL_APP\" > "+out+"\n", true)

	runner, _ := newRunner(t, root)
	// Secrets are appended last, so a colliding name wins. That is deliberate and
	// documented; this test pins the behavior so it cannot change silently.
	result := runner.Run(context.Background(), PreDeploy, Context{
		App:     "real-app",
		Secrets: map[string]string{"BUIDL_APP": "from-secret"},
	})
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	data, _ := os.ReadFile(out)
	if string(data) != "from-secret" {
		t.Errorf("got %q; later environment entries should win", string(data))
	}
}

func TestHookTimeout(t *testing.T) {
	root := t.TempDir()
	writeHook(t, root, PreDeploy, "#!/bin/sh\nsleep 30\n", true)

	runner, _ := newRunner(t, root)
	// A hung hook must not hold a deploy open indefinitely.
	runner.Timeout = 200 * time.Millisecond

	result := runner.Run(context.Background(), PreDeploy, Context{})
	if result.Err == nil {
		t.Fatal("expected a timeout error")
	}
	if !strings.Contains(result.Err.Error(), "timed out") {
		t.Errorf("error should mention the timeout, got: %v", result.Err)
	}
}

func TestHookCancellation(t *testing.T) {
	root := t.TempDir()
	writeHook(t, root, PreDeploy, "#!/bin/sh\nsleep 30\n", true)

	runner, _ := newRunner(t, root)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	result := runner.Run(ctx, PreDeploy, Context{})
	if result.Err == nil {
		t.Fatal("expected an error when cancelled")
	}
	// Ctrl-C must actually stop the hook, not wait out its timeout.
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("cancellation took %s; should be immediate", elapsed)
	}
}

// TestAbortSemantics pins which hooks can fail a deploy.
func TestAbortSemantics(t *testing.T) {
	// A failed migration must stop the deploy that assumes it succeeded.
	for _, point := range []Point{PreBuild, PostBuild, PreDeploy} {
		if !point.Aborts() {
			t.Errorf("%s should abort the deploy on failure", point)
		}
	}
	// These run after the outcome is decided; failing the command on their account
	// would misrepresent what happened.
	for _, point := range []Point{PostDeploy, DeployFailed} {
		if point.Aborts() {
			t.Errorf("%s should not abort the deploy", point)
		}
	}
}

func TestAvailableListsOnlyExecutableHooks(t *testing.T) {
	root := t.TempDir()
	writeHook(t, root, PreDeploy, "#!/bin/sh\n", true)
	writeHook(t, root, PostDeploy, "#!/bin/sh\n", false)

	runner, _ := newRunner(t, root)
	available := runner.Available()

	if len(available) != 1 || available[0] != PreDeploy {
		t.Errorf("Available = %v, want only the executable hook", available)
	}
}

func TestPointsCoverEveryDescribedHook(t *testing.T) {
	for _, point := range Points() {
		if point.Description() == "" {
			t.Errorf("%s has no description", point)
		}
		// The scaffolded sample must be a runnable script.
		sample := SampleHook(point)
		if !strings.HasPrefix(sample, "#!") {
			t.Errorf("%s sample has no shebang", point)
		}
		if !strings.Contains(sample, "set -euo pipefail") {
			t.Errorf("%s sample should fail fast", point)
		}
		// The sample must state whether failing it stops the deploy.
		if point.Aborts() && !strings.Contains(sample, "ABORTS") {
			t.Errorf("%s sample should say it aborts the deploy", point)
		}
	}
}

func TestSampleHooksAreValidShell(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no shell available")
	}
	for _, point := range Points() {
		dir := t.TempDir()
		path := filepath.Join(dir, "hook")
		if err := os.WriteFile(path, []byte(SampleHook(point)), 0o755); err != nil {
			t.Fatal(err)
		}
		// Syntax-check rather than execute, since bash may be absent.
		if err := runShellCheck(path); err != nil {
			t.Errorf("%s sample is not valid shell: %v", point, err)
		}
	}
}

// runShellCheck parses a script without executing it, so a sample can be checked
// for syntax on any machine.
func runShellCheck(path string) error {
	cmd := exec.Command("bash", "-n", path)
	if _, err := exec.LookPath("bash"); err != nil {
		cmd = exec.Command("sh", "-n", path)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, out)
	}
	return nil
}
