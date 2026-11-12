package protocol

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

func TestNilProfilePreservesLegacyGolden(t *testing.T) {
	t.Parallel()
	in := Invocation{Argv: []string{"run"}, Env: map[string]string{"k": "v"}}
	// Wire emitted by the pre-profile v1 layout, including complete lengths/body.
	want, e := hex.DecodeString("49454e564f4e45310000000000000035000000000000000100000000000000016b00000000000000017600000000000000000000000000000001000000000000000372756e")
	if e != nil {
		t.Fatal(e)
	}
	got := frameBytes(t, in)
	if !bytes.Equal(got, want) {
		t.Fatalf("V1_GOLDEN_CHANGED got=%x want=%x", got, want)
	}
	decoded, e := ReadInvocation(bytes.NewReader(want))
	if e != nil || decoded.Profile != nil || !reflect.DeepEqual(decoded, in) {
		t.Fatalf("legacy read %v %#v", e, decoded)
	}
	if InvocationReadiness(decoded) != Readiness {
		t.Fatal("legacy guest would acknowledge wrong version")
	}
}
func TestProfileRoundTripPresenceNativeValuesAndStdin(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"", "/opt/bin:/fallback", "relative::/native\xff", strings.Repeat("segment:", 10000)} {
		t.Run(path[:min(len(path), 40)], func(t *testing.T) {
			p := ExecutionProfile{DefaultPath: path, Shell: "/shell\xff"}
			in := Invocation{Argv: []string{"command", "arg\xfe"}, Env: map[string]string{"SHELL": "workload-only"}, Profile: &p}
			wire := frameBytes(t, in)
			if string(wire[:8]) != profileFrameMagic {
				t.Fatal("PROFILE_NOT_VERSIONED")
			}
			tail := []byte{0, 255, 'x'}
			r := bytes.NewReader(append(wire, tail...))
			got, e := ReadInvocation(r)
			if e != nil || !reflect.DeepEqual(got, in) {
				t.Fatalf("PROFILE_LOST got=%#v err=%v", got, e)
			}
			rest, e := io.ReadAll(r)
			if e != nil || !bytes.Equal(rest, tail) {
				t.Fatalf("PROFILE_CONSUMED_STDIN %x %v", rest, e)
			}
			p.DefaultPath = "changed"
			if got.Profile.DefaultPath != path {
				t.Fatal("PROFILE_ALIASES_CALLER")
			}
			if InvocationReadiness(got) != ProfileReadiness {
				t.Fatal("V2_WRONG_READINESS")
			}
		})
	}
}
func TestProfileInvalidRefusesBeforeAnyWriteAndHidesValues(t *testing.T) {
	t.Parallel()
	for _, p := range []ExecutionProfile{{Shell: ""}, {Shell: "relative-secret"}, {Shell: "/secret\x00shell"}, {DefaultPath: "secret\x00path", Shell: "/bin/sh"}} {
		var b bytes.Buffer
		e := WriteInvocation(&b, Invocation{Argv: []string{"run"}, Profile: &p})
		if !errors.Is(e, ErrTransport) || b.Len() != 0 || strings.Contains(e.Error(), "secret") {
			t.Fatalf("INVALID_PROFILE_WRITTEN %d %v", b.Len(), e)
		}
	}
}
func TestProfileAllTruncationsNoPartialAndFutureRefused(t *testing.T) {
	t.Parallel()
	wire := frameBytes(t, Invocation{Argv: []string{"command"}, Env: map[string]string{"k": "v"}, Profile: &ExecutionProfile{DefaultPath: "/operator", Shell: "/shell"}})
	for n := 0; n < len(wire); n++ {
		got, e := ReadInvocation(bytes.NewReader(wire[:n]))
		if !errors.Is(e, ErrTransport) || !reflect.DeepEqual(got, Invocation{}) {
			t.Fatalf("PROFILE_TRUNCATION_ACCEPTED n=%d %#v %v", n, got, e)
		}
	}
	wire[7] = '9'
	got, e := ReadInvocation(bytes.NewReader(wire))
	var detail *Error
	if !errors.As(e, &detail) || detail.Kind != "version" || !reflect.DeepEqual(got, Invocation{}) {
		t.Fatalf("FUTURE_PROFILE_ACCEPTED %#v %v", got, e)
	}
}
func TestProfileReaderValidatesUntrustedShellBeforeResult(t *testing.T) {
	t.Parallel()
	wire := frameBytes(t, Invocation{Argv: []string{"run"}, Profile: &ExecutionProfile{DefaultPath: "", Shell: "/secret"}})
	// Replace only the same-length shell bytes; lengths remain valid.
	wire = bytes.Replace(wire, []byte("/secret"), []byte("xsecret"), 1)
	got, e := ReadInvocation(bytes.NewReader(wire))
	if !errors.Is(e, ErrTransport) || !reflect.DeepEqual(got, Invocation{}) || strings.Contains(e.Error(), "secret") {
		t.Fatalf("UNTRUSTED_PROFILE_ACCEPTED %#v %v", got, e)
	}
}
