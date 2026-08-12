package ui

import (
	"io"
	"os"
	"strings"
)

// ANSI attributes used by pretty output. Kept deliberately small: a deploy tool
// should be legible on any terminal theme, so we only use the six base colors
// plus bold/dim rather than a 256-color palette.
type attr string

const (
	reset  attr = "\033[0m"
	bold   attr = "\033[1m"
	dim    attr = "\033[2m"
	red    attr = "\033[31m"
	green  attr = "\033[32m"
	yellow attr = "\033[33m"
	blue   attr = "\033[34m"
	cyan   attr = "\033[36m"
)

// paint wraps s in the given attributes when color is enabled, and returns it
// untouched otherwise. Centralizing the check here means no call site has to
// branch on whether color is active.
func (p *Printer) paint(s string, attrs ...attr) string {
	if !p.color || len(attrs) == 0 || s == "" {
		return s
	}
	var b strings.Builder
	for _, a := range attrs {
		b.WriteString(string(a))
	}
	b.WriteString(s)
	b.WriteString(string(reset))
	return b.String()
}

// isTerminal reports whether w is an interactive terminal.
//
// This uses a stat-based check rather than an ioctl so the package stays
// dependency-free and cross-platform: a character device that is not /dev/null
// is a terminal for our purposes. The consequence of a false positive is only
// cosmetic (escape codes in a log), and ModeAuto already forces plain output
// whenever CI is detected, which covers the realistic failure case.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	if info.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	// /dev/null is a character device but is not interactive.
	return info.Name() != os.DevNull
}
