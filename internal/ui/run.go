package ui

import (
	"fmt"
	"strings"
	"time"
)

// StepStatus is the outcome of one phase of a run.
type StepStatus string

const (
	StepRunning StepStatus = "running"
	StepOK      StepStatus = "ok"
	StepFailed  StepStatus = "failed"
	StepSkipped StepStatus = "skipped"
)

// StepRecord is one phase of a run, with how long it took and how it ended.
//
// Recording these is what lets a run end with an honest account of itself: which
// phases ran, which were skipped, which failed, and where the time went. A deploy
// that reports only "success" leaves the user guessing whether the slow part was
// the build, the image push, or waiting on health checks.
type StepRecord struct {
	Name     string
	Status   StepStatus
	Duration time.Duration
	// Detail carries a short outcome note, e.g. "5 objects applied".
	Detail string
	// Err is the failure, when Status is StepFailed.
	Err error
}

// Steps returns the recorded phases of this run, in order.
func (p *Printer) Steps() []StepRecord {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]StepRecord, len(p.steps))
	copy(out, p.steps)
	return out
}

// StepDetail annotates the current step with a short outcome note, shown in the
// closing summary.
func (p *Printer) StepDetail(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stepDetail = fmt.Sprintf(format, args...)
}

// FailStep closes the current step as failed.
//
// The error is recorded rather than printed here, because the caller normally
// returns it and the top level reports it once. Printing at both layers would
// double every failure.
func (p *Printer) FailStep(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stepStatus = StepFailed
	p.stepErr = err
	p.endStepLocked()
}

// SkipStep closes the current step as skipped, with a reason.
func (p *Printer) SkipStep(reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stepStatus = StepSkipped
	p.stepDetail = reason
	p.endStepLocked()
}

// Summary renders a closing account of the run: every step, its outcome, and its
// duration.
//
// In JSON mode this becomes a single structured event so a CI job can assert on
// the shape of a deploy rather than scraping prose.
func (p *Printer) Summary(title string) {
	p.mu.Lock()
	// Close any step still open, so its duration is accounted for.
	p.endStepLocked()
	steps := make([]StepRecord, len(p.steps))
	copy(steps, p.steps)
	mode := p.mode
	p.mu.Unlock()

	if len(steps) == 0 {
		return
	}

	if mode == ModeJSON {
		items := make([]map[string]any, 0, len(steps))
		var total time.Duration
		for _, s := range steps {
			total += s.Duration
			item := map[string]any{
				"step":        s.Name,
				"status":      string(s.Status),
				"duration_ms": s.Duration.Milliseconds(),
			}
			if s.Detail != "" {
				item["detail"] = s.Detail
			}
			if s.Err != nil {
				item["error"] = s.Err.Error()
			}
			items = append(items, item)
		}
		p.Fields(title, map[string]any{
			"steps":          items,
			"total_duration": total.Round(time.Millisecond).String(),
		})
		return
	}

	rows := make([][]string, 0, len(steps))
	var total time.Duration
	for _, s := range steps {
		total += s.Duration
		rows = append(rows, []string{
			statusMark(s.Status),
			s.Name,
			formatDuration(s.Duration),
			s.Detail,
		})
	}

	p.Info("")
	p.Info("%s", title)
	p.Table([]string{"", "step", "took", "detail"}, rows)
	p.Info("total %s", formatDuration(total))
}

// statusMark renders a step status as a compact marker.
//
// Plain text rather than color, because this must stay legible in a CI log, in a
// pasted terminal capture, and in a file.
func statusMark(status StepStatus) string {
	switch status {
	case StepOK:
		return "ok"
	case StepFailed:
		return "FAIL"
	case StepSkipped:
		return "skip"
	default:
		return "..."
	}
}

// formatDuration renders a duration at a precision that suits its magnitude.
//
// Sub-second precision matters for a fast apply; on a four-minute build,
// milliseconds are noise.
func formatDuration(d time.Duration) string {
	switch {
	case d == 0:
		return "-"
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	default:
		m := int(d.Minutes())
		s := int(d.Seconds()) - m*60
		return fmt.Sprintf("%dm%02ds", m, s)
	}
}

// Result renders the terminal outcome of a run as a single prominent line.
func (p *Printer) Result(ok bool, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if ok {
		p.Success("%s", msg)
		return
	}
	p.Error("%s", msg)
}

// Bullets prints a labelled list, used for reporting sets of objects or hosts.
func (p *Printer) Bullets(label string, items []string) {
	if len(items) == 0 {
		return
	}
	p.Info("%s:", label)
	for _, item := range items {
		p.Info("  - %s", item)
	}
}

// KeyValues prints aligned key/value pairs, for a facts block.
//
// Takes ordered pairs rather than a map so output order is meaningful rather than
// whatever the runtime chooses.
func (p *Printer) KeyValues(pairs [][2]string) {
	width := 0
	for _, kv := range pairs {
		if len(kv[0]) > width {
			width = len(kv[0])
		}
	}
	for _, kv := range pairs {
		if kv[1] == "" {
			continue
		}
		p.Info("%s  %s", pad(kv[0], width), kv[1])
	}
}

// Indented prints a block of pre-formatted text with a prefix, for embedded
// output such as a diff or container logs.
func (p *Printer) Indented(prefix, block string) {
	for _, line := range strings.Split(strings.TrimRight(block, "\n"), "\n") {
		p.Info("%s%s", prefix, line)
	}
}
