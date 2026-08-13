package config

import (
	"fmt"
	"strings"
)

// SplitCIDRs breaks a k3s-style comma-separated CIDR list into one network
// per entry. Firewall examples need this: `ufw allow from a,b` is not valid.
func SplitCIDRs(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// HasIPv6CIDR reports whether any network in a CIDR list is IPv6.
func HasIPv6CIDR(s string) bool {
	for _, cidr := range SplitCIDRs(s) {
		if strings.Contains(cidr, ":") {
			return true
		}
	}
	return false
}

func hasIPv4CIDR(s string) bool {
	for _, cidr := range SplitCIDRs(s) {
		if !strings.Contains(cidr, ":") {
			return true
		}
	}
	return false
}

// checkDualStackCIDRs requires cluster and service networks to cover the same
// IP families. k3s rejects a dual-stack pod CIDR paired with an IPv4-only
// service CIDR (and the reverse) at start, with a log line that does not name
// the config keys.
func checkDualStackCIDRs(cluster, service string) error {
	if cluster == "" && service == "" {
		return nil
	}
	if hasIPv4CIDR(cluster) != hasIPv4CIDR(service) || HasIPv6CIDR(cluster) != HasIPv6CIDR(service) {
		return fmt.Errorf("`infra.kubernetes.clusterCIDR` and `serviceCIDR` must cover the same IP families (IPv4, IPv6, or both)")
	}
	return nil
}
