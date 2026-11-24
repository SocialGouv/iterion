package execenv

import (
	"bytes"
	"errors"
	"reflect"
	"testing"

	"github.com/SocialGouv/iterion/pkg/sandbox/execenv/protocol"
)

func TestHostControlProfileIsRefusedRatherThanIgnored(t *testing.T) {
	t.Parallel()
	l := sampleLaunch()
	raw, e := encodeWire(l)
	if e != nil {
		t.Fatal(e)
	}
	var b bytes.Buffer
	b.Write(raw[:24])
	e = protocol.WriteInvocation(&b, protocol.Invocation{Argv: []string{l.ClientPath}, Profile: &protocol.ExecutionProfile{DefaultPath: "", Shell: "/bin/sh"}})
	if e != nil {
		t.Fatal(e)
	}
	e = protocol.WriteInvocation(&b, l.Invocation)
	if e != nil {
		t.Fatal(e)
	}
	got, e := decodeWire(b.Bytes())
	if !errors.Is(e, protocol.ErrTransport) || !reflect.DeepEqual(got, wireLaunch{}) {
		t.Fatalf("CONTROL_PROFILE_IGNORED %#v %v", got, e)
	}
}
func TestHostProfileIsCapturedAndReadinessIsFrameSpecific(t *testing.T) {
	t.Parallel()
	l := sampleLaunch()
	p := protocol.ExecutionProfile{DefaultPath: "", Shell: "/operator/shell"}
	l.Invocation.Profile = &p
	wire, e := encodeWire(l)
	if e != nil {
		t.Fatal(e)
	}
	p.Shell = "/changed"
	got, e := decodeWire(wire)
	if e != nil || got.Invocation.Profile == nil || got.Invocation.Profile.Shell != "/operator/shell" {
		t.Fatalf("PROFILE_CAPTURE_LOST %#v %v", got, e)
	}
	for _, marker := range []string{protocol.Readiness, protocol.ProfileReadiness} {
		t.Run(marker, func(t *testing.T) {
			var dst bytes.Buffer
			rejected := false
			w := newPrefixWriter(&dst, func() { rejected = true }, got.Invocation)
			_, e := w.Write([]byte(marker + "body"))
			if marker == protocol.ProfileReadiness {
				if e != nil || rejected || dst.String() != "body" {
					t.Fatalf("V2_READY_LOST %v %v %q", e, rejected, dst.String())
				}
			} else {
				if !errors.Is(e, protocol.ErrTransport) || !rejected || dst.Len() != 0 {
					t.Fatalf("PROFILE_DOWNGRADE_ACCEPTED %v %v %q", e, rejected, dst.String())
				}
			}
		})
	}
}
