package config

import (
	"testing"

	"github.com/danecwalker/buidl/internal/inventory"
)

func TestFleetSize(t *testing.T) {
	if got := FleetSize(nil); got != 0 {
		t.Errorf("nil config = %d, want 0", got)
	}
	if got := FleetSize(&Config{}); got != 0 {
		t.Errorf("no infra = %d, want 0", got)
	}
	cfg := &Config{Infra: &Infra{Servers: []inventory.Server{
		{Host: "10.0.0.1"},
		{Host: "10.0.0.2"},
		{Host: "10.0.0.3"},
	}}}
	if got := FleetSize(cfg); got != 3 {
		t.Errorf("three servers = %d, want 3", got)
	}
}

func TestResolveAutoscale(t *testing.T) {
	tests := []struct {
		name     string
		nodes    int
		min, max int32
		wantMin  int32
		wantMax  int32
	}{
		{name: "single node", nodes: 1, wantMin: 1, wantMax: 4},
		{name: "zero nodes falls back to one", nodes: 0, wantMin: 1, wantMax: 4},
		{name: "two nodes", nodes: 2, wantMin: 2, wantMax: 8},
		{name: "three nodes", nodes: 3, wantMin: 2, wantMax: 9},
		{name: "five nodes", nodes: 5, wantMin: 2, wantMax: 15},
		{name: "explicit bounds win", nodes: 5, min: 3, max: 10, wantMin: 3, wantMax: 10},
		{name: "explicit min, derived max", nodes: 1, min: 5, wantMin: 5, wantMax: 20},
		{name: "derived max never below min", nodes: 1, min: 6, max: 0, wantMin: 6, wantMax: 24},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{Deploy: Deploy{Autoscale: &Autoscale{Min: tt.min, Max: tt.max, CPUPercent: 70}}}
			if tt.min == 0 {
				cfg.Deploy.Autoscale.derivedMin = true
			}
			if tt.max == 0 {
				cfg.Deploy.Autoscale.derivedMax = true
			}
			ResolveAutoscale(cfg, tt.nodes)
			as := cfg.Deploy.Autoscale
			if as.Min != tt.wantMin || as.Max != tt.wantMax {
				t.Errorf("min/max = %d/%d, want %d/%d", as.Min, as.Max, tt.wantMin, tt.wantMax)
			}
		})
	}
}

func TestResolveAutoscaleRecomputesDerivedBounds(t *testing.T) {
	cfg := &Config{Deploy: Deploy{Autoscale: &Autoscale{CPUPercent: 70}}}
	ResolveAutoscale(cfg, 1)
	if cfg.Deploy.Autoscale.Min != 1 || cfg.Deploy.Autoscale.Max != 4 {
		t.Fatalf("after 1 node: min/max = %d/%d", cfg.Deploy.Autoscale.Min, cfg.Deploy.Autoscale.Max)
	}
	// A later, better view of the fleet must be allowed to raise the ceiling.
	ResolveAutoscale(cfg, 4)
	if cfg.Deploy.Autoscale.Min != 2 || cfg.Deploy.Autoscale.Max != 12 {
		t.Errorf("after 4 nodes: min/max = %d/%d, want 2/12", cfg.Deploy.Autoscale.Min, cfg.Deploy.Autoscale.Max)
	}
}

func TestResolveAutoscaleLeavesExplicitBounds(t *testing.T) {
	cfg := &Config{Deploy: Deploy{Autoscale: &Autoscale{Min: 3, Max: 10, CPUPercent: 70}}}
	ResolveAutoscale(cfg, 8)
	if cfg.Deploy.Autoscale.Min != 3 || cfg.Deploy.Autoscale.Max != 10 {
		t.Errorf("explicit bounds were overwritten: %d/%d", cfg.Deploy.Autoscale.Min, cfg.Deploy.Autoscale.Max)
	}
}

func TestResolveAutoscaleNoopWithoutHPA(t *testing.T) {
	cfg := &Config{}
	ResolveAutoscale(cfg, 3)
	if cfg.Deploy.Autoscale != nil {
		t.Error("should not invent an HPA")
	}
}
