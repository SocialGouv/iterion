package runview

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// snapshotOriginBarrier schedules a mutation between BuildSnapshot's load
// and its scan, right after the first load returns or as the scan begins,
// then delegates both to the wrapped store unchanged. It records what
// reached the reducer; it never applies an identity policy.
type snapshotOriginBarrier struct {
	store.RunStore
	afterLoad  func()
	beforeScan func()
	scanned    int
	seen       string
	exact      string
}

func (s *snapshotOriginBarrier) LoadRun(ctx context.Context, id string) (*store.Run, error) {
	run, err := s.RunStore.LoadRun(ctx, id)
	if hook := s.afterLoad; hook != nil {
		s.afterLoad = nil
		hook()
	}
	return run, err
}

func (s *snapshotOriginBarrier) ScanEvents(ctx context.Context, id string, visit func(*store.Event) bool) error {
	if s.beforeScan != nil {
		s.beforeScan()
	}
	return s.RunStore.ScanEvents(ctx, id, func(e *store.Event) bool {
		s.scanned++
		if marker, ok := e.Data["marker"].(string); ok {
			s.seen = marker
		}
		if n, ok := e.Data["n"]; ok {
			raw, err := json.Marshal(n)
			if err != nil {
				s.exact = "unmarshalable: " + err.Error()
			} else {
				s.exact = string(raw)
			}
		}
		return visit(e)
	})
}

// snapshotOriginModes: "replace" swaps A for B as the scan begins,
// "replace-load" right after the load returns (a re-read of the header
// would adopt B), "missing" deletes A; "empty" and "positive" are the
// legitimate runs that must still snapshot.
var snapshotOriginModes = []string{"replace", "replace-load", "missing", "empty", "positive"}

