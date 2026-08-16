package watch

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	attrReset  = "\033[0m"
	attrBold   = "\033[1m"
	attrDim    = "\033[2m"
	attrRed    = "\033[31m"
	attrGreen  = "\033[32m"
	attrYellow = "\033[33m"
	attrCyan   = "\033[36m"
	attrInvert = "\033[7m"
)

// View is one frame of the dashboard, interactive or a single print.
type View struct {
	Snapshot    Snapshot
	History     History
	Selected    int
	Interval    time.Duration
	Now         time.Time
	Width       int
	Height      int
	Color       bool
	Interactive bool
	Err         string
}

// Render paints the dashboard as lines joined by \n, with no trailing newline
// on the last line so an alt-screen write does not scroll.
func Render(v View) string {
	if v.Now.IsZero() {
		v.Now = time.Now()
	}
	if v.Width <= 0 {
		v.Width = 100
	}
	v.Selected = ClampSelected(v.Selected, len(v.Snapshot.Apps))

	var lines []string
	lines = append(lines, v.header()...)
	if v.Snapshot.Time.IsZero() && v.Err == "" && len(v.Snapshot.Apps) == 0 {
		lines = append(lines, v.box(v.paint("buidl watch", attrBold, attrCyan), []string{
			v.paint("  loading…", attrDim),
		}, v.Width, attrCyan)...)
		lines = append(lines, v.footer()...)
		return strings.Join(clipLines(lines, v.Width), "\n")
	}
	if v.Err != "" {
		lines = append(lines, v.box("error", []string{"  " + oneLine(v.Err)}, v.Width, attrRed)...)
	}
	if alerts := v.Snapshot.Alerts; len(alerts) > 0 {
		body := make([]string, 0, len(alerts))
		for _, a := range alerts {
			code := attrYellow
			if a.Level == "crit" {
				code = attrRed
			}
			body = append(body, "  "+v.paint(a.Text, code))
		}
		border := attrYellow
		for _, a := range alerts {
			if a.Level == "crit" {
				border = attrRed
				break
			}
		}
		lines = append(lines, v.box("alerts", body, v.Width, border)...)
	}

	var top []string
	if len(v.Snapshot.Nodes) > 0 && v.Width >= 72 {
		lw := v.Width / 2
		rw := v.Width - lw - 1
		sb := v.stackBody(lw)
		cb := v.clusterBody(rw)
		for len(sb) < len(cb) {
			sb = append(sb, "")
		}
		for len(cb) < len(sb) {
			cb = append(cb, "")
		}
		top = zipCols(v.box("stack", sb, lw, attrCyan), v.box("cluster", cb, rw, attrBlue), lw, rw)
	} else {
		top = append(top, v.stackCard()...)
		if c := v.clusterCard(); len(c) > 0 {
			top = append(top, c...)
		}
	}

	apps := v.appCards()
	footer := v.footer()

	if v.Height > 0 {
		reserved := len(footer) + 1
		room := v.Height - reserved
		if room < 8 {
			room = 8
		}
		used := len(lines)
		if used+len(top)+len(apps) > room && len(v.Snapshot.Nodes) > 0 && v.Width >= 72 {
			// Side-by-side cluster is the first thing to drop; apps stay.
			top = v.stackCard()
		}
		if used+len(top)+len(apps) > room {
			apps = v.compactAppCards()
		}
		lines = append(lines, top...)
		if len(lines)+len(apps) > room {
			apps = trimLines(apps, room-len(lines))
		}
		lines = append(lines, apps...)
		if len(lines) > room {
			lines = lines[:room]
		}
	} else {
		lines = append(lines, top...)
		lines = append(lines, apps...)
	}
	lines = append(lines, footer...)
	return strings.Join(clipLines(lines, v.Width), "\n")
}

func (v View) header() []string {
	s := v.Snapshot
	title := "buidl watch"
	meta := joinMeta(s.Stack, s.Environment, "ns/"+orDash(s.Namespace), s.Context)
	ago := ""
	if !s.Time.IsZero() {
		ago = "updated " + FormatAge(v.Now.Sub(s.Time)) + " ago"
	}
	metrics := metricsLine(s)

	left := v.paint(title, attrBold, attrCyan) + "  " + v.paint(meta, attrDim)
	right := v.paint(strings.TrimSpace(ago+"  "+metrics), attrDim)
	row := fitRow(left, right, v.Width, v.Color)
	if ago != "" && !strings.Contains(stripANSI(row), "updated") {
		return []string{left, v.paint(strings.TrimSpace(ago+"  "+metrics), attrDim)}
	}
	return []string{row}
}

