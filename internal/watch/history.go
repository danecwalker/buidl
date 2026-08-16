package watch

// historyCap is about two minutes at the default 2s refresh — enough
// for a sparkline to show a shape without hanging onto stale hours.
const historyCap = 60

// History is a ring of CPU/RAM samples keyed by app and node name.
// Zero value is empty and safe to read.
type History struct {
	cap   int
	apps  map[string]*metricSeries
	nodes map[string]*metricSeries
	stack metricSeries
}

type metricSeries struct {
	cpu []sample
	mem []sample
}

type sample struct {
	v     int64
	known bool
}

// Record appends this snapshot. Call once per successful refresh.
func (h *History) Record(s Snapshot) {
	if h.cap <= 0 {
		h.cap = historyCap
	}
	if h.apps == nil {
		h.apps = make(map[string]*metricSeries)
	}
	if h.nodes == nil {
		h.nodes = make(map[string]*metricSeries)
	}
	var total Usage
	for _, app := range s.Apps {
		ser := h.ensure(h.apps, app.Name)
		ser.add(app.Usage, h.cap)
		total = AddUsage(total, app.Usage)
	}
	h.stack.add(total, h.cap)
	for _, node := range s.Nodes {
		h.ensure(h.nodes, node.Name).add(node.Usage, h.cap)
	}
}

func (h *History) ensure(m map[string]*metricSeries, name string) *metricSeries {
	ser := m[name]
	if ser == nil {
		ser = &metricSeries{}
		m[name] = ser
	}
	return ser
}

func (s *metricSeries) add(u Usage, cap int) {
	s.cpu = appendSample(s.cpu, sample{v: u.CPUMilli, known: u.Known}, cap)
	s.mem = appendSample(s.mem, sample{v: u.Memory, known: u.Known}, cap)
}

func appendSample(dst []sample, s sample, cap int) []sample {
	if cap <= 0 {
		cap = historyCap
	}
	if len(dst) >= cap {
		copy(dst, dst[len(dst)-cap+1:])
		dst = dst[:cap-1]
	}
	return append(dst, s)
}

func (h History) appCPU(name string) []sample {
	if h.apps == nil || h.apps[name] == nil {
		return nil
	}
	return h.apps[name].cpu
}

func (h History) appMem(name string) []sample {
	if h.apps == nil || h.apps[name] == nil {
		return nil
	}
	return h.apps[name].mem
}

func (h History) nodeCPU(name string) []sample {
	if h.nodes == nil || h.nodes[name] == nil {
		return nil
	}
	return h.nodes[name].cpu
}

func (h History) nodeMem(name string) []sample {
	if h.nodes == nil || h.nodes[name] == nil {
		return nil
	}
	return h.nodes[name].mem
}

func (h History) stackCPU() []sample { return h.stack.cpu }
func (h History) stackMem() []sample { return h.stack.mem }
