package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestObservedProducerNativeTransparentWrapper(t *testing.T) {
	t.Parallel()
	s, e := New(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.CreateRun(t.Context(), "r", "wf", nil); e != nil {
		t.Fatal(e)
	}
	wrapper := &producerForward{inner: s}
	if _, e = AppendObservedEvent(WithTenant(t.Context(), "wrong"), wrapper, "r", Event{LogOffset: 1, ActiveMs: 1}); !errors.Is(e, ErrRunDataIdentityChanged) {
		t.Fatal(e)
	}
	if wrapper.calls != 1 {
		t.Fatal("outer wrapper bypassed", wrapper.calls)
	}
	if _, e = os.Stat(filepath.Join(s.root, "runs", "r", "events.jsonl")); !os.IsNotExist(e) {
		t.Fatal("marker lost through wrapper", e)
	}
	got, e := AppendObservedEvent(t.Context(), wrapper, "r", Event{})
	if e != nil || got == nil || wrapper.calls != 2 {
		t.Fatal(got, e, wrapper.calls)
	}
	if o, ok := got.EventObservation(); !ok || o.TenantID() != "" || !o.ParentPresent() {
		t.Fatal(o)
	}
}
