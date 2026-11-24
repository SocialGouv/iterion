package runstream

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/SocialGouv/iterion/pkg/store"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type legacyCursorTestSource struct{ calls []int64 }

func (s *legacyCursorTestSource) SubscribeEvents(_ context.Context, _ string, n int64) (EventSubscription, error) {
	s.calls = append(s.calls, n)
	return nil, nil
}
func (s *legacyCursorTestSource) SubscribeLogs(context.Context, string, int64) (LogSubscription, error) {
	return nil, nil
}
func (s *legacyCursorTestSource) Close() error { return nil }
func TestEventCursorLegacyDispatch(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		c           EventCursor
		want        int64
		unsupported bool
	}{{EventCursor{Seq: math.MinInt64}, math.MinInt64, false}, {EventCursor{Seq: math.MinInt64, After: true}, math.MinInt64 + 1, false}, {EventCursor{Seq: math.MaxInt64}, math.MaxInt64, false}, {EventCursor{Seq: math.MaxInt64, After: true}, 0, true}, {EventCursor{Seq: 0, After: true}, 1, false}} {
		s := &legacyCursorTestSource{}
		_, e := SubscribeEventsCursor(t.Context(), s, "r", tc.c)
		if tc.unsupported {
			if !errors.Is(e, ErrEventCursorUnsupported) || len(s.calls) != 0 {
				t.Fatal(tc, e, s.calls)
			}
		} else if e != nil || len(s.calls) != 1 || s.calls[0] != tc.want {
			t.Fatal(tc, e, s.calls)
		}
	}
}
func cursorTestStore(t *testing.T, seqs ...int64) (*store.FilesystemRunStore, string) {
	t.Helper()
	_, st, root := newFileSourceFixture(t)
	if _, e := st.CreateRun(t.Context(), "r", "wf", nil); e != nil {
		t.Fatal(e)
	}
	var raw []byte
	for _, seq := range seqs {
		line, e := json.Marshal(store.Event{RunID: "r", Seq: seq, Type: store.EventNodeStarted})
		if e != nil {
			t.Fatal(e)
		}
		raw = append(append(raw, line...), '\n')
	}
	if e := os.WriteFile(filepath.Join(root, "runs", "r", "events.jsonl"), raw, 0600); e != nil {
		t.Fatal(e)
	}
	return st, root
}
func TestReplayCursorBoundaries(t *testing.T) {
	restore := SetMaxEventsPerPageForTest(1)
	defer restore()
	for _, tc := range []struct {
		name string
		seqs []int64
		c    EventCursor
		want []int64
	}{{"empty-min", nil, EventCursor{Seq: math.MinInt64}, nil}, {"inclusive-min", []int64{0}, EventCursor{Seq: math.MinInt64}, []int64{0}}, {"full-max", []int64{math.MaxInt64}, EventCursor{Seq: math.MaxInt64}, []int64{math.MaxInt64}}, {"after-max", []int64{math.MaxInt64}, EventCursor{Seq: math.MaxInt64, After: true}, nil}, {"after-zero", []int64{0, 1}, EventCursor{Seq: 0, After: true}, []int64{1}}} {
		t.Run(tc.name, func(t *testing.T) {
			st, _ := cursorTestStore(t, tc.seqs...)
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			pipe := NewEventPipe()
			p, e := ReplayEventsCursor(ctx, st, "r", tc.c, pipe)
			pipe.Finish(nil)
			if e != nil {
				t.Fatal(e)
			}
			var got []int64
			for rows := range pipe.Events() {
				for _, row := range rows {
					got = append(got, row.Seq)
				}
			}
			if len(got) != len(tc.want) {
				t.Fatal("records", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatal(got, tc.want)
				}
			}
			if len(got) == 0 {
				if p.Found || p.Cursor != tc.c {
					t.Fatal(p)
				}
			} else if !p.Found || p.LastSeq != got[len(got)-1] || p.Cursor != (EventCursor{Seq: p.LastSeq, After: true}) {
				t.Fatal(p)
			}
		})
	}
}
func TestReplayLegacyEmptyMinIsExplicit(t *testing.T) {
	t.Parallel()
	st, _ := cursorTestStore(t)
	pipe := NewEventPipe()
	defer pipe.Close()
	_, e := ReplayEvents(t.Context(), st, "r", math.MinInt64, pipe)
	if !errors.Is(e, ErrReplayProgressUnrepresentable) {
		t.Fatal(e)
	}
	if e = st.DeleteRun(t.Context(), "r"); e != nil {
		t.Fatal(e)
	}
	_, e = ReplayEvents(t.Context(), st, "r", math.MinInt64, pipe)
	if !errors.Is(e, ErrReplayProgressUnrepresentable) || !errors.Is(e, store.ErrRunDeleted) {
		t.Fatal("original refusal lost", e)
	}
}
func TestReplayCursorFailedShipKeepsPosition(t *testing.T) {
	t.Parallel()
	st, _ := cursorTestStore(t, 0)
	pipe := NewEventPipe()
	for i := 0; i < cap(pipe.ch); i++ {
		pipe.ch <- nil
	}
	pipe.Close()
	c := EventCursor{Seq: 0}
	p, _ := ReplayEventsCursor(t.Context(), st, "r", c, pipe)
	if p.Found || p.Cursor != c {
		t.Fatal("unshipped progress", p)
	}
}
func TestFileSourceCursorMaxDoesNotEndLiveTail(t *testing.T) {
	t.Parallel()
	for _, after := range []bool{false, true} {
		st, root := cursorTestStore(t, math.MaxInt64)
		src := NewFileSource(st, root, quietLogger())
		sub, e := src.SubscribeEventsCursor(t.Context(), "r", EventCursor{Seq: math.MaxInt64, After: after})
		if e != nil {
			t.Fatal(e)
		}
		if !after {
			select {
			case rows := <-sub.Events():
				if len(rows) != 1 || rows[0].Seq != math.MaxInt64 {
					t.Fatal(rows)
				}
			case <-time.After(time.Second):
				t.Fatal("inclusive max missing")
			}
		}
		select {
		case rows, ok := <-sub.Events():
			t.Fatal("after boundary must stay live without duplicate", rows, ok)
		case <-time.After(30 * time.Millisecond):
		}
		if e = sub.Close(); e != nil {
			t.Fatal(e)
		}
		if e = src.Close(); e != nil {
			t.Fatal(e)
		}
	}
}
