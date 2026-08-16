// Package watch is the live operational view of a stack.
//
// status is a one-shot report. watch is the thing you leave up: health,
// memory, CPU, uptime, restarts, and node pressure, refreshed in place.
package watch

import "time"

// MetricsState is whether metrics.k8s.io answered.
type MetricsState string

const (
	// MetricsOK means the API is present. Individual rows may still lack a sample.
	MetricsOK MetricsState = "ok"
	// MetricsMissing means metrics-server is not installed or not discovered.
	MetricsMissing MetricsState = "missing"
	// MetricsDenied means the credentials cannot read PodMetrics / NodeMetrics.
	MetricsDenied MetricsState = "denied"
	// MetricsError means the API returned an unexpected error.
	MetricsError MetricsState = "error"
)

// Health is a row-level summary. Keep the vocabulary small so a glance works.
const (
	HealthHealthy  = "healthy"
	HealthDegraded = "degraded"
	HealthRollout  = "rollout"
	HealthStopped  = "stopped"
	HealthMissing  = "missing"
)

// Snapshot is one observation of the stack and the nodes it runs on.
type Snapshot struct {
	Time        time.Time    `json:"time"`
	Stack       string       `json:"stack"`
	Environment string       `json:"environment"`
	Namespace   string       `json:"namespace"`
	Context     string       `json:"context"`
	Server      string       `json:"server,omitempty"`
	Metrics     MetricsState `json:"metrics"`
	MetricsErr  string       `json:"metrics_error,omitempty"`
	Apps        []App        `json:"apps"`
	Nodes       []Node       `json:"nodes"`
	Alerts      []Alert      `json:"alerts,omitempty"`
}

// App is one process or stateful member of the stack.
type App struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Health   string `json:"health"`
	Ready    int32  `json:"ready"`
	Desired  int32  `json:"desired"`
	Restarts int32  `json:"restarts"`
	Release  string `json:"release,omitempty"`
	Image    string `json:"image,omitempty"`
	URL      string `json:"url,omitempty"`
	GitSHA   string `json:"git_sha,omitempty"`
	// Uptime is how long the oldest ready instance has been running.
	// Zero means nothing is up.
	Uptime     time.Duration `json:"uptime_ns,omitempty"`
	StartedAt  time.Time     `json:"started_at,omitempty"`
	Usage      Usage         `json:"usage"`
	Instances  []Instance    `json:"instances,omitempty"`
	Releases   int           `json:"releases,omitempty"`
	Conditions []string      `json:"conditions,omitempty"`
}

// Instance is one Pod.
type Instance struct {
	Name      string        `json:"name"`
	Phase     string        `json:"phase"`
	Ready     bool          `json:"ready"`
	Restarts  int32         `json:"restarts"`
	Age       time.Duration `json:"age_ns,omitempty"`
	Uptime    time.Duration `json:"uptime_ns,omitempty"`
	StartedAt time.Time     `json:"started_at,omitempty"`
	Node      string        `json:"node,omitempty"`
	Release   string        `json:"release,omitempty"`
	Message   string        `json:"message,omitempty"`
	Usage     Usage         `json:"usage"`
}

// Node is one cluster node as the API reports it.
type Node struct {
	Name        string        `json:"name"`
	Ready       bool          `json:"ready"`
	Schedulable bool          `json:"schedulable"`
	Roles       string        `json:"roles,omitempty"`
	Version     string        `json:"version,omitempty"`
	Age         time.Duration `json:"age_ns,omitempty"`
	CPUAlloc    int64         `json:"cpu_alloc_milli,omitempty"`
	MemAlloc    int64         `json:"memory_alloc_bytes,omitempty"`
	Usage       Usage         `json:"usage"`
	Message     string        `json:"message,omitempty"`
}

// Usage is a CPU/memory sample. Known is false when metrics-server did not
// report this object, which must render as "—" rather than "0".
type Usage struct {
	Known    bool  `json:"known"`
	CPUMilli int64 `json:"cpu_milli,omitempty"`
	Memory   int64 `json:"memory_bytes,omitempty"`
}

// Alert is a problem worth pinning above the tables.
type Alert struct {
	Level string `json:"level"`
	Text  string `json:"text"`
}

// AddUsage returns a+b, Known only when both sides have a sample.
func AddUsage(a, b Usage) Usage {
	if !a.Known {
		return b
	}
	if !b.Known {
		return a
	}
	return Usage{Known: true, CPUMilli: a.CPUMilli + b.CPUMilli, Memory: a.Memory + b.Memory}
}

// SelectIndex returns the row to highlight, preferring name, then 0.
func (s Snapshot) SelectIndex(name string) int {
	if name == "" || len(s.Apps) == 0 {
		return 0
	}
	for i, app := range s.Apps {
		if app.Name == name {
			return i
		}
	}
	return 0
}

// Selected returns the highlighted app, or a zero App.
func (s Snapshot) Selected(i int) App {
	if i < 0 || i >= len(s.Apps) {
		return App{}
	}
	return s.Apps[i]
}

// ClampSelected keeps i inside the app list after a refresh.
func ClampSelected(i, n int) int {
	if n <= 0 {
		return 0
	}
	if i < 0 {
		return 0
	}
	if i >= n {
		return n - 1
	}
	return i
}
