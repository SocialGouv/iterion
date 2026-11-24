package protocol

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
)

func TestNativeEnvironmentCapsuleExact(t *testing.T) {
	t.Parallel()
	cases := [][]string{nil, {""}, {"A=1", "A=2", "BA=stay", "=empty-key", "bare", ""}, {"X=\xff\n=tail", "\xfe=\xfd", "IENV_POD_ENV_FD=workload"}, {"LONG=" + strings.Repeat("z", 400000)}}
	for i, want := range cases {
		raw := []byte{}
		for _, s := range want {
			raw = append(raw, []byte(s)...)
			raw = append(raw, 0)
		}
		for name, reader := range map[string]io.Reader{"plain": bytes.NewReader(raw), "one-byte": iotest.OneByteReader(bytes.NewReader(raw)), "data-and-EOF": iotest.DataErrReader(bytes.NewReader(raw))} {
			got, err := ReadNativeEnvironment(reader)
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("CAPSULE_BYTES_LOST case=%d reader=%s lengths=%d/%d err=%v", i, name, len(got), len(want), err)
			}
		}
	}
}
func TestNativeEnvironmentCapsuleRefusesPartial(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"X=secret", "A=1\x00tail", "\x00missing-terminator"} {
		got, err := ReadNativeEnvironment(strings.NewReader(raw))
		if got != nil || !errors.Is(err, ErrTransport) || !errors.Is(err, io.ErrUnexpectedEOF) || strings.Contains(err.Error(), "secret") {
			t.Fatalf("CAPSULE_PARTIAL_ACCEPTED got=%v err=%v", got, err)
		}
	}
	cause := errors.New("synthetic-secret-in-reader")
	got, err := ReadNativeEnvironment(io.MultiReader(strings.NewReader("A=1\x00"), iotest.ErrReader(cause)))
	if got != nil || !errors.Is(err, cause) || !errors.Is(err, ErrTransport) || strings.Contains(err.Error(), cause.Error()) {
		t.Fatalf("CAPSULE_ERROR_CHAIN got=%v err=%v", got, err)
	}
	got, err = ReadNativeEnvironment(&nativeTransientEmptyReader{next: strings.NewReader("A=1\x00")})
	if err != nil || !reflect.DeepEqual(got, []string{"A=1"}) {
		t.Fatalf("CAPSULE_TRANSIENT_EMPTY_REFUSED got=%v err=%v", got, err)
	}
}

type nativeTransientEmptyReader struct {
	once bool
	next io.Reader
}

func (r *nativeTransientEmptyReader) Read(p []byte) (int, error) {
	if !r.once {
		r.once = true
		return 0, nil
	}
	return r.next.Read(p)
}
func TestNativeEnvironmentOverridesExact(t *testing.T) {
	t.Parallel()
	inherited := []string{"X=first", "KEEP=\xff", "X=second", "XY=keep", "bare", "", "=old", "PWD=logical"}
	overrides := map[string]string{"X": "\xfe\n=last", "": "new", "PWD": "", "IENV_POD_ENV_FD": "workload"}
	before := append([]string(nil), inherited...)
	copyMap := map[string]string{}
	for k, v := range overrides {
		copyMap[k] = v
	}
	want := []string{"KEEP=\xff", "XY=keep", "bare", "", "=new", "IENV_POD_ENV_FD=workload", "PWD=", "X=\xfe\n=last"}
	got, err := ApplyEnvironmentOverrides(inherited, overrides)
	if err != nil || !reflect.DeepEqual(got, want) || !reflect.DeepEqual(inherited, before) || !reflect.DeepEqual(overrides, copyMap) {
		t.Fatalf("OVERRIDE_BYTES_OR_ALIAS got=%q err=%v", got, err)
	}
	got[0] = "changed"
	if !reflect.DeepEqual(inherited, before) {
		t.Fatal("OVERRIDE_ALIASES_INPUT")
	}
	for _, overrides := range []map[string]string{nil, {}} {
		got, err := ApplyEnvironmentOverrides(inherited, overrides)
		if err != nil || !reflect.DeepEqual(got, inherited) {
			t.Fatalf("NATIVE_ENV_CHANGED_WITHOUT_OVERRIDE %v", err)
		}
	}
}
func TestNativeEnvironmentInvalidRefusesWhole(t *testing.T) {
	t.Parallel()
	for _, overrides := range []map[string]string{{"bad=key": "v"}, {"bad\x00key": "v"}, {"OK": "secret\x00suffix"}} {
		got, err := ApplyEnvironmentOverrides([]string{"KEEP=1"}, overrides)
		if got != nil || !errors.Is(err, ErrTransport) {
			t.Fatalf("INVALID_ENV_ACCEPTED got=%v err=%v", got, err)
		}
	}
	got, err := ApplyEnvironmentOverrides([]string{"GOOD=1", "BAD=\x00"}, nil)
	if got != nil || !errors.Is(err, ErrTransport) {
		t.Fatalf("INVALID_INHERITED_ACCEPTED got=%v err=%v", got, err)
	}
}
