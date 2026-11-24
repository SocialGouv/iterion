package protocol

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

func frameBytes(t *testing.T, in Invocation) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := WriteInvocation(&b, in); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func TestInvocationNativeRoundTripAndStdin(t *testing.T) {
	t.Parallel()
	cwd := "../link/../ dir\n\xff"
	empty := ""
	for _, dir := range []*string{nil, &empty, &cwd} {
		in := Invocation{Argv: []string{"argv0", "", "a\n\xff"}, Env: map[string]string{"": "empty key", "a\xff": "\xfe\n", "IENV_POD_ENV_FD": "workload datum", "PWD": ""}, WorkDir: dir}
		wire := frameBytes(t, in)
		tail := []byte{0, 255, 's', 't', 'd', 'i', 'n'}
		r := bytes.NewReader(append(wire, tail...))
		got, err := ReadInvocation(r)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, in) {
			t.Fatalf("NATIVE_ROUNDTRIP_LOST got=%#v want=%#v", got, in)
		}
		rest, err := io.ReadAll(r)
		if err != nil || !bytes.Equal(rest, tail) {
			t.Fatalf("STDIN_CONSUMED %x %v", rest, err)
		}
		if !bytes.Equal(wire, frameBytes(t, in)) {
			t.Fatal("ORDER_UNSTABLE")
		}
	}
}
func TestInvocationRejectsBeforeWriting(t *testing.T) {
	t.Parallel()
	badDir := "x\x00y"
	for _, in := range []Invocation{{}, {Argv: []string{"x\x00y"}}, {Argv: []string{"x"}, Env: map[string]string{"a=b": "v"}}, {Argv: []string{"x"}, Env: map[string]string{"a\x00b": "v"}}, {Argv: []string{"x"}, Env: map[string]string{"k": "secret\x00suffix"}}, {Argv: []string{"x"}, WorkDir: &badDir}} {
		var b bytes.Buffer
		err := WriteInvocation(&b, in)
		if !errors.Is(err, ErrTransport) || b.Len() != 0 {
			t.Fatalf("INVALID_FRAME_WRITTEN bytes=%d err=%v", b.Len(), err)
		}
		if strings.Contains(err.Error(), "secret") {
			t.Fatal("SECRET_DIAGNOSTIC")
		}
	}
}
func TestInvocationAllTruncationsRefuseNoPartialResult(t *testing.T) {
	t.Parallel()
	cwd := "here"
	wire := frameBytes(t, Invocation{Argv: []string{"run", "arg"}, Env: map[string]string{"k": "value"}, WorkDir: &cwd})
	for n := 0; n < len(wire); n++ {
		got, err := ReadInvocation(bytes.NewReader(wire[:n]))
		if !errors.Is(err, ErrTransport) || !reflect.DeepEqual(got, Invocation{}) {
			t.Fatalf("TRUNCATED_ACCEPTED n=%d got=%#v err=%v", n, got, err)
		}
	}
}
func TestInvocationHostileFrameShapes(t *testing.T) {
	t.Parallel()
	body := func(parts ...any) []byte {
		var b bytes.Buffer
		for _, p := range parts {
			switch v := p.(type) {
			case uint64:
				_ = binary.Write(&b, binary.BigEndian, v)
			case string:
				_ = binary.Write(&b, binary.BigEndian, uint64(len(v)))
				b.WriteString(v)
			}
		}
		var out bytes.Buffer
		out.WriteString(frameMagic)
		_ = binary.Write(&out, binary.BigEndian, uint64(b.Len()))
		out.Write(b.Bytes())
		return out.Bytes()
	}
	frames := map[string][]byte{
		"duplicate": body(uint64(2), "k", "one", "k", "two", uint64(0), uint64(1), "x"),
		"count":     body(^uint64(0), uint64(0), uint64(1), "x"),
		"presence":  body(uint64(0), uint64(2), uint64(1), "x"),
		"length":    body(uint64(1), ^uint64(0)),
		"noargv":    body(uint64(0), uint64(0), uint64(0)),
		"envnul":    body(uint64(1), "k", "bad\x00value", uint64(0), uint64(1), "x"),
		"keyequals": body(uint64(1), "k=", "value", uint64(0), uint64(1), "x"),
		"dirnul":    body(uint64(0), uint64(1), "bad\x00dir", uint64(1), "x"),
		"argvnul":   body(uint64(0), uint64(0), uint64(1), "bad\x00arg"),
		"trailing":  body(uint64(0), uint64(0), uint64(1), "x", uint64(0)),
	}
	for name, wire := range frames {
		t.Run(name, func(t *testing.T) {
			got, err := ReadInvocation(bytes.NewReader(wire))
			if !errors.Is(err, ErrTransport) || !reflect.DeepEqual(got, Invocation{}) {
				t.Fatalf("MALFORMED_ACCEPTED %s %#v %v", name, got, err)
			}
		})
	}
	wire := frameBytes(t, Invocation{Argv: []string{"x"}})
	wire[7] = '9'
	_, err := ReadInvocation(bytes.NewReader(wire))
	var detail *Error
	if !errors.As(err, &detail) || detail.Kind != "version" {
		t.Fatalf("FUTURE_VERSION_ACCEPTED %v", err)
	}
	binary.BigEndian.PutUint64(wire[8:16], ^uint64(0))
	wire[7] = '1'
	_, err = ReadInvocation(bytes.NewReader(wire))
	if !errors.As(err, &detail) || detail.Kind != "length" {
		t.Fatalf("OVERFLOW_ACCEPTED %v", err)
	}
}