func metricsLine(s Snapshot) string {
	switch s.Metrics {
	case MetricsOK:
		return "metrics-server ok"
	case MetricsDenied:
		return "metrics-server: not authorized"
	case MetricsError:
		if s.MetricsErr != "" {
			return "metrics-server: " + oneLine(s.MetricsErr)
		}
		return "metrics-server: error"
	case MetricsMissing:
		return "metrics-server missing — RAM/CPU show —"
	default:
		return ""
	}
}

func (v View) stackCard() []string {
	return v.box("stack", v.stackBody(v.Width), v.Width, attrCyan)
}

func (v View) stackBody(width int) []string {
	s := v.Snapshot
	var healthy, degraded, missing, ready, desired int32
	var uptime time.Duration
	var started time.Time
	var cpu, mem Usage
	for _, app := range s.Apps {
		ready += app.Ready
		desired += app.Desired
		cpu = AddUsage(cpu, app.Usage)
		mem = AddUsage(mem, app.Usage)
		switch app.Health {
		case HealthHealthy:
			healthy++
		case HealthDegraded:
			degraded++
		case HealthMissing:
			missing++
		}
		if !app.StartedAt.IsZero() && (started.IsZero() || app.StartedAt.Before(started)) {
			started = app.StartedAt
			uptime = app.Uptime
		}
	}
	status := fmt.Sprintf("%s %d healthy", v.dot(HealthHealthy), healthy)
	if degraded > 0 {
		status += "  " + fmt.Sprintf("%s %d degraded", v.dot(HealthDegraded), degraded)
	}
	if missing > 0 {
		status += "  " + fmt.Sprintf("%s %d missing", v.dot(HealthMissing), missing)
	}
	sparkW := sparkRoom(width, 14) // " CPU  " + 6-wide value + "  "
	return []string{
		" " + status,
		fmt.Sprintf(" %d apps   %s ready   up %s", len(s.Apps), FormatReady(ready, desired), FormatUptime(started, uptime)),
		" CPU  " + padVisible(FormatCPU(cpu), 6) + "  " + v.paint(sparkline(v.History.stackCPU(), sparkW), attrCyan),
		" RAM  " + padVisible(FormatMemory(mem), 6) + "  " + v.paint(sparkline(v.History.stackMem(), sparkW), attrMagenta),
	}
}

func (v View) clusterCard() []string {
	body := v.clusterBody(v.Width)
	if body == nil {
		return nil
	}
	return v.box("cluster", body, v.Width, attrBlue)
}

func (v View) clusterBody(width int) []string {
	nodes := v.Snapshot.Nodes
	if len(nodes) == 0 {
		return nil
	}
	body := make([]string, 0, len(nodes)*3)
	// " CPU  " + 10-wide value + " " + bar + "  " + spark
	barW, sparkW := splitGauge(width, 17)
	for _, n := range nodes {
		ready := "yes"
		if !n.Ready {
			ready = "no"
		}
		if !n.Schedulable {
			ready += ", unschedulable"
		}
		label := n.Name
		if n.Roles != "" {
			label += "  " + n.Roles
		}
		cpuFrac := barFrac(n.Usage.CPUMilli, n.CPUAlloc)
		memFrac := barFrac(n.Usage.Memory, n.MemAlloc)
		body = append(body, " "+label+"  "+ready+"  "+FormatAge(n.Age))
		cpuLine := " CPU  " + padVisible(FormatNodeCPU(n), 10) + " " + v.paint(bar(cpuFrac, barW), barAttr(cpuFrac))
		memLine := " RAM  " + padVisible(FormatNodeMemory(n), 10) + " " + v.paint(bar(memFrac, barW), barAttr(memFrac))
		if n.Usage.Known && sparkW > 0 {
			cpuLine += "  " + v.paint(sparkline(v.History.nodeCPU(n.Name), sparkW), attrCyan)
			memLine += "  " + v.paint(sparkline(v.History.nodeMem(n.Name), sparkW), attrMagenta)
		}
		body = append(body, cpuLine, memLine)
		if n.Message != "" && !n.Ready {
			body = append(body, v.paint("  "+oneLine(n.Message), attrDim))
		}
	}
	return body
}

