package ui

import (
	"fmt"
	"time"
)

// Progress draws a download bar. total < 0 means the server omitted
// Content-Length.
//
// Pretty terminals rewrite one line in place. Everywhere else (CI, a
// redirected file) gets a few discrete lines so a 50MB download does not
// emit thousands of progress events.
func (p *Printer) Progress(done, total int64) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.mode == ModeJSON {
		return
	}

	now := time.Now()
	if p.mode == ModePretty && isTerminal(p.out) {
		if done < total && now.Sub(p.progressAt) < 50*time.Millisecond {
			return
		}
		p.progressAt = now
		p.stopSpinnerLocked()
		fmt.Fprintf(p.out, "\r\033[K  %s", p.formatProgress(done, total))
		p.spin.drawn = true
		return
	}

	if !p.shouldPrintPlainProgress(done, total, now) {
		return
	}
	p.progressAt = now
	p.clearSpinnerLocked()
	fmt.Fprintf(p.out, "%s%s\n", p.prefix(LevelDebug), p.formatProgress(done, total))
}

func (p *Printer) shouldPrintPlainProgress(done, total int64, now time.Time) bool {
	if total > 0 {
		pct := int(float64(done) / float64(total) * 100)
		if pct > 100 {
			pct = 100
		}
		bucket := pct / 25
		if bucket == p.progressBucket && pct != 100 {
			return false
		}
		p.progressBucket = bucket
		return true
	}
	return p.progressAt.IsZero() || now.Sub(p.progressAt) >= 2*time.Second || done == 0
}

func (p *Printer) formatProgress(done, total int64) string {
	const width = 24
	if total <= 0 {
		return fmt.Sprintf("%s downloaded", formatBytes(done))
	}
	frac := float64(done) / float64(total)
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	filled := int(frac * width)
	bar := p.paint(repeatRune('█', filled)+repeatRune('░', width-filled), cyan)
	pct := int(frac * 100)
	return fmt.Sprintf("%s  %s / %s  %d%%", bar, formatBytes(done), formatBytes(total), pct)
}

func formatBytes(n int64) string {
	const kb = 1024
	switch {
	case n >= kb*kb:
		return fmt.Sprintf("%.1f MB", float64(n)/float64(kb*kb))
	case n >= kb:
		return fmt.Sprintf("%.1f KB", float64(n)/float64(kb))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func repeatRune(r rune, n int) string {
	if n <= 0 {
		return ""
	}
	b := make([]rune, n)
	for i := range b {
		b[i] = r
	}
	return string(b)
}
