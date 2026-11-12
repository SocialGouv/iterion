package execenv

import (
	"bytes"
	"encoding/binary"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/sandbox/execenv/protocol"
)

func sampleLaunch() wireLaunch {
	cwd := "../native\xff"
	return wireLaunch{ClientPath: "/captured/client", ClientArgs: []string{"--namespace", "control"}, Invocation: protocol.Invocation{Argv: []string{"/target", "a\xff\n"}, Env: map[string]string{"PATH": "workload-only", wireCount: "payload-not-control", "non\xffutf8": "\xfe\n=value"}, WorkDir: &cwd}, StartTimeout: 30 * time.Second, WaitDelay: 2 * time.Second}
}
func TestWirePreservesNativeValuesAndSeparatesControl(t *testing.T) {
	t.Parallel()
	want := sampleLaunch()
	raw, err := encodeWire(want)
	if err != nil {
		t.Fatal(err)
	}
	control := []string{"PATH=/control", "HTTP_PROXY=operator", "BARE", "EMPTY=", "DUP=a", "DUP=b"}
	env, err := wireEnvironment(control, raw)
	if err != nil {
		t.Fatal(err)
	}
	got, clean, err := readWireEnvironment(env)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(clean, control) {
		t.Fatalf("native/authority mismatch: %#v %#v", got, clean)
	}
	// The constructor output is independent of caller mutation after capture.
	want.ClientArgs[0] = "changed"
	want.Invocation.Argv[1] = "changed"
	want.Invocation.Env["PATH"] = "changed"
	*want.Invocation.WorkDir = "changed"
	control[0] = "changed"
	again, clean, err := readWireEnvironment(env)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(again, got) || clean[0] != "PATH=/control" {
		t.Fatal("caller mutation changed captured launch")
	}
	for _, e := range env {
		if strings.Contains(e, "workload-only") || strings.Contains(e, "payload-not-control") {
			t.Fatal("workload exposed as native host configuration")
		}
	}
}
func TestWireChunksHaveNoProtocolPayloadCeiling(t *testing.T) {
	t.Parallel()
	l := sampleLaunch()
	l.Invocation.Env["large"] = strings.Repeat("value-\xff", 50000)
	raw, err := encodeWire(l)
	if err != nil {
		t.Fatal(err)
	}
	env, err := wireEnvironment(nil, raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(env) < 10 {
		t.Fatal("witness did not span chunks")
	}
	got, _, err := readWireEnvironment(env)
	if err != nil || !reflect.DeepEqual(got, l) {
		t.Fatalf("large in-memory payload: %v", err)
	}
	// This is an in-memory proof, not permission to exceed kernel exec limits.
}
func TestHostEnvelopeRejectsMalformedControlWithoutPartialResult(t *testing.T) {
	t.Parallel()
	raw, err := encodeWire(sampleLaunch())
	if err != nil {
		t.Fatal(err)
	}
	valid, err := wireEnvironment([]string{"KEEP=x"}, raw)
	if err != nil {
		t.Fatal(err)
	}
	variants := map[string][]string{
		"missing": {"KEEP=x"}, "count-0": {wireCount + "=0"}, "count-negative": {wireCount + "=-1"},
		"count-huge": {wireCount + "=922337203685477580700"}, "count-leadingzero": {wireCount + "=01", wirePrefix + "0=x"},
		"missing-chunk": {wireCount + "=1", wirePrefix + "1=x"}, "unknown-reserved": {wireCount + "=1", wirePrefix + "OTHER=x"},
		"bad-base64": {wireCount + "=1", wirePrefix + "0=!"}, "count-without-equals": {wireCount},
		"duplicate-count": append(append([]string(nil), valid...), valid[len(valid)-1]),
		"duplicate-chunk": append(append([]string(nil), valid...), valid[1]),
	}
	for name, env := range variants {
		t.Run(name, func(t *testing.T) {
			got, clean, e := readWireEnvironment(env)
			if !errors.Is(e, protocol.ErrTransport) || !reflect.DeepEqual(got, wireLaunch{}) || clean != nil {
				t.Fatalf("accepted or partial: %#v %#v %v", got, clean, e)
			}
		})
	}
}
func TestHostControlEnvironmentCollisionRefusesEvenEmptyOrBare(t *testing.T) {
	t.Parallel()
	for _, e := range []string{wireCount + "=operator", wirePrefix + "future=", wirePrefix + "bare"} {
		t.Run(e, func(t *testing.T) {
			if _, err := wireEnvironment([]string{e}, []byte("a")); !errors.Is(err, protocol.ErrTransport) {
				t.Fatalf("collision: %v", err)
			}
		})
	}
}
func TestHostEnvelopeVersionsShapeAndDurations(t *testing.T) {
	t.Parallel()
	raw, e := encodeWire(sampleLaunch())
	if e != nil {
		t.Fatal(e)
	}
	cases := map[string][]byte{"short": raw[:23], "future": append([]byte("IENVHST2"), raw[8:]...), "trailing": append(append([]byte(nil), raw...), 1)}
	overflow := append([]byte(nil), raw...)
	binary.BigEndian.PutUint64(overflow[8:16], 1<<63)
	cases["overflow"] = overflow
	// Forbidden control Env is not accepted just because it is a valid frame.
	var bad bytes.Buffer
	bad.Write(raw[:24])
	e = protocol.WriteInvocation(&bad, protocol.Invocation{Argv: []string{"/client"}, Env: map[string]string{"PATH": "hostile"}})
	if e != nil {
		t.Fatal(e)
	}
	e = protocol.WriteInvocation(&bad, sampleLaunch().Invocation)
	if e != nil {
		t.Fatal(e)
	}
	cases["control-env"] = bad.Bytes()
	for name, b := range cases {
		t.Run(name, func(t *testing.T) {
			got, e := decodeWire(b)
			if !errors.Is(e, protocol.ErrTransport) || !reflect.DeepEqual(got, wireLaunch{}) {
				t.Fatalf("unexpected: %#v %v", got, e)
			}
		})
	}
	for _, d := range []time.Duration{0, 1, 1<<63 - 1} {
		l := sampleLaunch()
		l.StartTimeout = d
		l.WaitDelay = d
		b, e := encodeWire(l)
		if e != nil {
			t.Fatal(e)
		}
		g, e := decodeWire(b)
		if e != nil || g.StartTimeout != d || g.WaitDelay != d {
			t.Fatalf("duration %v lost: %#v %v", d, g, e)
		}
	}
}
func TestHostEnvelopeRefusalDoesNotPrintTransportedValues(t *testing.T) {
	t.Parallel()
	l := sampleLaunch()
	l.ClientPath = "secret-relative"
	_, err := encodeWire(l)
	var typed *protocol.Error
	if !errors.As(err, &typed) || strings.Contains(err.Error(), "secret-relative") {
		t.Fatalf("unsafe error: %v", err)
	}
	l = sampleLaunch()
	l.Invocation.Env["credential"] = "secret\x00value"
	_, err = encodeWire(l)
	if !errors.Is(err, protocol.ErrTransport) || strings.Contains(err.Error(), "secret") {
		t.Fatalf("unsafe frame error: %v", err)
	}
}
