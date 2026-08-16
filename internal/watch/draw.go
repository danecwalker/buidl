package watch

import (
	"math"
	"strings"
	"unicode/utf8"
)

const (
	attrMagenta = "\033[35m"
	attrBlue    = "\033[34m"
)

var sparkRunes = []rune(" ▁▂▃▄▅▆▇█")

// sparkline maps the last `width` samples onto block characters, scaled
// to the max known value in that window. Unknown samples are a gap.
func sparkline(samples []sample, width int) string {
	if width <= 0 {
		return ""
	}
	if len(samples) == 0 {
		return strings.Repeat("·", width)
	}
	if len(samples) > width {
		samples = samples[len(samples)-width:]
	}
	var max int64
	any := false
	for _, s := range samples {
		if s.known {
			any = true
			if s.v > max {
				max = s.v
			}
		}
	}
	if !any {
		return strings.Repeat("·", width)
	}
	if max <= 0 {
		max = 1
	}
	var b strings.Builder
	pad := width - len(samples)
	for i := 0; i < pad; i++ {
		b.WriteRune('·')
	}
	levels := int64(len(sparkRunes) - 1)
	for _, s := range samples {
		if !s.known {
			b.WriteRune('·')
			continue
		}
		n := s.v * levels / max
		if n < 0 {
			n = 0
		}
		if n > levels {
			n = levels
		}
		// A known zero still ticks the baseline so a quiet series is not empty.
		if s.v == 0 {
			n = 1
		}
		if n == 0 {
			n = 1
		}
		b.WriteRune(sparkRunes[n])
	}
	return b.String()
}

// bar is a horizontal gauge. frac is 0..1; values outside are clamped.
func bar(frac float64, width int) string {
	if width <= 0 {
		return ""
	}
	if math.IsNaN(frac) || frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	filled := int(math.Round(frac * float64(width)))
	if filled > width {
		filled = width
	}
	return strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
}

func barFrac(used, total int64) float64 {
	if total <= 0 {
		return 0
	}
	return float64(used) / float64(total)
}

func barAttr(frac float64) string {
	switch {
	case frac >= 0.9:
		return attrRed
	case frac >= 0.7:
		return attrYellow
	default:
		return attrGreen
	}
}

// sparkRoom is how many spark glyphs fit after a prefix in a boxed panel.
func sparkRoom(panelW, prefix int) int {
	w := panelW - 2 - prefix
	if w < 0 {
		return 0
	}
	return w
}

// splitGauge shares leftover inner width between a bar and a sparkline.
func splitGauge(panelW, prefix int) (barW, sparkW int) {
	remain := panelW - 2 - prefix
	if remain <= 4 {
		if remain < 0 {
			remain = 0
		}
		return remain, 0
	}
	barW = remain / 2
	if barW > 12 {
		barW = 12
	}
	sparkW = remain - barW - 2
	if sparkW < 0 {
		sparkW = 0
		barW = remain
	}
	return barW, sparkW
}

func visibleLen(s string) int {
	return utf8.RuneCountInString(stripANSI(s))
}

func padVisible(s string, width int) string {
	n := visibleLen(s)
	if n == width {
		return s
	}
	if n > width {
		return truncateRunes(stripANSI(s), width)
	}
	return s + strings.Repeat(" ", width-n)
}

// box frames body in a titled unicode panel. title may contain ANSI;
// the border uses attr when Color is on.
func (v View) box(title string, body []string, width int, attr string) []string {
	if width < 8 {
		width = 8
	}
	inner := width - 2
	top := v.boxTop(title, width, attr)
	side := v.paint("│", attr)
	bot := v.paint("└"+strings.Repeat("─", inner)+"┘", attr)
	out := make([]string, 0, len(body)+2)
	out = append(out, top)
	for _, line := range body {
		out = append(out, side+padVisible(line, inner)+side)
	}
	out = append(out, bot)
	return out
}

func (v View) boxTop(title string, width int, attr string) string {
	if title == "" {
		return v.paint("┌"+strings.Repeat("─", width-2)+"┐", attr)
	}
	plain := stripANSI(title)
	maxTitle := width - 6
	if maxTitle < 1 {
		maxTitle = 1
	}
	if utf8.RuneCountInString(plain) > maxTitle {
		title = truncateRunes(plain, maxTitle)
		plain = title
	}
	rest := width - 5 - utf8.RuneCountInString(plain)
	if rest < 1 {
		rest = 1
	}
	left := v.paint("┌─", attr)
	mid := v.paint("─", attr)
	right := v.paint("┐", attr)
	return left + " " + title + " " + strings.Repeat(mid, rest) + right
}

// zipCols places two already-rendered panels side by side. leftW + 1 + rightW
// should equal total. Shorter side is padded with blank rows.
func zipCols(left, right []string, leftW, rightW int) []string {
	n := len(left)
	if len(right) > n {
		n = len(right)
	}
	out := make([]string, n)
	blankL := strings.Repeat(" ", leftW)
	blankR := strings.Repeat(" ", rightW)
	for i := 0; i < n; i++ {
		l, r := blankL, blankR
		if i < len(left) {
			l = padVisible(left[i], leftW)
		}
		if i < len(right) {
			r = padVisible(right[i], rightW)
		}
		out[i] = l + " " + r
	}
	return out
}