func TestSnapshotOriginNativeFS(t *testing.T) {
	for _, f := range []store.ValueFormat{1, 2} {
		for _, mode := range snapshotOriginModes {
			t.Run(fmt.Sprintf("f%d/%s", f, mode), func(t *testing.T) {
				s, err := store.New(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				snapshotOriginMatrix(t, s, t.Context(), f, mode)
			})
		}
		t.Run(fmt.Sprintf("f%d/tenant", f), func(t *testing.T) {
			snapshotOriginTenantFS(t, f)
		})
	}
	for _, forge := range []string{"duplicate", "alias"} {
		t.Run("f1/lenient-tenant-"+forge, func(t *testing.T) {
			snapshotOriginLenientTenantFS(t, forge)
		})
	}
}

// fsSnapshotRun creates run r in format f with one event marked "A" and
// returns its store, root and canonical header.
func fsSnapshotRun(t *testing.T, f store.ValueFormat) (*store.FilesystemRunStore, string, store.RunDataHeader) {
	t.Helper()
	root := t.TempDir()
	s, err := store.New(root)
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.CreateRunWithValueFormat(t.Context(), store.RunCreateSpec{ID: "r", WorkflowName: "wf", Format: f})
	if err != nil {
		t.Fatal(err)
	}
	write, err := store.BindRunValueFormat(t.Context(), a)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AppendEvent(write, "r", store.Event{Data: map[string]any{"marker": "A"}}); err != nil {
		t.Fatal(err)
	}
	h, err := store.RunDataHeaderFromJSON(t.Context(), readFSRunJSON(t, root))
	if err != nil {
		t.Fatal(err)
	}
	if h.BirthID == "" || h.BirthID != a.BirthID || h.Format != f {
		t.Fatalf("fixture: canonical header %+v, created birth %q format %d", h, a.BirthID, f)
	}
	return s, root, h
}

func readFSRunJSON(t *testing.T, root string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "runs", "r", "run.json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// fsCanonicalObservation reads run r's header and tenant through the
// filesystem store's canonical parsers: its stream selection's observation.
func fsCanonicalObservation(t *testing.T, s *store.FilesystemRunStore) (store.RunObservation, error) {
	t.Helper()
	sel, observed, err := s.OpenFilesystemStream(t.Context(), "r", store.FilesystemStreamOpenOptions{})
	if err != nil {
		return store.RunObservation{}, err
	}
	defer sel.Close()
	o, ok, err := store.CaptureEventStreamObservation(sel)
	if err != nil || !ok || !observed {
		t.Fatalf("fixture: selection observation %t %t %v", ok, observed, err)
	}
	return o, nil
}

// retenantFSRun sets tenant_id in run r's run.json, every other member kept,
// and asserts the canonical parsers read header h and tenant back.
func retenantFSRun(t *testing.T, s *store.FilesystemRunStore, root, tenant string, h store.RunDataHeader) {
	t.Helper()
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(readFSRunJSON(t, root), &raw); err != nil {
		t.Fatal(err)
	}
	var err error
	if raw["tenant_id"], err = json.Marshal(tenant); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "runs", "r", "run.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	o, err := fsCanonicalObservation(t, s)
	if err != nil || o.Header() != h || o.TenantID() != tenant {
		t.Fatalf("fixture: canonical observation after the rewrite = %+v tenant %q, %v; want header %+v tenant %q", o.Header(), o.TenantID(), err, h, tenant)
	}
}

// snapshotOriginTenantFS: A's run.json records tenant "T" (filesystem
// creation records none, so it is seeded and read back canonically); as the
// scan begins B rewrites only the tenant to "U", A's birth, format and
// protocol kept. BuildSnapshot binds the loaded run's tenant, so the scan
// refuses exactly ErrRunDataIdentityChanged before any event is folded.
func snapshotOriginTenantFS(t *testing.T, f store.ValueFormat) {
	s, root, h := fsSnapshotRun(t, f)
	retenantFSRun(t, s, root, "T", h)
	control := &snapshotOriginBarrier{RunStore: s}
	if snap, err := BuildSnapshot(t.Context(), control, "r"); err != nil || snap == nil || control.scanned != 1 || control.seen != "A" {
		t.Fatalf("control: tenant \"T\" snapshot=%v err=%v scanned=%d marker=%q", snap != nil, err, control.scanned, control.seen)
	}
	wrapped := &snapshotOriginBarrier{RunStore: s, beforeScan: func() { retenantFSRun(t, s, root, "U", h) }}
	snap, err := BuildSnapshot(t.Context(), wrapped, "r")
	if !errors.Is(err, store.ErrRunDataIdentityChanged) || snap != nil || wrapped.scanned != 0 {
		t.Fatalf("tenant rewritten \"T\" -> \"U\" as the scan begins: snapshot=%v err=%v scanned=%d marker=%q, want ErrRunDataIdentityChanged and nothing scanned", snap != nil, err, wrapped.scanned, wrapped.seen)
	}
}

// snapshotOriginLenientTenantFS: run.json carries a duplicated or case-alias
// tenant_id, so the decoded Run.TenantID ("T") and the canonical parser
// disagree. The observation BuildSnapshot and CaptureRunOriginIn capture
// keeps the decoded tenant; every bound native read refuses the record
// typed (ErrInvalidRunDataIdentity) and the run is never snapshotted.
func snapshotOriginLenientTenantFS(t *testing.T, forge string) {
	s, root, h := fsSnapshotRun(t, store.LegacyValueFormat)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(readFSRunJSON(t, root), &raw); err != nil {
		t.Fatal(err)
	}
	delete(raw, "tenant_id")
	body, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	members := body[1 : len(body)-1]
	var forged []byte
	switch forge {
	case "duplicate":
		forged = append(append([]byte(`{"tenant_id":"U",`), members...), []byte(`,"tenant_id":"T"}`)...)
	case "alias":
		forged = append(append([]byte(`{"Tenant_ID":"T",`), members...), '}')
	}
	if err = os.WriteFile(filepath.Join(root, "runs", "r", "run.json"), forged, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := store.RunDataHeaderFromJSON(t.Context(), forged); err != nil || got != h {
		t.Fatalf("fixture: forged header %+v, %v, want %+v", got, err, h)
	}
	if _, err := fsCanonicalObservation(t, s); !errors.Is(err, store.ErrInvalidRunDataIdentity) {
		t.Fatalf("fixture: the canonical parser accepts the %s tenant_id: %v", forge, err)
	}
	if r, err := s.LoadRun(t.Context(), "r"); err != nil || r.TenantID != "T" {
		t.Fatalf("fixture: the decoded tenant of a %s tenant_id = %v, %v, want \"T\"", forge, r, err)
	}

	wrapped := &snapshotOriginBarrier{RunStore: s}
	snap, err := BuildSnapshot(t.Context(), wrapped, "r")
	if !errors.Is(err, store.ErrInvalidRunDataIdentity) || snap != nil || wrapped.scanned != 0 {
		t.Fatalf("BuildSnapshot of a %s tenant_id: snapshot=%v err=%v scanned=%d, want ErrInvalidRunDataIdentity and nothing scanned", forge, snap != nil, err, wrapped.scanned)
	}
	lenientCaptureRefusedOnBoundReads(t, s, "T", forge)
}

// lenientCaptureRefusedOnBoundReads: CaptureRunOriginIn keeps the decoded
// tenant of a record whose canonical tenant is refused; reads bound to that
// observation, team-blind, refuse ErrInvalidRunDataIdentity and deliver
// nothing.
func lenientCaptureRefusedOnBoundReads(t *testing.T, s store.RunStore, decoded, forge string) {
	t.Helper()
	blind := store.WithoutTenantFilter(t.Context())
	o, err := CaptureRunOriginIn(blind, s, "r")
	if err != nil || o.TenantID() != decoded {
		t.Fatalf("CaptureRunOriginIn of a %s tenant_id = tenant %q, %v, want the decoded %q", forge, o.TenantID(), err, decoded)
	}
	bound, err := store.BindRunObservation(blind, o)
	if err != nil {
		t.Fatal(err)
	}
	if r, err := s.LoadRun(bound, "r"); !errors.Is(err, store.ErrInvalidRunDataIdentity) {
		t.Errorf("LoadRun bound to the lenient capture of a %s tenant_id: run %t, %v, want ErrInvalidRunDataIdentity", forge, r != nil, err)
	}
	visited := 0
	if err := s.ScanEvents(bound, "r", func(*store.Event) bool { visited++; return true }); !errors.Is(err, store.ErrInvalidRunDataIdentity) || visited != 0 {
		t.Errorf("ScanEvents bound to the lenient capture of a %s tenant_id: %d events, %v, want ErrInvalidRunDataIdentity", forge, visited, err)
	}
}

type snapshotOriginNative interface {
	store.RunStore
	CreateRunWithValueFormat(context.Context, store.RunCreateSpec) (*store.Run, error)
	PruneDeletionMarkers(context.Context, time.Time) (int, error)
}

// snapshotOriginMatrix loads run A, lets the barrier delete it (and, for
// the replace modes, recreate B under the same ID with its own event),
// then requires the snapshot to refuse rather than fold B's events into A.
// The positive cases require A's own event to reach the reducer, with a
// format-two exact number beyond float64 range intact. Only the replace
// modes witness the binding: a deleted run is refused without it.
func snapshotOriginMatrix(t *testing.T, s snapshotOriginNative, ctx context.Context, f store.ValueFormat, mode string) {
	t.Helper()
	a, err := s.CreateRunWithValueFormat(ctx, store.RunCreateSpec{ID: "r", WorkflowName: "wf", Format: f})
	if err != nil {
		t.Fatal(err)
	}
	write, err := store.BindRunValueFormat(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if mode != "empty" {
		data := map[string]any{"marker": "A"}
		if f == store.ExactValueFormat {
			data["n"] = json.Number("1e400")
		}
		if _, err = s.AppendEvent(write, "r", store.Event{Data: data}); err != nil {
			t.Fatal(err)
		}
	}
	wrapped := &snapshotOriginBarrier{RunStore: s}
	mutate := func() {
		if err := s.DeleteRun(ctx, "r"); err != nil {
			t.Fatal(err)
		}
		if mode == "missing" {
			return
		}
		if _, err := s.PruneDeletionMarkers(ctx, time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		b, err := s.CreateRunWithValueFormat(ctx, store.RunCreateSpec{ID: "r", WorkflowName: "wf", Format: f})
		if err != nil {
			t.Fatal(err)
		}
		bc, err := store.BindRunValueFormat(ctx, b)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.AppendEvent(bc, "r", store.Event{Data: map[string]any{"marker": "B"}}); err != nil {
			t.Fatal(err)
		}
	}
	switch mode {
	case "replace", "missing":
		wrapped.beforeScan = mutate
	case "replace-load":
		wrapped.afterLoad = mutate
	}
	snap, err := BuildSnapshot(ctx, wrapped, "r")
	switch mode {
	case "replace", "replace-load", "missing":
		refused := errors.Is(err, store.ErrRunDataIdentityChanged) || errors.Is(err, store.ErrRunNotFound) || errors.Is(err, store.ErrRunDeleted)
		if snap != nil || !refused {
			t.Fatalf("original lost: snapshot=%v err=%v scanned=%d marker=%s", snap != nil, err, wrapped.scanned, wrapped.seen)
		}
		if wrapped.seen == "B" {
			t.Fatal("successor event reached reducer")
		}
	case "empty":
		if err != nil || snap == nil || wrapped.scanned != 0 {
			t.Fatalf("empty run: snapshot=%v err=%v scanned=%d", snap != nil, err, wrapped.scanned)
		}
	case "positive":
		if err != nil || snap == nil || wrapped.scanned != 1 || wrapped.seen != "A" {
			t.Fatalf("own event lost: snapshot=%v err=%v scanned=%d marker=%q", snap != nil, err, wrapped.scanned, wrapped.seen)
		}
		if f == store.ExactValueFormat && wrapped.exact != "1e400" {
			t.Fatalf("exact number reached the reducer as %q, want 1e400", wrapped.exact)
		}
	}
}

// TestSnapshotOriginScanFailureAfterVisit makes the native scan fail after
// it has already delivered one event to the reducer: format two refuses
// its first undecodable line, format one refuses past its corruption
// threshold. The partially reduced snapshot must not be returned.
func TestSnapshotOriginScanFailureAfterVisit(t *testing.T) {
	for _, f := range []store.ValueFormat{1, 2} {
		t.Run(fmt.Sprintf("f%d", f), func(t *testing.T) {
			root := t.TempDir()
			s, err := store.New(root)
			if err != nil {
				t.Fatal(err)
			}
			a, err := s.CreateRunWithValueFormat(t.Context(), store.RunCreateSpec{ID: "r", WorkflowName: "wf", Format: f})
			if err != nil {
				t.Fatal(err)
			}
			write, err := store.BindRunValueFormat(t.Context(), a)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.AppendEvent(write, "r", store.Event{Data: map[string]any{"marker": "A"}}); err != nil {
				t.Fatal(err)
			}
			events, err := os.OpenFile(filepath.Join(root, "runs", "r", "events.jsonl"), os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = events.WriteString("{not json\n{not json\n"); err != nil {
				t.Fatal(err)
			}
			if err = events.Close(); err != nil {
				t.Fatal(err)
			}
			wrapped := &snapshotOriginBarrier{RunStore: s}
			snap, err := BuildSnapshot(t.Context(), wrapped, "r")
			if wrapped.scanned != 1 || wrapped.seen != "A" {
				t.Fatalf("the failure did not follow a delivered event: scanned=%d marker=%q err=%v", wrapped.scanned, wrapped.seen, err)
			}
			if snap != nil || err == nil {
				t.Fatalf("partial snapshot escaped a scan failure: snapshot=%v err=%v", snap != nil, err)
			}
		})
	}
}

type snapshotOriginCustom struct {
	store.RunStore
	run   *store.Run
	scans int
}

func (s *snapshotOriginCustom) LoadRun(context.Context, string) (*store.Run, error) {
	return s.run, nil
}

func (s *snapshotOriginCustom) ScanEvents(context.Context, string, func(*store.Event) bool) error {
	s.scans++
	return nil
}

// TestSnapshotOriginCustomContract pins what a store outside the native
// guarantee still gets: its own run snapshots as before, while metadata
// naming another run (or none) is refused before any event is scanned.
func TestSnapshotOriginCustomContract(t *testing.T) {
	for _, id := range []string{"r", "", "other"} {
		t.Run("id="+id, func(t *testing.T) {
			s := &snapshotOriginCustom{run: &store.Run{ID: id, WorkflowName: "wf"}}
			snap, err := BuildSnapshot(t.Context(), s, "r")
			if id != "r" {
				if !errors.Is(err, store.ErrValueFormatChanged) || snap != nil || s.scans != 0 {
					t.Fatalf("foreign metadata must refuse before scan: err=%v snapshot=%v scans=%d", err, snap != nil, s.scans)
				}
				return
			}
			if err != nil || snap == nil || s.scans != 1 {
				t.Fatalf("own run: err=%v snapshot=%v scans=%d", err, snap != nil, s.scans)
			}
		})
	}
}

func TestSnapshotOriginExistingIntentAndCancel(t *testing.T) {
	for _, mode := range []string{"format", "birth", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			s, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.CreateRun(t.Context(), "r", "wf", nil); err != nil {
				t.Fatal(err)
			}
			ctx := t.Context()
			switch mode {
			case "format":
				ctx, err = store.WithRunValueFormat(ctx, store.RunValueFormat{RunID: "r", Format: 2})
			case "birth":
				var b store.RunDataBinding
				b, err = store.CaptureRunDataBinding(store.RunDataHeader{RunID: "r", Format: 1, BirthID: "wrong"})
				if err == nil {
					ctx, err = store.WithRunDataBinding(ctx, b)
				}
			case "cancel":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if err != nil {
				t.Fatal(err)
			}
			snap, err := BuildSnapshot(ctx, s, "r")
			if snap != nil || err == nil {
				t.Fatal("existing intent adopted", mode)
			}
			if mode == "format" && !errors.Is(err, store.ErrValueFormatChanged) {
				t.Fatal(err)
			}
			if mode == "birth" && !errors.Is(err, store.ErrRunDataIdentityChanged) {
				t.Fatal(err)
			}
			if mode == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		})
	}
}
