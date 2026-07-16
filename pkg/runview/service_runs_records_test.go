package runview

import (
	"context"
	"errors"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

type listRecordsContextKey struct{}

type listRecordsStore struct {
	store.RunStore
	ids          []string
	runs         map[string]*store.Run
	events       map[string][]*store.Event
	loadCalls    map[string]int
	contextValue string
	contextsOK   bool
}

func (s *listRecordsStore) checkContext(ctx context.Context) {
	if ctx.Value(listRecordsContextKey{}) != s.contextValue {
		s.contextsOK = false
	}
}

func (s *listRecordsStore) ListRuns(ctx context.Context) ([]string, error) {
	s.checkContext(ctx)
	return append([]string(nil), s.ids...), nil
}

func (s *listRecordsStore) LoadRun(ctx context.Context, id string) (*store.Run, error) {
	s.checkContext(ctx)
	s.loadCalls[id]++
	r, ok := s.runs[id]
	if !ok {
		return nil, errors.New("corrupt run")
	}
	return r, nil
}

func (s *listRecordsStore) ScanEvents(ctx context.Context, runID string, visit func(*store.Event) bool) error {
	s.checkContext(ctx)
	for _, event := range s.events[runID] {
		if !visit(event) {
			break
		}
	}
	return nil
}

func newListRecordsStore() *listRecordsStore {
	base := time.Date(2026, 7, 14, 10, 0, 0, 0, time.UTC)
	run := func(id string, age time.Duration, status store.RunStatus) *store.Run {
		return &store.Run{
			ID:        id,
			Status:    status,
			CreatedAt: base.Add(age),
			UpdatedAt: base.Add(age),
		}
	}
	nodeStarted := func(nodeID string) []*store.Event {
		return []*store.Event{{Type: store.EventNodeStarted, NodeID: nodeID}}
	}
	return &listRecordsStore{
		ids: []string{"older", "wrong-status", "newer", "bad", "no-node", "middle"},
		runs: map[string]*store.Run{
			"older":        run("older", time.Minute, store.RunStatusFinished),
			"wrong-status": run("wrong-status", 5*time.Minute, store.RunStatusFailed),
			"newer":        run("newer", 4*time.Minute, store.RunStatusFinished),
			"no-node":      run("no-node", 6*time.Minute, store.RunStatusFinished),
			"middle":       run("middle", 3*time.Minute, store.RunStatusFinished),
		},
		events: map[string][]*store.Event{
			"older":        nodeStarted("review"),
			"wrong-status": nodeStarted("review"),
			"newer":        nodeStarted("review"),
			"no-node":      nodeStarted("other"),
			"middle":       nodeStarted("review"),
		},
		loadCalls:    make(map[string]int),
		contextValue: "tenant-a",
		contextsOK:   true,
	}
}

func TestListRunRecordsCtxPreservesListSemantics(t *testing.T) {
	ctx := context.WithValue(context.Background(), listRecordsContextKey{}, "tenant-a")
	filter := ListFilter{Status: store.RunStatusFinished, Node: "review", Limit: 2}

	t.Run("records", func(t *testing.T) {
		st := newListRecordsStore()
		svc := &Service{store: st, logger: iterlog.Nop(), manager: NewManager()}

		got, err := svc.ListRunRecordsCtx(ctx, filter)
		if err != nil {
			t.Fatalf("ListRunRecordsCtx: %v", err)
		}
		if len(got) != 2 || got[0].ID != "newer" || got[1].ID != "middle" {
			t.Fatalf("records = %v, want [newer middle]", runRecordIDs(got))
		}
		assertSingleListTraversal(t, st)
	})

	t.Run("summaries", func(t *testing.T) {
		st := newListRecordsStore()
		svc := &Service{store: st, logger: iterlog.Nop(), manager: NewManager()}

		got, err := svc.ListCtx(ctx, filter)
		if err != nil {
			t.Fatalf("ListCtx: %v", err)
		}
		if len(got) != 2 || got[0].ID != "newer" || got[1].ID != "middle" {
			t.Fatalf("summaries = %v, want [newer middle]", runSummaryIDs(got))
		}
		assertSingleListTraversal(t, st)
	})
}

func assertSingleListTraversal(t *testing.T, st *listRecordsStore) {
	t.Helper()
	if !st.contextsOK {
		t.Fatal("listing did not propagate the caller context to every store operation")
	}
	for _, id := range st.ids {
		if got := st.loadCalls[id]; got != 1 {
			t.Errorf("LoadRun(%q) calls = %d, want 1", id, got)
		}
	}
}

func runRecordIDs(runs []*store.Run) []string {
	ids := make([]string, 0, len(runs))
	for _, run := range runs {
		ids = append(ids, run.ID)
	}
	return ids
}

func runSummaryIDs(runs []RunSummary) []string {
	ids := make([]string, 0, len(runs))
	for _, run := range runs {
		ids = append(ids, run.ID)
	}
	return ids
}
