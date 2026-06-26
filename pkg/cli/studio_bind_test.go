package cli

import "testing"

// TestIsLoopbackBindHost guards the studio no-auth bind gate (Seki finding):
// local-mode studio runs DisableAuth=true, which is only safe on loopback —
// a non-loopback bind would expose unauthenticated super-admin to the network.
func TestIsLoopbackBindHost(t *testing.T) {
	cases := []struct {
		bind string
		want bool
	}{
		{"", true},               // defaults to 127.0.0.1 upstream
		{"127.0.0.1", true},
		{"127.0.0.5", true},      // whole 127/8 loopback
		{"::1", true},
		{"localhost", true},
		{"LocalHost", true},      // case-insensitive
		{"0.0.0.0", false},       // all interfaces → network-exposed
		{"::", false},
		{"192.168.1.10", false},  // LAN
		{"10.0.0.3", false},
		{"example.com", false},   // unparseable host → fail safe (non-loopback)
	}
	for _, c := range cases {
		if got := isLoopbackBindHost(c.bind); got != c.want {
			t.Errorf("isLoopbackBindHost(%q) = %v; want %v", c.bind, got, c.want)
		}
	}
}
