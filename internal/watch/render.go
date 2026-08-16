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
		lines = append(lines, v.paint("  loading…", attrDim))
		lines = append(lines, v.footer()...)
		return strings.Join(clipLines(lines, v.Width), "\n")
	}
	if v.Err != "" {
		lines = append(lines, v.paint("  "+oneLine(v.Err), attrRed))
		lines = append(lines, "")
	}
	if alerts := v.Snapshot.Alerts; len(alerts) > 0 {
		lines = append(lines, v.paint("ALERTS", attrBold))
		for _, a := range alerts {
			code := attrYellow
			if a.Level == "crit" {
				code = attrRed
			}
			lines = append(lines, "  "+v.paint(a.Text, code))
		}
		lines = append(lines, "")
	}

	lines = append(lines, v.appsTable()...)
	lines = append(lines, "")
	lines = append(lines, v.instancesTable()...)
	lines = append(lines, "")
	cluster := v.clusterTable()
	footer := v.footer()

	if v.Height > 0 {
		// Footer stays pinned. Cluster is the first thing to drop when the
		// terminal is short — the apps and their instances are why you opened this.
		reserved := len(footer) + 1
		room := v.Height - reserved
		if room < 8 {
			room = 8
		}
		if len(lines)+len(cluster) > room {
			cluster = nil
		}
		if len(lines) > room {
			lines = lines[:room]
		}
	}
	lines = append(lines, cluster...)
	if len(cluster) > 0 {
		lines = append(lines, "")
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
	return []string{fitRow(left, right, v.Width, v.Color), ""}
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

func (v View) appsTable() []string {
	apps := v.Snapshot.Apps
	if len(apps) == 0 {
		return []string{v.paint("APPS", attrBold), "  (none)"}
	}
	mark := make([]string, len(apps))
	names := make([]string, len(apps))
	types := make([]string, len(apps))
	healths := make([]string, len(apps))
	readys := make([]string, len(apps))
	cpus := make([]string, len(apps))
	rams := make([]string, len(apps))
	uptimes := make([]string, len(apps))
	restarts := make([]string, len(apps))
	releases := make([]string, len(apps))
	for i, app := range apps {
		mark[i] = " "
		if v.Interactive && i == v.Selected {
			mark[i] = "▸"
		}
		names[i] = app.Name
		types[i] = orDash(app.Type)
		healths[i] = app.Health
		readys[i] = FormatReady(app.Ready, app.Desired)
		cpus[i] = FormatCPU(app.Usage)
		rams[i] = FormatMemory(app.Usage)
		uptimes[i] = FormatUptime(app.StartedAt, app.Uptime)
		restarts[i] = fmt.Sprintf("%d", app.Restarts)
		releases[i] = orDash(app.Release)
	}
	// Drop type/release/restarts/cpu before RAM and uptime — those two are
	// why this command exists.
	return v.table("APPS", []column{
		{header: "", cells: mark},
		{header: "app", cells: names},
		{header: "type", cells: types, drop: 4},
		{header: "health", cells: healths},
		{header: "ready", cells: readys},
		{header: "cpu", cells: cpus, drop: 1},
		{header: "ram", cells: rams},
		{header: "uptime", cells: uptimes},
		{header: "restarts", cells: restarts, drop: 2},
		{header: "release", cells: releases, drop: 3},
	}, func(row int, header, cell string) string {
		if row < 0 || row >= len(apps) {
			return cell
		}
		if header == "health" {
			return v.paint(cell, healthAttr(apps[row].Health))
		}
		if v.Interactive && row == v.Selected {
			return v.paint(cell, attrCyan)
		}
		return cell
	})
}

func (v View) instancesTable() []string {
	app := v.Snapshot.Selected(v.Selected)
	title := "INSTANCES"
	if app.Name != "" {
		title += "  " + app.Name
	}
	if app.URL != "" {
		title += "  " + app.URL
	}
	if len(app.Instances) == 0 {
		msg := "  (none)"
		if app.Health == HealthMissing {
			msg = "  not deployed"
		}
		return []string{v.paint(title, attrBold), msg}
	}
	n := len(app.Instances)
	names := make([]string, n)
	phases := make([]string, n)
	readys := make([]string, n)
	cpus := make([]string, n)
	rams := make([]string, n)
	uptimes := make([]string, n)
	restarts := make([]string, n)
	nodes := make([]string, n)
	for i, inst := range app.Instances {
		names[i] = inst.Name
		phases[i] = orDash(inst.Phase)
		if inst.Ready {
			readys[i] = "yes"
		} else {
			readys[i] = "no"
		}
		cpus[i] = FormatCPU(inst.Usage)
		rams[i] = FormatMemory(inst.Usage)
		uptimes[i] = FormatUptime(inst.StartedAt, inst.Uptime)
		restarts[i] = fmt.Sprintf("%d", inst.Restarts)
		nodes[i] = orDash(inst.Node)
	}
	out := v.table(title, []column{
		{header: "instance", cells: names},
		{header: "phase", cells: phases, drop: 1},
		{header: "ready", cells: readys},
		{header: "cpu", cells: cpus, drop: 2},
		{header: "ram", cells: rams},
		{header: "uptime", cells: uptimes},
		{header: "restarts", cells: restarts, drop: 3},
		{header: "node", cells: nodes, drop: 4},
	}, func(row int, header, cell string) string {
		if row < 0 || row >= n {
			return cell
		}
		if !app.Instances[row].Ready {
			return v.paint(cell, attrYellow)
		}
		return cell
	})
	for _, inst := range app.Instances {
		if inst.Message != "" && !inst.Ready {
			out = append(out, v.paint("  "+inst.Name+": "+oneLine(inst.Message), attrDim))
		}
	}
	return out
}

func (v View) clusterTable() []string {
	if len(v.Snapshot.Nodes) == 0 {
		return nil
	}
	nodes := v.Snapshot.Nodes
	n := len(nodes)
	names := make([]string, n)
	readys := make([]string, n)
	cpus := make([]string, n)
	rams := make([]string, n)
	uptimes := make([]string, n)
	roles := make([]string, n)
	for i, node := range nodes {
		names[i] = node.Name
		readys[i] = "no"
		if node.Ready {
			readys[i] = "yes"
		}
		if !node.Schedulable {
			readys[i] += ", unschedulable"
		}
		cpus[i] = FormatNodeCPU(node)
		rams[i] = FormatNodeMemory(node)
		uptimes[i] = FormatAge(node.Age)
		roles[i] = orDash(node.Roles)
	}
	return v.table("CLUSTER", []column{
		{header: "node", cells: names},
		{header: "ready", cells: readys},
		{header: "cpu", cells: cpus},
		{header: "ram", cells: rams},
		{header: "uptime", cells: uptimes},
		{header: "roles", cells: roles, drop: 1},
	}, func(row int, header, cell string) string {
		if row < 0 || row >= n {
			return cell
		}
		if !nodes[row].Ready {
			return v.paint(cell, attrRed)
		}
		return cell
	})
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

type column struct {
	header string
	cells  []string
	// drop is how soon this column is sacrificed on a narrow terminal.
	// 0 means keep it until nothing else can go.
	drop int
}

func (v View) table(title string, cols []column, color func(row int, header, cell string) string) []string {
	cols = fitColumns(cols, v.Width)
	cols = shrinkColumns(cols, v.Width)
	widths := make([]int, len(cols))
	rows := 0
	for i, c := range cols {
		widths[i] = utf8.RuneCountInString(c.header)
		if len(c.cells) > rows {
			rows = len(c.cells)
		}
		for _, cell := range c.cells {
			if n := utf8.RuneCountInString(cell); n > widths[i] {
				widths[i] = n
			}
		}
	}

	headers := make([]string, len(cols))
	for i, c := range cols {
		headers[i] = c.header
	}

	var out []string
	out = append(out, v.paint(title, attrBold))
	out = append(out, "  "+v.paint(joinCells(headers, widths, true), attrDim))
	for r := 0; r < rows; r++ {
		cells := make([]string, len(cols))
		for i, c := range cols {
			cell := ""
			if r < len(c.cells) {
				cell = c.cells[r]
			}
			padded := padCell(cell, widths[i])
			if color != nil {
				padded = color(r, c.header, padded)
			}
			cells[i] = padded
		}
		out = append(out, "  "+strings.TrimRight(strings.Join(cells, "  "), " "))
	}
	return out
}

func fitColumns(cols []column, width int) []column {
	if width <= 0 {
		return cols
	}
	for {
		widths := make([]int, len(cols))
		for i, c := range cols {
			widths[i] = utf8.RuneCountInString(c.header)
			for _, cell := range c.cells {
				if n := utf8.RuneCountInString(cell); n > widths[i] {
					widths[i] = n
				}
			}
		}
		if totalWidth(widths)+2 <= width || !dropOneColumn(&cols) {
			return cols
		}
	}
}

// shrinkColumns trims the widest remaining column when dropping columns
// still leaves the table wider than the terminal. Pod names and dirty
// release ids are the usual offenders.
func shrinkColumns(cols []column, width int) []column {
	if width <= 0 || len(cols) == 0 {
		return cols
	}
	for {
		widths := columnWidths(cols)
		extra := totalWidth(widths) - width
		if extra <= 0 {
			return cols
		}
		widest := 0
		for i, w := range widths {
			if w > widths[widest] {
				widest = i
			}
		}
		target := widths[widest] - extra
		if target < 4 {
			target = 4
		}
		if target >= widths[widest] {
			return cols
		}
		cols[widest].header = truncateRunes(cols[widest].header, target)
		for i, cell := range cols[widest].cells {
			cols[widest].cells[i] = truncateRunes(cell, target)
		}
	}
}

func columnWidths(cols []column) []int {
	widths := make([]int, len(cols))
	for i, c := range cols {
		widths[i] = utf8.RuneCountInString(c.header)
		for _, cell := range c.cells {
			if n := utf8.RuneCountInString(cell); n > widths[i] {
				widths[i] = n
			}
		}
	}
	return widths
}

func dropOneColumn(cols *[]column) bool {
	best := -1
	bestPri := 0
	for i, c := range *cols {
		if c.drop > bestPri {
			bestPri = c.drop
			best = i
		}
	}
	if best < 0 {
		return false
	}
	*cols = append((*cols)[:best], (*cols)[best+1:]...)
	return true
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

func joinCells(cells []string, widths []int, upper bool) string {
	parts := make([]string, 0, len(cells))
	for i, c := range cells {
		if upper {
			c = strings.ToUpper(c)
		}
		w := 0
		if i < len(widths) {
			w = widths[i]
		}
		parts = append(parts, padCell(c, w))
	}
	return strings.TrimRight(strings.Join(parts, "  "), " ")
}

func padCell(s string, w int) string {
	n := utf8.RuneCountInString(s)
	if n >= w {
		return s
	}
	return s + strings.Repeat(" ", w-n)
}

func totalWidth(widths []int) int {
	n := 2
	for i, w := range widths {
		n += w
		if i < len(widths)-1 {
			n += 2
		}
	}
	return n
}

func fitRow(left, right string, width int, color bool) string {
	plainLeft := stripANSI(left)
	plainRight := stripANSI(right)
	if width <= 0 {
		return left + "  " + right
	}
	gap := width - utf8.RuneCountInString(plainLeft) - utf8.RuneCountInString(plainRight)
	if gap < 2 {
		// Prefer the identity line; the timestamp can wrap on a tiny terminal.
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
