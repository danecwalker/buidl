// Package ui renders progress and results for three very different consumers: a
// human at an interactive terminal, a CI log, and a machine parsing JSON.
//
// The same call sites serve all three. Code elsewhere in buidl never checks
// whether it is running in CI — it calls ui.Step/Info/Warn and this package
// decides how that should look.
package ui

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Mode selects the output format.
type Mode string

const (
	// ModeAuto picks Pretty on a TTY and Plain otherwise.
	ModeAuto Mode = "auto"
	// ModePretty uses color, symbols and timing.
	ModePretty Mode = "pretty"
	// ModePlain is line-oriented with no escape codes: correct for CI logs and
	// for piping to a file.
	ModePlain Mode = "plain"
	// ModeJSON emits one JSON event per line for programmatic consumers.
	ModeJSON Mode = "json"
)

// Level classifies an event.
type Level string

const (
	LevelStep    Level = "step"
	LevelInfo    Level = "info"
	LevelWarn    Level = "warn"
	LevelError   Level = "error"
	LevelSuccess Level = "success"
	LevelDebug   Level = "debug"
	LevelResult  Level = "result"
)

// Event is the machine-readable form of every message. In ModeJSON these are
// written verbatim, one per line, which is a stable contract for CI scripts.
type Event struct {
	Time    time.Time      `json:"time"`
	Level   Level          `json:"level"`
	Message string         `json:"message,omitempty"`
	Step    string         `json:"step,omitempty"`
	Fields  map[string]any `json:"fields,omitempty"`
}

// Printer renders events. It is safe for concurrent use.
type Printer struct {
	mu      sync.Mutex
	out     io.Writer
	errOut  io.Writer
	mode    Mode
	color   bool
	verbose bool

	// ci describes the detected CI provider, used for log folding.
	ci CI

	step      string
	stepStart time.Time
	// stepStatus, stepDetail and stepErr accumulate the outcome of the current
	// step until it closes.
	stepStatus StepStatus
	stepDetail string
	stepErr    error
	// steps is the completed record of the run, rendered by Summary.
	steps []StepRecord

	// spin animates long steps on an interactive terminal.
	spin spinner

	// progressBucket and progressAt throttle Progress in non-TTY output.
	progressBucket int
	progressAt     time.Time

	// failed records whether any error was emitted, so the process can exit
	// non-zero even when an error was recovered and reported.
	failed bool
}

// Options configures a Printer.
type Options struct {
	Out     io.Writer
	ErrOut  io.Writer
	Mode    Mode
	Verbose bool
	// NoColor forces color off regardless of TTY detection.
	NoColor bool
}

// New builds a Printer, resolving ModeAuto against the environment.
func New(opts Options) *Printer {
	out := opts.Out
	if out == nil {
		out = os.Stdout
	}
	errOut := opts.ErrOut
	if errOut == nil {
		errOut = os.Stderr
	}

	ci := DetectCI()
	mode := opts.Mode
	if mode == "" {
		mode = ModeAuto
	}
	if mode == ModeAuto {
		// In CI, escape codes and cursor tricks make logs unreadable and break
		// log-scraping, so plain is the right default even though the runner may
		// present a pseudo-TTY.
		if ci.Detected || !isTerminal(out) {
			mode = ModePlain
		} else {
			mode = ModePretty
		}
	}

	color := mode == ModePretty && !opts.NoColor && os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb"

	return &Printer{
		out:            out,
		errOut:         errOut,
		mode:           mode,
		color:          color,
		verbose:        opts.Verbose,
		ci:             ci,
		progressBucket: -1,
	}
}

// Mode reports the resolved output mode.
func (p *Printer) Mode() Mode { return p.mode }

// CI reports the detected CI provider.
func (p *Printer) CI() CI { return p.ci }

// Failed reports whether any error was emitted.
func (p *Printer) Failed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.failed
}

