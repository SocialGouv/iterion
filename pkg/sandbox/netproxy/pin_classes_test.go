package netproxy

import (
	"net"
	"strings"
	"testing"
)

// The pin decides the shared (CGNAT) space and a non-public IPv6 transition
// address as the private classes: a strict pin refuses an answer holding
// one before its dialer is asked anything, and PinAllowPrivate admits it
// and dials the address it validated.
func TestThePinDecidesTheSharedAndTransitionClasses(t *testing.T) {
	const gw = "gw.classes.test"
	port, _ := localService(t)
	for _, answer := range []string{"100.64.0.1", "64:ff9b::7f00:1"} {
		dial, dialed := recordingDial(net.JoinHostPort("127.0.0.1", port))
		strict := startPinProxy(t, Options{PinnedHosts: []string{gw}, PinResolver: newScriptedResolver(gw, answer), Dial: dial})
		if got := connectVia(t, strict, gw+":443"); !strings.HasPrefix(got, "502") || !strings.Contains(got, "is not a public unicast IP") {
			t.Errorf("strict pin answered %s: CONNECT = %q; want the pin's refusal", answer, got)
		}
		if got := dialed(); len(got) != 0 {
			t.Errorf("strict pin answered %s: the dialer was asked for %q; want nothing", answer, got)
		}

		dial, dialed = recordingDial(net.JoinHostPort("127.0.0.1", port))
		lifted := startPinProxy(t, Options{PinnedHosts: []string{gw}, PinResolver: newScriptedResolver(gw, answer), PinAllowPrivate: true, Dial: dial})
		if got := connectVia(t, lifted, gw+":443"); !strings.HasPrefix(got, "200") {
			t.Errorf("lifted pin answered %s: CONNECT = %q; want a tunnel", answer, got)
		}
		if got, want := dialed(), net.JoinHostPort(answer, "443"); len(got) != 1 || got[0] != want {
			t.Errorf("lifted pin answered %s: the dialer was asked for %q; want [%s], the address the pin validated", answer, got, want)
		}
	}
}
