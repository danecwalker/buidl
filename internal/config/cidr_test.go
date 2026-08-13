package config

import "testing"

func TestSplitCIDRs(t *testing.T) {
	got := SplitCIDRs("10.42.0.0/16, fd00:42::/56")
	if len(got) != 2 || got[0] != "10.42.0.0/16" || got[1] != "fd00:42::/56" {
		t.Fatalf("SplitCIDRs = %v", got)
	}
	if SplitCIDRs("") != nil {
		t.Fatal("empty should yield nil")
	}
}

func TestHasIPv6CIDR(t *testing.T) {
	if !HasIPv6CIDR(DefaultClusterCIDR) {
		t.Fatal("default cluster CIDR should be dual-stack")
	}
	if HasIPv6CIDR("10.42.0.0/16") {
		t.Fatal("IPv4-only should not report IPv6")
	}
}

func TestCheckDualStackCIDRs(t *testing.T) {
	if err := checkDualStackCIDRs(DefaultClusterCIDR, DefaultServiceCIDR); err != nil {
		t.Fatalf("defaults must be consistent: %v", err)
	}
	if err := checkDualStackCIDRs("10.42.0.0/16", "10.43.0.0/16"); err != nil {
		t.Fatalf("IPv4-only pair must be valid: %v", err)
	}
	if err := checkDualStackCIDRs("10.42.0.0/16,fd00:42::/56", "10.43.0.0/16"); err == nil {
		t.Fatal("mixed families should be rejected")
	}
}