// Step begins a named phase. In CI providers that support it, phases become
// collapsible log groups, which keeps a deploy log skimmable.
func (p *Printer) Step(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.endStepLocked()
	p.step = name
	p.stepStart = time.Now()
	p.stepStatus = StepRunning
	p.stepDetail = ""
	p.stepErr = nil

	switch p.mode {
	case ModeJSON:
		p.emitLocked(Event{Level: LevelStep, Step: name})
	case ModePretty:
		fmt.Fprintf(p.out, "%s %s\n", p.paint("▸", cyan, bold), p.paint(name, bold))
		// Several steps are slow and silent; without this a live run is
		// indistinguishable from a hung one.
		p.startSpinnerLocked(name)
	default:
		if open := p.ci.GroupStart(name); open != "" {
			fmt.Fprintln(p.out, open)
		} else {
			fmt.Fprintf(p.out, "==> %s\n", name)
		}
	}
}

// EndStep closes the current phase, if any.
func (p *Printer) EndStep() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.endStepLocked()
}

func (p *Printer) endStepLocked() {
	if p.step == "" {
		return
	}

	p.stopSpinnerLocked()

	status := p.stepStatus
	if status == StepRunning || status == "" {
		// A step that closed without an explicit outcome succeeded; failures are
		// recorded via FailStep or by an Error emitted during the step.
		status = StepOK
	}
	p.steps = append(p.steps, StepRecord{
		Name:     p.step,
		Status:   status,
		Duration: time.Since(p.stepStart),
		Detail:   p.stepDetail,
		Err:      p.stepErr,
	})

	if p.mode == ModePlain {
		if close := p.ci.GroupEnd(); close != "" {
			fmt.Fprintln(p.out, close)
		}
	}

	p.step = ""
	p.stepStatus = ""
	p.stepDetail = ""
	p.stepErr = nil
}

// Info reports normal progress.
func (p *Printer) Info(format string, args ...any) {
	p.event(LevelInfo, fmt.Sprintf(format, args...), nil)
}

// Detail reports secondary progress, shown only with --verbose.
func (p *Printer) Detail(format string, args ...any) {
	if !p.verbose {
		return
	}
	p.event(LevelDebug, fmt.Sprintf(format, args...), nil)
}

// Success reports a completed operation.
func (p *Printer) Success(format string, args ...any) {
	p.event(LevelSuccess, fmt.Sprintf(format, args...), nil)
}

// Warn reports a recoverable problem. In CI this becomes a provider annotation
// so it surfaces in the run summary rather than being buried in the log.
func (p *Printer) Warn(format string, args ...any) {
	p.event(LevelWarn, fmt.Sprintf(format, args...), nil)
}

// Error reports a failure and marks the printer failed.
func (p *Printer) Error(format string, args ...any) {
	p.mu.Lock()
	p.failed = true
	// An error emitted inside a step is that step's outcome, so the closing
	// summary reports the failure against the phase it happened in.
	if p.step != "" && p.stepStatus == StepRunning {
		p.stepStatus = StepFailed
	}
	p.mu.Unlock()
	p.event(LevelError, fmt.Sprintf(format, args...), nil)
}

// Fields attaches structured data to an info event. Useful in ModeJSON, where a
// CI script may want the digest or release ID without parsing prose.
func (p *Printer) Fields(msg string, fields map[string]any) {
	p.event(LevelResult, msg, fields)
}

func (p *Printer) event(level Level, msg string, fields map[string]any) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.mode == ModeJSON {
		p.emitLocked(Event{Level: level, Message: msg, Step: p.step, Fields: fields})
		return
	}

	p.clearSpinnerLocked()

	w := p.out
	if level == LevelError || level == LevelWarn {
		w = p.errOut
	}

	// CI annotations make warnings and errors visible in the run summary UI.
	if p.mode == ModePlain {
		if ann := p.ci.Annotate(level, msg); ann != "" {
			fmt.Fprintln(w, ann)
			return
		}
	}

	fmt.Fprintf(w, "%s%s\n", p.prefix(level), msg)
	for _, line := range sortedFieldLines(fields) {
		fmt.Fprintf(w, "%s%s\n", p.prefix(LevelDebug), line)
	}
}

