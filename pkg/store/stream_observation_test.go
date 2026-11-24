package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func readerWrite(t *testing.T, p string, raw []byte) {
	t.Helper()
	if e := os.WriteFile(p, raw, 0600); e != nil {
		t.Fatal(e)
	}
}
func TestReaderOriginFilesystemMatrix(t *testing.T) {
	for _, p := range []RunDataProtocol{0, 1} {
		for _, f := range []ValueFormat{1, 2} {
			t.Run(fmt.Sprintf("p%d-f%d", p, f), func(t *testing.T) {
				if e := RequireRunDataStorage(p); e != nil {
					if p == 1 && errors.Is(e, ErrRunDataCapability) {
						t.Skip("admission OFF")
					}
					t.Fatal(e)
				}
				dir := t.TempDir()
				s, e := New(dir)
				if e != nil {
					t.Fatal(e)
				}
				r, e := s.CreateRunWithValueFormat(t.Context(), RunCreateSpec{ID: "r", Format: f, DataProtocol: p})
				if e != nil {
					t.Fatal(e)
				}
				c, e := BindRunValueFormat(WithTenant(t.Context(), "request"), r)
				if e != nil {
					t.Fatal(e)
				}
				b, e := CaptureRunDataBinding(RunDataHeader{RunID: "r", Format: f, Protocol: p, BirthID: r.BirthID})
				if e != nil {
					t.Fatal(e)
				}
				c, e = WithRunDataBinding(c, b)
				if e != nil {
					t.Fatal(e)
				}
				path := filepath.Join(dir, "runs", "r", "run.json")
				raw, e := os.ReadFile(path)
				if e != nil {
					t.Fatal(e)
				}
				// Alter only canonical metadata, never float-decode the business payload.
				raw = append([]byte(`{"tenant_id":"canonical",`), raw[1:]...)
				readerWrite(t, path, raw)
				selected, ok, e := s.OpenFilesystemStream(c, "r", FilesystemStreamOpenOptions{})
				if e != nil || !ok {
					t.Fatal(e, ok)
				}
				defer selected.Close()
				orig, capable, e := CaptureEventStreamObservation(selected)
				if e != nil || !capable || orig.TenantID() != "canonical" || orig.Header().BirthID != r.BirthID {
					t.Fatal(orig, e)
				}
				event := Event{Data: map[string]any{"x": json.Number("17")}, TenantID: "row"}
				if f == 2 {
					event.Data["x"] = json.Number("1e400")
				}
				wire, e := EncodeValueRecordJSON(c, f, ExactEvent, event)
				if e != nil {
					t.Fatal(e)
				}
				legacy, e := selected.DecodeEvent(c, wire)
				if e != nil || legacy.RunID != "" {
					t.Fatal(legacy, e)
				}
				got, e := selected.(ObservedFilesystemEventDecoder).DecodeObservedEvent(c, wire)
				if e != nil {
					t.Fatal(e)
				}
				o, known := got.EventObservation()
				if !known || o != orig || got.RunID != "r" || got.TenantID != "row" {
					t.Fatal(got, o)
				}
				if f == 2 && fmt.Sprint(got.Data["x"]) != "1e400" {
					t.Fatal(got.Data)
				}
				got.RunID = ""
				same, e := EncodeValueRecordJSON(c, f, ExactEvent, *got)
				if e != nil || string(wire) != string(same) {
					t.Fatal("wire differs beyond approved RunID normalization", e)
				}
				changed := append([]byte(`{"tenant_id":"successor",`), raw[len(`{"tenant_id":"canonical",`):]...)
				readerWrite(t, path, changed)
				// Decode is retained and does not reload; Validate refuses changed tenant.
				got, e = selected.(ObservedFilesystemEventDecoder).DecodeObservedEvent(c, wire)
				if e != nil {
					t.Fatal(e)
				}
				o, _ = got.EventObservation()
				if o != orig {
					t.Fatal("adopted successor")
				}
				if e = selected.Validate(c); !errors.Is(e, ErrRunDataIdentityChanged) {
					t.Fatal(e)
				}
			})
		}
	}
}
func TestReaderOriginFilesystemCanonicalTenant(t *testing.T) {
	for _, tc := range []struct {
		name, field string
		valid       bool
	}{{"missing", "", true}, {"empty", `"tenant_id":"",`, true}, {"string", `"tenant_id":"t",`, true}, {"null", `"tenant_id":null,`, false}, {"number", `"tenant_id":3,`, false}, {"duplicate", `"tenant_id":"a","tenant_id":"b",`, false}, {"alias", `"TENANT_ID":"a",`, false}, {"fold", `"tenant_İd":"a",`, false}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			s, e := New(dir)
			if e != nil {
				t.Fatal(e)
			}
			p := filepath.Join(dir, "runs", "r")
			if e = os.MkdirAll(p, 0700); e != nil {
				t.Fatal(e)
			}
			initial := []byte(`{"id":"r"}`)
			readerWrite(t, filepath.Join(p, "run.json"), initial)
			sel, _, e := s.OpenFilesystemStream(t.Context(), "r", FilesystemStreamOpenOptions{})
			if e != nil {
				t.Fatal(e)
			}
			defer sel.Close()
			raw := []byte(`{` + tc.field + `"id":"r","inputs":{"huge":1e400},"status":{}}`)
			readerWrite(t, filepath.Join(p, "run.json"), raw)
			next, _, e := s.OpenFilesystemStream(t.Context(), "r", FilesystemStreamOpenOptions{})
			if tc.valid {
				if e != nil {
					t.Fatal(e)
				}
				next.Close()
			} else if !errors.Is(e, ErrInvalidRunDataIdentity) {
				t.Fatal(e)
			}
			e = sel.Validate(t.Context())
			if !tc.valid {
				if !errors.Is(e, ErrInvalidRunDataIdentity) {
					t.Fatal(e)
				}
			} else if tc.name == "string" {
				if !errors.Is(e, ErrRunDataIdentityChanged) {
					t.Fatal(e)
				}
			} else if e != nil {
				t.Fatal(e)
			}
		})
	}
}
func TestReaderOriginFilesystemOrphanPartitions(t *testing.T) {
	dir := t.TempDir()
	s, e := New(dir)
	if e != nil {
		t.Fatal(e)
	}
	p := filepath.Join(dir, "runs", "r")
	if e = os.MkdirAll(p, 0700); e != nil {
		t.Fatal(e)
	}
	raw := []byte(`{"seq":0,"data":{"same":"bytes"}}`)
	readerWrite(t, filepath.Join(p, "events.jsonl"), append(raw, '\n'))
	var observations []RunObservation
	var selections []FilesystemStreamSelection
	for _, tenant := range []string{"A", "B"} {
		c := WithTenant(t.Context(), tenant)
		sel, ok, e := s.OpenFilesystemStream(c, "r", FilesystemStreamOpenOptions{})
		if e != nil || !ok {
			t.Fatal(e)
		}
		defer sel.Close()
		selections = append(selections, sel)
		fd, e := sel.OpenEvents(c)
		if e != nil {
			t.Fatal(e)
		}
		stored, e := io.ReadAll(fd)
		closeErr := fd.Close()
		if e != nil || closeErr != nil || string(stored) != string(append(append([]byte(nil), raw...), '\n')) {
			t.Fatal("orphan payload not shared", e, closeErr, string(stored))
		}

		evt, e := sel.(ObservedFilesystemEventDecoder).DecodeObservedEvent(c, raw)
		if e != nil {
			t.Fatal(e)
		}
		o, _ := evt.EventObservation()
		if o.ParentPresent() || o.TenantID() != tenant || evt.Data["same"] != "bytes" {
			t.Fatal(o, evt)
		}
		observations = append(observations, o)
		if e = sel.Validate(c); e != nil {
			t.Fatal(e)
		}
	}
	if observations[0] == observations[1] {
		t.Fatal("request partitions collapsed")
	}
	readerWrite(t, filepath.Join(p, "run.json"), []byte(`{"id":"r"}`))
	// Published absence cannot adopt this newly visible parent.
	for i, tenant := range []string{"A", "B"} {
		if e = selections[i].Validate(WithTenant(t.Context(), tenant)); !errors.Is(e, ErrRunDataIdentityChanged) {
			t.Fatal(e)
		}
	}
}

type readerInherited struct {
	*filesystemStreamSelection
	calls int
}

func (r *readerInherited) DecodeEvent(context.Context, []byte) (*Event, error) {
	r.calls++
	return nil, nil
}

type readerPartial struct{}

func (readerPartial) EventStreamObservation() RunObservation { return RunObservation{} }
func TestReaderOriginSelectionReceiver(t *testing.T) {
	o, err := CaptureRunObservation(RunDataHeader{RunID: "r", Format: 1}, "", true)
	if err != nil {
		t.Fatal(err)
	}
	n := &filesystemStreamSelection{observation: o}
	over := &readerInherited{filesystemStreamSelection: n}
	var nilNative *filesystemStreamSelection
	for _, s := range []any{nil, nilNative, over, readerPartial{}} {
		if _, _, e := CaptureEventStreamObservation(s); !errors.Is(e, ErrRunDataCapability) {
			t.Fatal(e)
		}
	}
	if over.calls != 0 {
		t.Fatal(over.calls)
	}
	if _, capable, e := CaptureEventStreamObservation(struct{}{}); e != nil || capable {
		t.Fatal(e, capable)
	}
}
