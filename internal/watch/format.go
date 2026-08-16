package watch

import (
	"fmt"
	"time"
)

// FormatCPU renders millicores the way kubectl top does (45m, 1.2).
func FormatCPU(u Usage) string {
	if !u.Known {
		return "—"
	}
	return formatMilli(u.CPUMilli)
}

// FormatMemory renders bytes as Ki/Mi/Gi.
func FormatMemory(u Usage) string {
	if !u.Known {
		return "—"
	}
	return formatBytes(u.Memory)
}

// FormatNodeCPU is used/allocatable, or "—" when the used sample is missing.
func FormatNodeCPU(n Node) string {
	if n.CPUAlloc <= 0 && !n.Usage.Known {
		return "—"
	}
	used := "—"
	if n.Usage.Known {
		used = formatMilli(n.Usage.CPUMilli)
	}
	if n.CPUAlloc <= 0 {
		return used
	}
	return used + "/" + formatMilli(n.CPUAlloc)
}

// FormatNodeMemory is used/allocatable.
func FormatNodeMemory(n Node) string {
	if n.MemAlloc <= 0 && !n.Usage.Known {
		return "—"
	}
	used := "—"
	if n.Usage.Known {
		used = formatBytes(n.Usage.Memory)
	}
	if n.MemAlloc <= 0 {
		return used
	}
	return used + "/" + formatBytes(n.MemAlloc)
}

// FormatAge is a compact duration for tables (12s, 4m, 3h, 2d).
func FormatAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dD", int(d.Hours()/24))
	}
}

// FormatUptime is FormatAge, or "—" when the process has not started.
func FormatUptime(started time.Time, uptime time.Duration) string {
	if started.IsZero() {
		return "—"
	}
	return FormatAge(uptime)
}

// FormatReady is "2/2".
func FormatReady(ready, desired int32) string {
	return fmt.Sprintf("%d/%d", ready, desired)
}

func formatMilli(milli int64) string {
	if milli < 0 {
		milli = 0
	}
	if milli < 1000 {
		return fmt.Sprintf("%dm", milli)
	}
	if milli%1000 == 0 {
		return fmt.Sprintf("%d", milli/1000)
	}
	return fmt.Sprintf("%.1f", float64(milli)/1000)
}

func formatBytes(b int64) string {
	if b < 0 {
		b = 0
	}
	const (
		ki = 1024
		mi = 1024 * ki
		gi = 1024 * mi
		ti = 1024 * gi
	)
	switch {
	case b >= ti:
		return trimFrac(float64(b)/float64(ti)) + "Ti"
	case b >= gi:
		return trimFrac(float64(b)/float64(gi)) + "Gi"
	case b >= mi:
		return trimFrac(float64(b)/float64(mi)) + "Mi"
	case b >= ki:
		return fmt.Sprintf("%dKi", b/ki)
	default:
		return fmt.Sprintf("%dB", b)
	}
}

func trimFrac(v float64) string {
	if v >= 10 || v == float64(int64(v)) {
		return fmt.Sprintf("%.0f", v)
	}
	return fmt.Sprintf("%.1f", v)
}
