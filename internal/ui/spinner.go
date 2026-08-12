package ui

import (
	"fmt"
	"time"
)

// spinnerFrames is a braille cycle: compact, monospaced, and legible in any
// terminal font that renders box drawing.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// spinnerInterval is fast enough to read as motion without being distracting.
const spinnerInterval = 100 * time.Millisecond

// spinner shows that a long step is still running.
//
// Several steps are legitimately slow and silent — installing a distribution
// takes minutes, waiting on a rollout can too — and a still cursor is
// indistinguishable from a hang. Showing elapsed time answers the real question,
// which is not "is it alive" but "how long has this been going".
//
// It runs only on an interactive terminal. In a CI log or a redirected file,
// carriage returns and escape codes produce thousands of lines of garbage, so
// there the periodic progress messages are the whole story.
type spinner struct {
	// active is whether the ticker goroutine is running.
	active bool
	// drawn is whether a spinner line is currently on screen and needs clearing
	// before anything else is written.
	drawn bool
	frame int
	label string
	start time.Time
	stop  chan struct{}
}

// spinnerEnabled reports whether this printer should animate.
func (p *Printer) spinnerEnabled() bool {
	return p.mode == ModePretty && isTerminal(p.out)
}

// startSpinner begins animating for a step. The caller must hold the lock.
func (p *Printer) startSpinnerLocked(label string) {
	if !p.spinnerEnabled() {
		return
	}
	p.stopSpinnerLocked()

	p.spin.active = true
	p.spin.label = label
	p.spin.start = time.Now()
	p.spin.frame = 0
	p.spin.stop = make(chan struct{})

	stop := p.spin.stop
	go func() {
		ticker := time.NewTicker(spinnerInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				p.tickSpinner()
			}
		}
	}()
}

// stopSpinnerLocked halts the animation and clears its line. The caller must
// hold the lock.
func (p *Printer) stopSpinnerLocked() {
	if !p.spin.active {
		return
	}
	close(p.spin.stop)
	p.spin.active = false
	p.clearSpinnerLocked()
}

// clearSpinnerLocked erases the spinner line if one is on screen.
//
// Every other write goes through this first, so real output never lands on top
// of a partially drawn spinner.
func (p *Printer) clearSpinnerLocked() {
	if !p.spin.drawn {
		return
	}
	// Carriage return, then erase to end of line.
	fmt.Fprint(p.out, "\r\033[K")
	p.spin.drawn = false
}

// tickSpinner redraws the spinner line.
func (p *Printer) tickSpinner() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.spin.active {
		return
	}

	p.spin.frame = (p.spin.frame + 1) % len(spinnerFrames)
	elapsed := time.Since(p.spin.start)

	// Elapsed time only appears once the step has run long enough to be worth
	// worrying about; showing "0s" on every fast step is noise.
	suffix := ""
	if elapsed >= 2*time.Second {
		suffix = "  " + formatDuration(elapsed)
	}

	fmt.Fprintf(p.out, "\r\033[K%s %s%s",
		p.paint(spinnerFrames[p.spin.frame], cyan),
		p.spin.label,
		p.paint(suffix, dim))
	p.spin.drawn = true
}
