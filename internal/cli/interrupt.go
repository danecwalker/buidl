package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// interruptConfirmWindow is how long the second Ctrl+C must arrive after the
// first to count as confirmation. A later press is a new first press.
const interruptConfirmWindow = 5 * time.Second

// 128 + SIGINT. The conventional status for a process killed from the terminal.
const interruptExitCode = 130

// 128 + SIGTERM. Used when the process is asked to stop by the OS or CI.
const termExitCode = 143

// interruptWatch implements the two-press Ctrl+C confirmation.
//
// The first SIGINT only prints a prompt. A second SIGINT inside window cancels
// the context and force-exits: BuildKit and SSH often ignore a cancelled
// context, so a confirmed interrupt has to actually kill the process.
// SIGTERM is not confirmed; CI and kill send it once and expect an exit.
type interruptWatch struct {
	window time.Duration
	now    func() time.Time
	notify func(string)
	exit   func(code int)
}

func interruptConfirmMessage(window time.Duration) string {
	return fmt.Sprintf("interrupt: press Ctrl+C again in the next %s to exit", window.Round(time.Second))
}

func (w interruptWatch) run(parent context.Context, sigs <-chan os.Signal) (context.Context, context.CancelFunc) {
	if w.now == nil {
		w.now = time.Now
	}
	if w.window <= 0 {
		w.window = interruptConfirmWindow
	}

	ctx, cancel := context.WithCancel(parent)
	go func() {
		var first time.Time
		for {
			select {
			case <-ctx.Done():
				return
			case sig, ok := <-sigs:
				if !ok {
					return
				}
				if sig == syscall.SIGTERM {
					cancel()
					if w.exit != nil {
						w.exit(termExitCode)
					}
					return
				}

				now := w.now()
				if first.IsZero() || now.Sub(first) > w.window {
					first = now
					if w.notify != nil {
						w.notify(interruptConfirmMessage(w.window))
					}
					continue
				}

				cancel()
				if w.exit != nil {
					w.exit(interruptExitCode)
				}
				return
			}
		}
	}()
	return ctx, cancel
}

// context returns a command context that times out and that treats Ctrl+C as a
// confirmed interrupt. See interruptWatch.
func (a *App) context() (context.Context, context.CancelFunc) {
	ctx, timeoutCancel := context.WithTimeout(context.Background(), a.opts.timeout)

	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)

	w := interruptWatch{
		window: interruptConfirmWindow,
		now:    time.Now,
		notify: func(msg string) {
			if a.log != nil {
				a.log.Warn("%s", msg)
			} else {
				fmt.Fprintln(os.Stderr, msg)
			}
		},
		exit: func(code int) { os.Exit(code) },
	}
	ctx, watchCancel := w.run(ctx, sigs)

	return ctx, func() {
		signal.Stop(sigs)
		watchCancel()
		timeoutCancel()
	}
}
