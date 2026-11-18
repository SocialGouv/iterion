package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"
)

// observedTenantFixture creates run id in format, appends events and run.log
// bytes while the run records no tenant, then seeds tenant into its raw
// run.json (filesystem creation records none). It returns the store, the
// canonical header of the run and a ctx bound to its observation — header and
// tenant both read by the canonical parsers from the seeded bytes.
func observedTenantFixture(t *testing.T, format ValueFormat, id, tenant string, events int) (*FilesystemRunStore, RunDataHeader, context.Context) {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateRunWithValueFormat(t.Context(), RunCreateSpec{ID: id, WorkflowName: "A", Format: format}); err != nil {
		t.Fatal(err)
	}
	writer, err := WithRunValueFormat(t.Context(), RunValueFormat{RunID: id, Format: format})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < events; i++ {
		if _, err = s.AppendEvent(writer, id, Event{Type: "tenant_witness", Data: map[string]any{"i": "v"}}); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.AppendRunLog(writer, id, 0, []byte("original log")); err != nil {
		t.Fatal(err)
	}
	h := canonicalRunJSONHeader(t, s, id)
	if got := rewriteRunJSONTenant(t, s, id, tenant); got != h {
		t.Fatalf("fixture: seeding the tenant changed the header %+v -> %+v", h, got)
	}
	o, err := CaptureRunObservation(h, tenant, true)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := BindRunObservation(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	return s, h, bound
}

func canonicalRunJSONHeader(t *testing.T, s *FilesystemRunStore, id string) RunDataHeader {
	t.Helper()
	raw, err := os.ReadFile(s.runJSONPath(id))
	if err != nil {
		t.Fatal(err)
	}
	h, err := RunDataHeaderFromJSON(t.Context(), raw)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// rewriteRunJSONTenant sets tenant_id in id's run.json, every other member kept
// as written, asserts the canonical parser reads that tenant back and returns
// the canonical header of the rewritten bytes.
func rewriteRunJSONTenant(t *testing.T, s *FilesystemRunStore, id, tenant string) RunDataHeader {
	t.Helper()
	b, err := os.ReadFile(s.runJSONPath(id))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err = json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	if raw["tenant_id"], err = json.Marshal(tenant); err != nil {
		t.Fatal(err)
	}
	if b, err = json.Marshal(raw); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(s.runJSONPath(id), b, filePerm); err != nil {
		t.Fatal(err)
	}
	if got, err := streamCanonicalTenant(t.Context(), b); err != nil || got != tenant {
		t.Fatalf("fixture: canonical tenant of the rewritten run.json = %q, %v, want %q", got, err, tenant)
	}
	return canonicalRunJSONHeader(t, s, id)
}

func loadedTenant(r *Run) string {
	if r == nil {
		return "<no run>"
	}
	return r.TenantID
}

func scanCount(ctx context.Context, s *FilesystemRunStore, id string) (int, error) {
	n := 0
	err := s.ScanEvents(ctx, id, func(*Event) bool { n++; return true })
	return n, err
}

// TestFilesystemBoundReadsRefuseAnotherTenant: a run whose run.json records
// tenant "T" is bound; the file is rewritten to tenant "U" with the same birth,
// format and protocol, so every header compare still agrees. Each bound native
// read refuses ErrRunDataIdentityChanged and delivers nothing.
func TestFilesystemBoundReadsRefuseAnotherTenant(t *testing.T) {
	t.Parallel()
	const id = "r"
	s, h, bound := observedTenantFixture(t, ExactValueFormat, id, "T", 3)
	if h.BirthID == "" || h.Format != ExactValueFormat {
		t.Fatalf("fixture: header %+v has no birth or is not exact", h)
	}
	if r, err := s.LoadRun(bound, id); err != nil || r.TenantID != "T" {
		t.Fatalf("control: bound load of the observed run = tenant %q, %v", loadedTenant(r), err)
	}
	if n, err := scanCount(bound, s, id); err != nil || n != 3 {
		t.Fatalf("control: bound scan of the observed run = %d, %v", n, err)
	}
	if got, err := s.ReadRunLogRange(bound, id, 0, 0); err != nil || string(got) != "original log" {
		t.Fatalf("control: bound run.log of the observed run = %q, %v", got, err)
	}

	if got := rewriteRunJSONTenant(t, s, id, "U"); got != h {
		t.Fatalf("fixture: the forged successor changed the header %+v -> %+v; the header path would refuse first", h, got)
	}

	if r, err := s.LoadRun(bound, id); !errors.Is(err, ErrRunDataIdentityChanged) {
		t.Errorf("LoadRun: bound to tenant \"T\", run.json records \"U\": loaded tenant %q, %v, want ErrRunDataIdentityChanged", loadedTenant(r), err)
	}
	if n, err := scanCount(bound, s, id); !errors.Is(err, ErrRunDataIdentityChanged) || n != 0 {
		t.Errorf("ScanEvents: bound to tenant \"T\", run.json records \"U\": %d events, %v, want 0 and ErrRunDataIdentityChanged", n, err)
	}
	if got, err := s.ReadRunLogRange(bound, id, 0, 0); !errors.Is(err, ErrRunDataIdentityChanged) || got != nil {
		t.Errorf("run.log: bound to tenant \"T\", run.json records \"U\": %q, %v, want ErrRunDataIdentityChanged", got, err)
	}
	if got, err := s.ObserveRunDataHeader(bound, id); !errors.Is(err, ErrRunDataIdentityChanged) {
		t.Errorf("ObserveRunDataHeader: bound to tenant \"T\", run.json records \"U\": %+v, %v, want ErrRunDataIdentityChanged", got, err)
	}
	sel, observed, err := s.OpenFilesystemStream(bound, id, FilesystemStreamOpenOptions{})
	if sel != nil {
		sel.Close()
	}
	if !errors.Is(err, ErrRunDataIdentityChanged) || sel != nil || observed {
		t.Errorf("OpenFilesystemStream: bound to tenant \"T\", run.json records \"U\": selection %t observed %t, %v, want ErrRunDataIdentityChanged", sel != nil, observed, err)
	}
}

// TestFilesystemStreamOperationComparesTheBoundTenant: a selection retained
// unbound serves an operation whose ctx observed another tenant for the same
// header only after comparing that tenant with the run.json it re-reads.
func TestFilesystemStreamOperationComparesTheBoundTenant(t *testing.T) {
	t.Parallel()
	const id = "r"
	s, h, boundT := observedTenantFixture(t, ExactValueFormat, id, "T", 1)
	sel, observed, err := s.OpenFilesystemStream(t.Context(), id, FilesystemStreamOpenOptions{})
	if err != nil || !observed {
		t.Fatalf("unbound open: %v %t", err, observed)
	}
	defer sel.Close()
	if o := sel.(EventObservationSelection).EventStreamObservation(); o.Header() != h || o.TenantID() != "T" {
		t.Fatalf("fixture: selection observed %+v tenant %q", o.Header(), o.TenantID())
	}
	f, err := sel.OpenEvents(boundT)
	if err != nil {
		t.Fatalf("control: operation bound to the selection's tenant: %v", err)
	}
	f.Close()
	oU, err := CaptureRunObservation(h, "U", true)
	if err != nil {
		t.Fatal(err)
	}
	boundU, err := BindRunObservation(t.Context(), oU)
	if err != nil {
		t.Fatal(err)
	}
	if f, err := sel.OpenEvents(boundU); !errors.Is(err, ErrRunDataIdentityChanged) {
		if f != nil {
			f.Close()
		}
		t.Fatalf("OpenEvents: ctx observed tenant \"U\", run.json records \"T\": %v, want ErrRunDataIdentityChanged", err)
	}
	if err := sel.Validate(boundU); !errors.Is(err, ErrRunDataIdentityChanged) {
		t.Fatalf("Validate: ctx observed tenant \"U\", run.json records \"T\": %v, want ErrRunDataIdentityChanged", err)
	}
}

// TestFilesystemTenantRewriteUnboundReadsAsBefore: without an observed tenant
// the same rewrite reads as the file says — unbound and header-only bound.
func TestFilesystemTenantRewriteUnboundReadsAsBefore(t *testing.T) {
	t.Parallel()
	const id = "r"
	s, h, _ := observedTenantFixture(t, ExactValueFormat, id, "T", 2)
	if got := rewriteRunJSONTenant(t, s, id, "U"); got != h {
		t.Fatalf("fixture: header changed %+v -> %+v", h, got)
	}
	headerOnly, err := CaptureRunDataBinding(h)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := WithRunDataBinding(t.Context(), headerOnly)
	if err != nil {
		t.Fatal(err)
	}
	if legacy, err = WithRunValueFormat(legacy, RunValueFormat{RunID: id, Format: h.Format}); err != nil {
		t.Fatal(err)
	}
	for name, ctx := range map[string]context.Context{"unbound": t.Context(), "header-only": legacy} {
		if r, err := s.LoadRun(ctx, id); err != nil || r.TenantID != "U" {
			t.Fatalf("%s LoadRun = tenant %q, %v, want the file's tenant \"U\"", name, loadedTenant(r), err)
		}
		if n, err := scanCount(ctx, s, id); err != nil || n != 2 {
			t.Fatalf("%s ScanEvents = %d, %v, want 2 events", name, n, err)
		}
		if got, err := s.ReadRunLogRange(ctx, id, 0, 0); err != nil || string(got) != "original log" {
			t.Fatalf("%s run.log = %q, %v", name, got, err)
		}
		if got, err := s.ObserveRunDataHeader(ctx, id); err != nil || got != h {
			t.Fatalf("%s ObserveRunDataHeader = %+v, %v", name, got, err)
		}
	}
}

// TestFilesystemBoundTenantUsesTheCanonicalParser: under an observed tenant a
// run.json whose tenant_id is duplicated, or spelled as a case alias, is an
// invalid identity — a lenient decode would read the observed "T" and admit
// it. Unbound reads keep the legacy decode.
func TestFilesystemBoundTenantUsesTheCanonicalParser(t *testing.T) {
	t.Parallel()
	for name, wrap := range map[string]func(body []byte) []byte{
		"duplicate": func(body []byte) []byte {
			return append(append([]byte(`{"tenant_id":"U",`), body[1:len(body)-1]...), []byte(`,"tenant_id":"T"}`)...)
		},
		"alias": func(body []byte) []byte {
			return append(append([]byte(`{"Tenant_ID":"T",`), body[1:len(body)-1]...), '}')
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			const id = "r"
			s, h, bound := observedTenantFixture(t, LegacyValueFormat, id, "T", 0)
			b, err := os.ReadFile(s.runJSONPath(id))
			if err != nil {
				t.Fatal(err)
			}
			var raw map[string]json.RawMessage
			if err = json.Unmarshal(b, &raw); err != nil {
				t.Fatal(err)
			}
			delete(raw, "tenant_id")
			if b, err = json.Marshal(raw); err != nil {
				t.Fatal(err)
			}
			forged := wrap(b)
			if !bytes.HasPrefix(forged, []byte("{")) {
				t.Fatal("fixture: not an object")
			}
			if got, err := RunDataHeaderFromJSON(t.Context(), forged); err != nil || got != h {
				t.Fatalf("fixture: forged header %+v, %v, want %+v", got, err, h)
			}
			if _, err := streamCanonicalTenant(t.Context(), forged); !errors.Is(err, ErrInvalidRunDataIdentity) {
				t.Fatalf("fixture: the canonical parser accepts the forged tenant: %v", err)
			}
			var lenient struct {
				TenantID string `json:"tenant_id"`
			}
			if err := json.Unmarshal(forged, &lenient); err != nil || lenient.TenantID != "T" {
				t.Fatalf("fixture: a lenient decode reads %q, %v, want the observed \"T\"", lenient.TenantID, err)
			}
			if err = os.WriteFile(s.runJSONPath(id), forged, filePerm); err != nil {
				t.Fatal(err)
			}
			if r, err := s.LoadRun(bound, id); !errors.Is(err, ErrInvalidRunDataIdentity) {
				t.Fatalf("bound LoadRun of a %s tenant_id = tenant %q, %v, want ErrInvalidRunDataIdentity", name, loadedTenant(r), err)
			}
			if r, err := s.LoadRun(t.Context(), id); err != nil || r.TenantID != "T" {
				t.Fatalf("unbound LoadRun of a %s tenant_id = tenant %q, %v, want the legacy decode", name, loadedTenant(r), err)
			}
		})
	}
}

// TestFilesystemBoundAbsenceAnswersPerPath: once the observed run is gone, a
// birth-bearing binding answers per read path — LoadRun ErrRunNotFound, the
// scan and payload opens ErrRunDataIdentityChanged — and a tombstone answers
// ErrRunDeleted where the path checks it.
func TestFilesystemBoundAbsenceAnswersPerPath(t *testing.T) {
	t.Parallel()
	const id = "r"
	for _, prune := range []bool{true, false} {
		s, _, bound := observedTenantFixture(t, ExactValueFormat, id, "T", 1)
		if err := s.UpdateRunStatus(t.Context(), id, RunStatusFinished, ""); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteRun(t.Context(), id); err != nil {
			t.Fatal(err)
		}
		if prune {
			if _, err := s.PruneDeletionMarkers(t.Context(), time.Now().Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
		}
		load, scan, payload := ErrRunNotFound, ErrRunDataIdentityChanged, ErrRunDataIdentityChanged
		if !prune {
			load, scan = ErrRunDeleted, ErrRunDeleted
		}
		if _, err := s.LoadRun(bound, id); !errors.Is(err, load) {
			t.Fatalf("pruned=%t LoadRun: %v, want %v", prune, err, load)
		}
		if n, err := scanCount(bound, s, id); !errors.Is(err, scan) || n != 0 {
			t.Fatalf("pruned=%t ScanEvents: %d, %v, want %v", prune, n, err, scan)
		}
		if _, err := s.ReadRunLogRange(bound, id, 0, 0); !errors.Is(err, payload) {
			t.Fatalf("pruned=%t run.log: %v, want %v", prune, err, payload)
		}
	}
}
