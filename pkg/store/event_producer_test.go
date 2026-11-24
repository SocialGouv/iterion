package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

type producerReply struct {
	event *Event
	calls int
}

func (p *producerReply) AppendEvent(context.Context, string, Event) (*Event, error) {
	p.calls++
	return p.event, nil
}
func (p *producerReply) CheckEventObservationProducer(actual EventAppender) error {
	r, ok := actual.(*producerReply)
	if p == nil || !ok || r != p {
		return ErrRunDataCapability
	}
	return nil
}

type producerOverride struct {
	*FilesystemRunStore
	calls int
}

func (p *producerOverride) AppendEvent(context.Context, string, Event) (*Event, error) {
	p.calls++
	return &Event{RunID: "r"}, nil
}

type producerForward struct {
	inner EventAppender
	calls int
}

func (p *producerForward) CheckEventObservationProducer(actual EventAppender) error {
	r, ok := actual.(*producerForward)
	if p == nil || !ok || r != p {
		return ErrRunDataCapability
	}
	return RequireEventObservationProducer(p.inner)
}
func (p *producerForward) AppendEvent(c context.Context, id string, e Event) (*Event, error) {
	p.calls++
	return p.inner.AppendEvent(c, id, e)
}

func TestObservedProducerReceiverAndReturn(t *testing.T) {
	t.Parallel()
	st, e := New(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	over := &producerOverride{FilesystemRunStore: st}
	if _, e = AppendObservedEvent(t.Context(), over, "r", Event{}); !errors.Is(e, ErrRunDataCapability) || over.calls != 0 {
		t.Fatal(e, over.calls)
	}
	var typedNil *producerReply
	for _, p := range []EventAppender{nil, typedNil} {
		if e := RequireEventObservationProducer(p); !errors.Is(e, ErrRunDataCapability) {
			t.Fatal(e)
		}
	}
	a, e := CaptureRunObservation(RunDataHeader{RunID: "r", Format: 1, BirthID: "A"}, "tenant", true)
	if e != nil {
		t.Fatal(e)
	}
	good, e := WithEventObservation(Event{RunID: "r"}, a)
	if e != nil {
		t.Fatal(e)
	}
	for _, tc := range []struct {
		name  string
		event *Event
		id    string
		ctx   context.Context
		want  error
	}{
		{"nil", nil, "r", t.Context(), ErrRunDataCapability},
		{"unknown", &Event{RunID: "r"}, "r", t.Context(), ErrRunDataCapability},
		{"wrong-call-run", &good, "other", t.Context(), ErrRunDataIdentityChanged},
		{"tenant", &good, "r", WithTenant(t.Context(), "other"), ErrRunDataIdentityChanged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &producerReply{event: tc.event}
			if _, e := AppendObservedEvent(tc.ctx, p, tc.id, Event{}); !errors.Is(e, tc.want) {
				t.Fatal(e)
			}
		})
	}
	b, _ := CaptureRunDataBinding(RunDataHeader{RunID: "r", Format: 1, BirthID: "B"})
	c, _ := WithRunDataBinding(t.Context(), b)
	if _, e = AppendObservedEvent(c, &producerReply{event: &good}, "r", Event{}); !errors.Is(e, ErrRunDataIdentityChanged) {
		t.Fatal(e)
	}
	f, _ := WithRunValueFormat(t.Context(), RunValueFormat{RunID: "r", Format: 2})
	if _, e = AppendObservedEvent(f, &producerReply{event: &good}, "r", Event{}); !errors.Is(e, ErrValueFormatChanged) {
		t.Fatal(e)
	}
	strictOrigin, _ := CaptureRunObservation(RunDataHeader{RunID: "r", Format: 1, Protocol: 1, BirthID: "strict"}, "", true)
	strictEvent, _ := WithEventObservation(Event{RunID: "r"}, strictOrigin)
	if _, e = AppendObservedEvent(t.Context(), &producerReply{event: &strictEvent}, "r", Event{}); !errors.Is(e, ErrRunDataBindingRequired) {
		t.Fatal("strict unbound result accepted", e)
	}
	exactOrigin, _ := CaptureRunObservation(RunDataHeader{RunID: "r", Format: 2}, "", true)
	exactEvent, _ := WithEventObservation(Event{RunID: "r"}, exactOrigin)
	if _, e = AppendObservedEvent(t.Context(), &producerReply{event: &exactEvent}, "r", Event{}); !errors.Is(e, ErrValueFormatChanged) {
		t.Fatal("exact unbound result accepted", e)
	}
	bad := good
	bad.RunID = "different"
	if _, e = AppendObservedEvent(t.Context(), &producerReply{event: &bad}, "r", Event{}); !errors.Is(e, ErrRunDataIdentityChanged) {
		t.Fatal(e)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	p := &producerReply{event: &good}
	if _, e = AppendObservedEvent(cancelled, p, "r", Event{}); !errors.Is(e, context.Canceled) || p.calls != 0 {
		t.Fatal(e, p.calls)
	}
	if _, e = AppendObservedEvent(nil, p, "r", Event{}); !errors.Is(e, ErrRunDataBindingRequired) || p.calls != 0 {
		t.Fatal(e, p.calls)
	}
	forward := &producerForward{inner: &producerReply{event: &good}}
	if got, e := AppendObservedEvent(WithTenant(t.Context(), "tenant"), forward, "r", Event{}); e != nil || got != &good || forward.calls != 1 {
		t.Fatal(got, e, forward.calls)
	}
}

func TestFilesystemAppendObservedParentAndProjection(t *testing.T) {
	t.Parallel()
	for _, protocol := range []RunDataProtocol{0, 1} {
		for _, format := range []ValueFormat{1, 2} {
			t.Run(fmt.Sprintf("p%d-f%d", protocol, format), func(t *testing.T) {
				if e := RequireRunDataStorage(protocol); e != nil {
					if protocol == 1 && errors.Is(e, ErrRunDataCapability) {
						t.Skip("admission OFF")
					}
					t.Fatal(e)
				}
				st, e := New(t.TempDir())
				if e != nil {
					t.Fatal(e)
				}
				ctx := t.Context()
				r, e := st.CreateRunWithValueFormat(ctx, RunCreateSpec{ID: "r", WorkflowName: "wf", Format: format, DataProtocol: protocol})
				if e != nil {
					t.Fatal(e)
				}
				ctx, e = BindRunValueFormat(ctx, r)
				if e != nil {
					t.Fatal(e)
				}
				binding, e := CaptureRunDataBinding(RunDataHeader{RunID: r.ID, Format: format, Protocol: protocol, BirthID: r.BirthID})
				if e != nil {
					t.Fatal(e)
				}
				ctx, e = WithRunDataBinding(ctx, binding)
				if e != nil {
					t.Fatal(e)
				}
				old, _ := CaptureRunObservation(RunDataHeader{RunID: "r", Format: 1, BirthID: "old"}, "other", true)
				input, _ := WithEventObservation(Event{RunID: "r", LogOffset: 7}, old)
				if format == 2 {
					input.Data = map[string]any{"huge": json.Number("1e400")}
				}
				calls := 0
				st.SetLogPositionFn(func(string) int64 { t.Error("legacy callback used despite projection"); return 0 })
				ctx, e = WithEventProjection(ctx, func(_ context.Context, o RunObservation) (int64, int64, error) {
					calls++
					if o.Header().BirthID != r.BirthID || o.TenantID() != r.TenantID || !o.ParentPresent() {
						t.Fatal(o)
					}
					return 9, 11, nil
				})
				if e != nil {
					t.Fatal(e)
				}
				got, e := AppendObservedEvent(ctx, st, "r", input)
				if e != nil {
					t.Fatal(e)
				}
				o, ok := got.EventObservation()
				if !ok || o.Header() != binding.Header() || got.LogOffset != 7 || got.ActiveMs != 11 || calls != 1 {
					t.Fatal(got, o, calls)
				}
				raw, e := EncodeValueRecordJSON(ctx, format, ExactEvent, got)
				if e != nil {
					t.Fatal(e)
				}
				var decoded Event
				if e = DecodeValueRecordJSON(ctx, format, ExactEvent, raw, &decoded); e != nil {
					t.Fatal(e)
				}
				if _, ok := decoded.EventObservation(); ok {
					t.Fatal("wire invented origin")
				}
			})
		}
	}
	st, e := New(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	got, e := st.AppendEvent(t.Context(), "orphan", Event{})
	if e != nil {
		t.Fatal(e)
	}
	o, ok := got.EventObservation()
	if !ok || o.ParentPresent() || o.Header().BirthID != "" {
		t.Fatal(o)
	}
}

func TestObservedProjectionFailureAndPrefill(t *testing.T) {
	t.Parallel()
	o, _ := CaptureRunObservation(RunDataHeader{RunID: "r", Format: 1}, "", true)
	calls := 0
	failure := errors.New("projection refused")
	c, e := WithEventProjection(t.Context(), func(context.Context, RunObservation) (int64, int64, error) { calls++; return 2, 3, failure })
	if e != nil {
		t.Fatal(e)
	}
	v := Event{LogOffset: 1, ActiveMs: 1}
	if ok, e := ApplyEventProjection(c, o, &v); !ok || e != nil || calls != 0 {
		t.Fatal(ok, e, calls)
	}
	v = Event{}
	if ok, e := ApplyEventProjection(c, o, &v); !ok || !errors.Is(e, failure) || v.LogOffset != 0 || v.ActiveMs != 0 || calls != 1 {
		t.Fatal(ok, e, v, calls)
	}
}