func (v View) appCards() []string {
	var out []string
	for i, app := range v.Snapshot.Apps {
		out = append(out, v.appCard(app, i == v.Selected, false)...)
	}
	return out
}

func (v View) compactAppCards() []string {
	var out []string
	for i, app := range v.Snapshot.Apps {
		out = append(out, v.appCard(app, i == v.Selected, true)...)
	}
	return out
}

func (v View) appCard(app App, selected, compact bool) []string {
	mark := " "
	if v.Interactive && selected {
		mark = "▸"
	}
	title := strings.TrimSpace(mark + " " + app.Name)
	if app.Type != "" {
		title += "  " + v.paint(app.Type, attrDim)
	}
	title += "  " + v.dot(app.Health) + " " + v.paint(app.Health, healthAttr(app.Health))
	title += "  " + FormatReady(app.Ready, app.Desired)
	if app.Release != "" {
		title += "  " + v.paint(app.Release, attrDim)
	}
	if !app.StartedAt.IsZero() {
		title += "  up " + FormatUptime(app.StartedAt, app.Uptime)
	}

	sparkW := sparkRoom(v.Width, 14) // " CPU  " + 6-wide value + "  "
	cpu := " CPU  " + padVisible(FormatCPU(app.Usage), 6) + "  " + v.paint(sparkline(v.History.appCPU(app.Name), sparkW), attrCyan)
	ram := " RAM  " + padVisible(FormatMemory(app.Usage), 6) + "  " + v.paint(sparkline(v.History.appMem(app.Name), sparkW), attrMagenta)
	body := []string{cpu, ram}

	if !compact && selected {
		if app.URL != "" {
			body = append(body, v.paint(" "+app.URL, attrDim))
		}
		body = append(body, v.instanceLines(app, v.Width-4)...)
		if app.Restarts > 0 {
			body = append(body, v.paint(fmt.Sprintf(" restarts %d", app.Restarts), attrYellow))
		}
	}

	border := attrDim
	if selected && v.Interactive {
		border = attrCyan
	}
	if app.Health == HealthDegraded {
		border = attrRed
	}
	return v.box(title, body, v.Width, border)
}

func (v View) instanceLines(app App, width int) []string {
	if len(app.Instances) == 0 {
		msg := "  (none)"
		if app.Health == HealthMissing {
			msg = "  not deployed"
		}
		return []string{v.paint(msg, attrDim)}
	}
	// name | phase | ready | ram | up | node — drop from the right on a squeeze.
	type row struct {
		name, phase, ready, ram, up, node string
	}
	rows := make([]row, len(app.Instances))
	for i, inst := range app.Instances {
		ready := "no"
		if inst.Ready {
			ready = "yes"
		}
		rows[i] = row{
			name:  inst.Name,
			phase: orDash(inst.Phase),
			ready: ready,
			ram:   FormatMemory(inst.Usage),
			up:    FormatUptime(inst.StartedAt, inst.Uptime),
			node:  orDash(inst.Node),
		}
	}
	cols := []struct {
		head  string
		width int
		drop  bool
		cell  func(row) string
	}{
		{"instance", 0, false, func(r row) string { return r.name }},
		{"phase", 0, true, func(r row) string { return r.phase }},
		{"ready", 0, false, func(r row) string { return r.ready }},
		{"ram", 0, false, func(r row) string { return r.ram }},
		{"up", 0, false, func(r row) string { return r.up }},
		{"node", 0, true, func(r row) string { return r.node }},
	}
	for i := range cols {
		w := utf8.RuneCountInString(cols[i].head)
		for _, r := range rows {
			if n := utf8.RuneCountInString(cols[i].cell(r)); n > w {
				w = n
			}
		}
		cols[i].width = w
	}
	total := func() int {
		n := 0
		for _, c := range cols {
			n += c.width + 2
		}
		return n
	}
	for total() > width {
		dropped := false
		for i := len(cols) - 1; i >= 0; i-- {
			if cols[i].drop {
				cols = append(cols[:i], cols[i+1:]...)
				dropped = true
				break
			}
		}
		if !dropped {
			// Shrink the name column.
			over := total() - width
			if cols[0].width-over < 8 {
				cols[0].width = 8
			} else {
				cols[0].width -= over
			}
			break
		}
	}
	head := make([]string, len(cols))
	for i, c := range cols {
		head[i] = padCell(c.head, c.width)
	}
	out := []string{v.paint(" "+strings.Join(head, "  "), attrDim)}
	for i, r := range rows {
		cells := make([]string, len(cols))
		for j, c := range cols {
			val := c.cell(r)
			if j == 0 {
				val = truncateRunes(val, c.width)
			}
			cells[j] = padCell(val, c.width)
		}
		line := " " + strings.Join(cells, "  ")
		if !app.Instances[i].Ready {
			line = v.paint(line, attrYellow)
		}
		out = append(out, line)
		if msg := app.Instances[i].Message; msg != "" && !app.Instances[i].Ready {
			out = append(out, v.paint("  "+app.Instances[i].Name+": "+oneLine(msg), attrDim))
		}
	}
	return out
}

