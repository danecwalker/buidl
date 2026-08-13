package cli

import (
	"context"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"
)

type interruptRecord struct {
	mu       sync.Mutex
	notifies []string
	code     int
}

func (r *interruptRecord) notify(msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.notifies = append(r.notifies, msg)
}

func (r *interruptRecord) exit(code int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.code = code
}

func (r *interruptRecord) snapshot() (notifies []string, code int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.notifies...), r.code
}

func waitUntil(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for interrupt watch")
}

func newWatch(t *testing.T, now *time.Time) (sigs chan os.Signal, rec *interruptRecord, ctx context.Context) {
	t.Helper()
	*now = time.Unix(1_700_000_000, 0)
	rec = &interruptRecord{code: -1}
	sigs = make(chan os.Signal, 4)
	w := interruptWatch{
		window: 3 * time.Second,
		now:    func() time.Time { return *now },
		notify: rec.notify,
		exit:   rec.exit,
	}
	var cancel context.CancelFunc
	ctx, cancel = w.run(context.Background(), sigs)
	t.Cleanup(cancel)
	return sigs, rec, ctx
}

func TestInterruptFirstSignalDoesNotExit(t *testing.T) {
	var now time.Time
	sigs, rec, ctx := newWatch(t, &now)

	sigs <- os.Interrupt
	waitUntil(t, func() bool {
		notes, _ := rec.snapshot()
		return len(notes) == 1
	})

	if ctx.Err() != nil {
		t.Fatalf("first Ctrl+C cancelled the context: %v", ctx.Err())
	}
	notes, code := rec.snapshot()
	if code != -1 {
		t.Fatalf("first Ctrl+C exited with %d", code)
	}
	want := interruptConfirmMessage(3 * time.Second)
	if notes[0] != want {
		t.Fatalf("prompt = %q, want %q", notes[0], want)
	}
}

func TestInterruptSecondSignalWithinWindowExits(t *testing.T) {
	var now time.Time
	sigs, rec, ctx := newWatch(t, &now)

	sigs <- os.Interrupt
	waitUntil(t, func() bool {
		notes, _ := rec.snapshot()
		return len(notes) == 1
	})

	now = now.Add(time.Second)
	sigs <- os.Interrupt
	waitUntil(t, func() bool { return ctx.Err() != nil })

	_, code := rec.snapshot()
	if code != interruptExitCode {
		t.Fatalf("exit code = %d, want %d", code, interruptExitCode)
	}
}

func TestInterruptWindowExpiryIsANewFirstPress(t *testing.T) {
	var now time.Time
	sigs, rec, ctx := newWatch(t, &now)

	sigs <- os.Interrupt
	waitUntil(t, func() bool {
		notes, _ := rec.snapshot()
		return len(notes) == 1
	})

	now = now.Add(4 * time.Second)
	sigs <- os.Interrupt
	waitUntil(t, func() bool {
		notes, _ := rec.snapshot()
		return len(notes) == 2
	})

	if ctx.Err() != nil {
		t.Fatalf("press after the window cancelled the context: %v", ctx.Err())
	}
	_, code := rec.snapshot()
	if code != -1 {
		t.Fatalf("press after the window exited with %d", code)
	}

	now = now.Add(time.Second)
	sigs <- os.Interrupt
	waitUntil(t, func() bool { return ctx.Err() != nil })

	_, code = rec.snapshot()
	if code != interruptExitCode {
		t.Fatalf("confirmed exit code = %d, want %d", code, interruptExitCode)
	}
}

func TestSIGTERMExitsWithoutConfirmation(t *testing.T) {
	var now time.Time
	sigs, rec, ctx := newWatch(t, &now)

	sigs <- syscall.SIGTERM
	waitUntil(t, func() bool { return ctx.Err() != nil })

	notes, code := rec.snapshot()
	if len(notes) != 0 {
		t.Fatalf("SIGTERM printed a confirmation prompt: %v", notes)
	}
	if code != termExitCode {
		t.Fatalf("exit code = %d, want %d", code, termExitCode)
	}
}