func (p *Printer) emitLocked(e Event) {
	e.Time = time.Now().UTC()
	enc := json.NewEncoder(p.out)
	// Deploy logs are read by humans too; keep events on one line each.
	enc.SetEscapeHTML(false)
	_ = enc.Encode(e)
}

func (p *Printer) prefix(level Level) string {
	switch p.mode {
	case ModePretty:
		switch level {
		case LevelInfo:
			return "  "
		case LevelSuccess:
			return "  " + p.paint("✓", green) + " "
		case LevelWarn:
			return "  " + p.paint("!", yellow) + " "
		case LevelError:
			return "  " + p.paint("✗", red) + " "
		case LevelDebug:
			return "  " + p.paint("·", dim) + " "
		}
		return "  "
	default:
		switch level {
		case LevelSuccess:
			return "    ok: "
		case LevelWarn:
			return "  warn: "
		case LevelError:
			return " error: "
		case LevelDebug:
			return " debug: "
		}
		return "    "
	}
}

// Table renders aligned columns, or a JSON array in ModeJSON.
func (p *Printer) Table(headers []string, rows [][]string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.mode == ModeJSON {
		objs := make([]map[string]string, 0, len(rows))
		for _, r := range rows {
			obj := map[string]string{}
			for i, h := range headers {
				if i < len(r) {
					obj[strings.ToLower(h)] = r[i]
				}
			}
			objs = append(objs, obj)
		}
		// Emitted as an event rather than a bare array so that every line in JSON
		// mode decodes into the same type. A consumer doing line-by-line
		// json.Unmarshal into an Event must never hit a differently-shaped line.
		p.emitLocked(Event{
			Level:  LevelResult,
			Step:   p.step,
			Fields: map[string]any{"rows": objs},
		})
		return
	}

	p.clearSpinnerLocked()

	// Widths are measured in runes, not bytes. Cells routinely contain multi-byte
	// characters (the "→" in a field change, "…" from truncation), and byte
	// lengths would under-pad those cells and skew every column after them.
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = displayWidth(h)
	}
	for _, row := range rows {
		for i, cell := range row {
			if i < len(widths) && displayWidth(cell) > widths[i] {
				widths[i] = displayWidth(cell)
			}
		}
	}

	// Tables share the indent of ordinary lines so a report reads as one block.
	indent := p.prefix(LevelInfo)

	var b strings.Builder
	for i, h := range headers {
		b.WriteString(pad(strings.ToUpper(h), widths[i]))
		if i < len(headers)-1 {
			b.WriteString("  ")
		}
	}
	fmt.Fprintln(p.out, indent+p.paint(strings.TrimRight(b.String(), " "), dim))

	for _, row := range rows {
		var line strings.Builder
		for i := range headers {
			cell := ""
			if i < len(row) {
				cell = row[i]
			}
			line.WriteString(pad(cell, widths[i]))
			if i < len(headers)-1 {
				line.WriteString("  ")
			}
		}
		fmt.Fprintln(p.out, indent+strings.TrimRight(line.String(), " "))
	}
}

// displayWidth counts the printable width of a cell.
//
// Rune count rather than byte length, so multi-byte characters do not skew column
// alignment.
func displayWidth(s string) int {
	return utf8.RuneCountInString(s)
}

// Raw writes a pre-formatted block (for example streamed container logs)
// untouched, except that JSON mode wraps each line as an event.
func (p *Printer) Raw(line string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.mode == ModeJSON {
		p.emitLocked(Event{Level: LevelInfo, Message: line, Step: p.step})
		return
	}
	p.clearSpinnerLocked()
	fmt.Fprintln(p.out, line)
}

func pad(s string, w int) string {
	width := displayWidth(s)
	if width >= w {
		return s
	}
	return s + strings.Repeat(" ", w-width)
}

func sortedFieldLines(fields map[string]any) []string {
	if len(fields) == 0 {
		return nil
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	// Stable output matters for golden-file tests and for diffing CI logs.
	sortStrings(keys)
	lines := make([]string, 0, len(keys))
	for _, k := range keys {
		lines = append(lines, fmt.Sprintf("%s: %v", k, fields[k]))
	}
	return lines
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
