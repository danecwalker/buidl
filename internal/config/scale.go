package config

// FleetSize is the number of servers listed in infra. Zero means the fleet is
// unknown: a managed cluster, or a config with no infra block.
func FleetSize(c *Config) int {
	if c == nil || c.Infra == nil {
		return 0
	}
	return len(c.Infra.Servers)
}

// ResolveAutoscale fills omitted (or previously derived) HPA bounds from the
// size of the fleet.
//
// The formula is meant to be a safe first-run default, not a capacity plan:
//
//   - one node keeps a single replica at rest so a laptop cluster still works
//   - two or more nodes keep a floor of two so a node loss is not an outage
//   - the ceiling is the larger of 3 pods per node, 4× the floor, and 4
//
// Explicit min/max in buidl.yaml are left alone. nodes <= 0 is treated as 1.
func ResolveAutoscale(c *Config, nodes int) {
	if c == nil || c.Deploy.Autoscale == nil {
		return
	}
	if nodes < 1 {
		nodes = 1
	}

	as := c.Deploy.Autoscale
	derivedMin := int32(1)
	if nodes >= 2 {
		derivedMin = 2
	}

	minForMax := as.Min
	if as.Min == 0 || as.derivedMin {
		minForMax = derivedMin
	}
	derivedMax := int32(nodes * 3)
	if floor := minForMax * 4; floor > derivedMax {
		derivedMax = floor
	}
	if derivedMax < 4 {
		derivedMax = 4
	}

	if as.Min == 0 || as.derivedMin {
		as.Min = derivedMin
		as.derivedMin = true
	}
	if as.Max == 0 || as.derivedMax {
		as.Max = derivedMax
		as.derivedMax = true
	}
	// Only raise a derived ceiling. An explicit max below min is a config
	// error and is left for Validate to report.
	if as.derivedMax && as.Max < as.Min {
		as.Max = as.Min
	}
}
