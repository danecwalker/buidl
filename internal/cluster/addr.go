package cluster

import (
	"net/netip"
	"strings"
)

// pickGlobalIPv6 chooses one address from `ip -6 -o addr show scope global`.
//
// A public address wins over ULA: ServiceLB publishes InternalIP, and Let's
// Encrypt (and browsers) need the address that is in DNS, not fd00::/8.
// Privacy/temporary addresses are skipped because they rotate.
func pickGlobalIPv6(stdout string) string {
	var ula string
	for _, line := range strings.Split(stdout, "\n") {
		if !strings.Contains(line, "inet6") {
			continue
		}
		if strings.Contains(line, "temporary") || strings.Contains(line, "deprecated") {
			continue
		}
		fields := strings.Fields(line)
		var raw string
		for i, f := range fields {
			if f == "inet6" && i+1 < len(fields) {
				raw = fields[i+1]
				break
			}
		}
		if slash := strings.Index(raw, "/"); slash >= 0 {
			raw = raw[:slash]
		}
		addr, err := netip.ParseAddr(raw)
		if err != nil || !addr.Is6() || addr.IsLoopback() || addr.IsLinkLocalUnicast() {
			continue
		}
		if addr.IsPrivate() {
			if ula == "" {
				ula = addr.String()
			}
			continue
		}
		return addr.String()
	}
	return ula
}
