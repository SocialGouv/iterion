package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// This is characterization, not an assertion that an unstamped decoded value
// carries the writer's private provenance.
func TestObservedProducerPlainReadCompatibility(t *testing.T) {
	t.Parallel()
	s, e := New(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.CreateRun(t.Context(), "r", "wf", nil); e != nil {
		t.Fatal(e)
	}
	written, e := s.AppendEvent(t.Context(), "r", Event{Data: map[string]any{"value": "unchanged"}})
	if e != nil {
		t.Fatal(e)
	}
	rows, e := s.LoadEvents(t.Context(), "r")
	if e != nil || len(rows) != 1 {
		t.Fatal(rows, e)
	}
	if _, ok := written.EventObservation(); !ok {
		t.Fatal("append missing provenance")
	}
	if _, ok := rows[0].EventObservation(); ok {
		t.Fatal("plain reader fabricated provenance")
	}
	if reflect.DeepEqual(written, rows[0]) {
		t.Fatal("characterization expected distinct private state")
	}
	copy := *written
	copy.observation = RunObservation{}
	if !reflect.DeepEqual(&copy, rows[0]) {
		t.Fatal("difference extends beyond private observation", copy, rows[0])
	}
	a, e := EncodeValueRecordJSON(t.Context(), 1, ExactEvent, written)
	if e != nil {
		t.Fatal(e)
	}
	b, e := EncodeValueRecordJSON(t.Context(), 1, ExactEvent, rows[0])
	if e != nil || !bytes.Equal(a, b) {
		t.Fatal("wire changed", string(a), string(b), e)
	}
}
func TestObservedProducerHistoricalEmptyTenant(t *testing.T) {
	t.Parallel()
	s, e := New(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	r, e := s.CreateRun(t.Context(), "r", "wf", nil)
	if e != nil || r.TenantID != "" {
		t.Fatal(r, e)
	}
	c := WithTenant(t.Context(), "caller-tenant")
	got, e := s.AppendEvent(c, "r", Event{})
	if e != nil {
		t.Fatal("legacy append compatibility", e)
	}
	o, ok := got.EventObservation()
	if !ok || o.TenantID() != "" {
		t.Fatal("invented canonical tenant", o)
	}
	before, err := s.LoadEvents(c, "r")
	if err != nil {
		t.Fatal(err)
	}
	// The new opt-in helper cannot certify caller tenant as this parent's tenant.
	if _, e = AppendObservedEvent(c, s, "r", Event{}); !errors.Is(e, ErrRunDataIdentityChanged) {
		t.Fatal("scoped mismatch must refuse", e)
	}
	after, err := s.LoadEvents(c, "r")
	if err != nil || len(after) != len(before) {
		t.Fatal("owned helper must refuse before append", len(before), len(after), err)
	}
	t.Logf("canonical tenant empty retained; owned helper refused before append: before=%d after=%d", len(before), len(after))
}

func TestObservedProducerNativeIntentBeforeProjection(t *testing.T) {
	t.Parallel()
	s, e := New(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	r, e := s.CreateRun(t.Context(), "r", "wf", nil)
	if e != nil {
		t.Fatal(e)
	}
	calls := 0
	c, e := WithEventProjection(WithTenant(t.Context(), "tenant"), func(context.Context, RunObservation) (int64, int64, error) { calls++; return 2, 3, nil })
	if e != nil {
		t.Fatal(e)
	}
	if _, e = AppendObservedEvent(c, s, "r", Event{}); !errors.Is(e, ErrRunDataIdentityChanged) {
		t.Fatal(e)
	}
	if calls != 0 {
		t.Fatal("projection before refusal", calls)
	}
	if _, e = os.Stat(filepath.Join(s.root, "runs", "r", "events.jsonl")); !os.IsNotExist(e) {
		t.Fatal("payload created before refusal", e)
	}
	// A real nonempty parent tenant is supported when it matches the caller.
	r.TenantID = "tenant"
	if e = s.SaveRun(t.Context(), r); e != nil {
		t.Fatal(e)
	}
	got, e := AppendObservedEvent(c, s, "r", Event{})
	if e != nil || calls != 1 {
		t.Fatal(got, e, calls)
	}
	o, ok := got.EventObservation()
	if !ok || o.TenantID() != "tenant" {
		t.Fatal(o)
	}
}
