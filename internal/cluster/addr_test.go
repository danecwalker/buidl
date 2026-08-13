package cluster

import "testing"

func TestPickGlobalIPv6PrefersPublic(t *testing.T) {
	out := `
2: eth0    inet6 fd00:42::1/64 scope global
2: eth0    inet6 2a01:4f8:c015:6304::1/64 scope global
2: eth0    inet6 2a01:4f8:c015:6304:a:b:c:d/64 scope global temporary
`
	got := pickGlobalIPv6(out)
	if got != "2a01:4f8:c015:6304::1" {
		t.Fatalf("pickGlobalIPv6 = %q", got)
	}
}

func TestPickGlobalIPv6FallsBackToULA(t *testing.T) {
	out := "4: cni0    inet6 fd00:42::1/64 scope global"
	got := pickGlobalIPv6(out)
	if got != "fd00:42::1" {
		t.Fatalf("pickGlobalIPv6 = %q", got)
	}
}

func TestPickGlobalIPv6Empty(t *testing.T) {
	if got := pickGlobalIPv6(""); got != "" {
		t.Fatalf("empty = %q", got)
	}
}