type byteWriter struct{ bytes.Buffer }

func (w *byteWriter) Write(b []byte) (int, error) {
	if len(b) > 1 {
		b = b[:1]
	}
	return w.Buffer.Write(b)
}

type failedWriter struct{ cause error }

func (w failedWriter) Write([]byte) (int, error) { return 0, w.cause }
func TestInvocationWriteAndReadErrorsRetainCauseWithoutData(t *testing.T) {
	t.Parallel()
	in := Invocation{Argv: []string{"run"}, Env: map[string]string{"k": "secret"}}
	var slow byteWriter
	if err := WriteInvocation(&slow, in); err != nil {
		t.Fatal(err)
	}
	got, err := ReadInvocation(bytes.NewReader(slow.Bytes()))
	if err != nil || !reflect.DeepEqual(got, in) {
		t.Fatalf("SHORT_WRITE_LOSS %#v %v", got, err)
	}
	cause := errors.New("secret reader diagnostic")
	err = WriteInvocation(failedWriter{cause}, in)
	if !errors.Is(err, cause) || strings.Contains(err.Error(), "secret") {
		t.Fatalf("IO_CAUSE_OR_DATA %v", err)
	}
	_, err = ReadInvocation(io.MultiReader(bytes.NewReader([]byte("IENV")), errorReader{cause}))
	if !errors.Is(err, cause) || strings.Contains(err.Error(), "secret") {
		t.Fatalf("IO_READ_CAUSE_OR_DATA %v", err)
	}
}

type errorReader struct{ error }

func (r errorReader) Read([]byte) (int, error) { return 0, r.error }
func TestInvocationAdvertisedLengthDoesNotEagerlyAllocate(t *testing.T) {
	t.Parallel()
	var b bytes.Buffer
	b.WriteString(frameMagic)
	for _, n := range []uint64{1 << 40, 1, 1 << 39} {
		_ = binary.Write(&b, binary.BigEndian, n)
	}
	got, err := ReadInvocation(&b)
	if !errors.Is(err, ErrTransport) || !reflect.DeepEqual(got, Invocation{}) {
		t.Fatalf("HUGE_TRUNCATED_ACCEPTED %#v %v", got, err)
	}
}
func TestInvocationLargeFrameNoArtificialLimit(t *testing.T) {
	t.Parallel()
	in := Invocation{Argv: []string{"run"}, Env: map[string]string{"k": strings.Repeat("\xff\n", 200000)}}
	got, err := ReadInvocation(bytes.NewReader(frameBytes(t, in)))
	if err != nil || !reflect.DeepEqual(got, in) {
		t.Fatalf("LARGE_NATIVE_FRAME_LOSS %v", err)
	}
}
