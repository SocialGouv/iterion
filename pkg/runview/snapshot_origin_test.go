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