func (v View) footer() []string {
	if !v.Interactive {
		if v.Snapshot.Metrics == MetricsMissing {
			return []string{v.paint("RAM and CPU need metrics-server. k3s bundles it unless disabled.", attrDim)}
		}
		return nil
	}
	help := "q quit   r refresh   j/k select"
	if v.Interval > 0 {
		help += fmt.Sprintf("   every %s", v.Interval.Round(time.Millisecond))
	}
	return []string{v.paint(help, attrDim)}
}

func (v View) dot(health string) string {
	ch := "●"
	switch health {
	case HealthMissing, HealthStopped:
		ch = "○"
	}
	return v.paint(ch, healthAttr(health))
}

func (v View) paint(s string, attrs ...string) string {
	if !v.Color || s == "" || len(attrs) == 0 {
		return s
	}
	return strings.Join(attrs, "") + s + attrReset
}

func healthAttr(h string) string {
	switch h {
	case HealthHealthy:
		return attrGreen
	case HealthDegraded:
		return attrRed
	case HealthRollout:
		return attrYellow
	case HealthStopped, HealthMissing:
		return attrDim
	default:
		return ""
	}
}

func padCell(s string, w int) string {
	n := utf8.RuneCountInString(s)
	if n >= w {
		return s
	}
	return s + strings.Repeat(" ", w-n)
}

func fitRow(left, right string, width int, color bool) string {
	plainLeft := stripANSI(left)
	plainRight := stripANSI(right)
	if width <= 0 {
		return left + "  " + right
	}
	gap := width - utf8.RuneCountInString(plainLeft) - utf8.RuneCountInString(plainRight)
	if gap < 2 {
		if !color {
			return truncateRunes(plainLeft, width)
		}
		return left
	}
	return left + strings.Repeat(" ", gap) + right
}

func stripANSI(s string) string {
	var b strings.Builder
	i := 0
	for i < len(s) {
		if s[i] == '\033' {
			if j := strings.IndexByte(s[i:], 'm'); j >= 0 {
				i += j + 1
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// clipLines keeps every row inside the terminal width. A wrap in raw
// mode would put the next write on the following line and smear the frame.
func clipLines(lines []string, width int) []string {
	if width <= 0 {
		return lines
	}
	out := make([]string, len(lines))
	for i, line := range lines {
		if utf8.RuneCountInString(stripANSI(line)) <= width {
			out[i] = line
			continue
		}
		out[i] = truncateRunes(stripANSI(line), width)
	}
	return out
}

func truncateRunes(s string, n int) string {
	if n <= 0 || utf8.RuneCountInString(s) <= n {
		return s
	}
	runes := []rune(s)
	if n == 1 {
		return string(runes[:1])
	}
	return string(runes[:n-1]) + "…"
}

func joinMeta(parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "  ·  ")
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 160 {
		return s[:157] + "..."
	}
	return s
}

func trimLines(lines []string, n int) []string {
	if n <= 0 || len(lines) <= n {
		if n <= 0 {
			return nil
		}
		return lines
	}
	return lines[:n]
}
