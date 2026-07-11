package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// PlanTodo is one entry in an agent's living TODO plan — the normalized
// shape captured from a TodoWrite (claude_code) / todo_write (claw) tool
// call. Both backends put their items under a top-level `todos` array but
// diverge on the item fields (claude_code carries `activeForm`, claw
// carries `id`+`priority`); this struct is the union, with Status
// canonicalised to the claude_code vocabulary (pending | in_progress |
// completed) so the studio renders both identically.
type PlanTodo struct {
	Content    string `json:"content"`
	Status     string `json:"status"`
	ActiveForm string `json:"active_form,omitempty"`
	Priority   string `json:"priority,omitempty"`
	ID         string `json:"id,omitempty"`
}

// PlanSnapshot is one persisted snapshot of an agent's plan at the moment
// a TodoWrite/todo_write tool call fired. Snapshots are chronological and
// per-run — the sequence of them shows how the plan evolved across the
// run's nodes and loop iterations.
type PlanSnapshot struct {
	Seq       int        `json:"seq"`
	NodeID    string     `json:"node_id"`
	Iteration int        `json:"iteration"`
	Tool      string     `json:"tool,omitempty"`
	Timestamp time.Time  `json:"ts"`
	Todos     []PlanTodo `json:"todos"`
}

// PlanStore is an optional interface implemented by stores that persist
// the chronological plan snapshots a run's agents produce via their
// TodoWrite/todo_write tool. The filesystem store backs it with
// runs/<id>/plans/<NNNN>.json (zero-padded sequence); the Mongo (cloud)
// store backs it with the run_plans collection (one doc per snapshot,
// keyed run_id + seq). The capture hook still nil-checks so a store
// without the seam simply skips persistence.
//
// Persistence is best-effort: the caller (the tool-started hook) must
// never fail an in-flight LLM call on a plan-write error.
type PlanStore interface {
	// AppendPlanSnapshot persists snap as the next chronological plan
	// snapshot for the run, assigning snap.Seq itself (callers leave it
	// zero). It DEDUPS: when the incoming Todos are byte-identical to the
	// immediately previous snapshot's Todos, nothing is written and
	// (previous, false, nil) is returned — TodoWrite fires often with no
	// change. Otherwise the snapshot is written and (snap, true, nil) is
	// returned with Seq populated.
	AppendPlanSnapshot(ctx context.Context, runID string, snap PlanSnapshot) (PlanSnapshot, bool, error)
	// ListPlanSnapshots returns every persisted plan snapshot for the run
	// in chronological (ascending Seq) order. A run with no plans/ dir
	// yields (nil, nil).
	ListPlanSnapshots(ctx context.Context, runID string) ([]PlanSnapshot, error)
}

// AsPlanStore returns s as PlanStore when the backend persists plan
// snapshots, or nil otherwise. Both the filesystem store and the Mongo
// (cloud) store satisfy it — the seam mirrors RunLogStore / ToolBlobStore,
// so the capture hook and the HTTP surface are backend-agnostic. Callers
// MUST nil-check (a store without the seam, or a nil store).
func AsPlanStore(s RunStore) PlanStore {
	if s == nil {
		return nil
	}
	p, _ := s.(PlanStore)
	return p
}

// plansDir returns <root>/runs/<runID>/plans after validating runID.
func (s *FilesystemRunStore) plansDir(runID string) (string, error) {
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return "", err
	}
	return filepath.Join(s.root, "runs", runID, "plans"), nil
}

// AppendPlanSnapshot implements PlanStore over runs/<id>/plans/<NNNN>.json.
// Holds s.mu across the read-max-seq + dedup-compare + write so parallel
// branches firing TodoWrite concurrently can't collide on a sequence
// number or race the dedup check.
func (s *FilesystemRunStore) AppendPlanSnapshot(_ context.Context, runID string, snap PlanSnapshot) (PlanSnapshot, bool, error) {
	dir, err := s.plansDir(runID)
	if err != nil {
		return PlanSnapshot{}, false, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	seqs, err := planSeqs(dir)
	if err != nil {
		return PlanSnapshot{}, false, err
	}

	// Dedup against the immediately-previous snapshot's todos.
	newTodos, err := json.Marshal(snap.Todos)
	if err != nil {
		return PlanSnapshot{}, false, fmt.Errorf("store: marshal plan todos: %w", err)
	}
	nextSeq := 0
	if len(seqs) > 0 {
		last := seqs[len(seqs)-1]
		prev, perr := readPlanFile(dir, last)
		if perr == nil {
			if prevTodos, merr := json.Marshal(prev.Todos); merr == nil && bytes.Equal(prevTodos, newTodos) {
				return prev, false, nil
			}
		}
		nextSeq = last + 1
	}

	snap.Seq = nextSeq
	if snap.Timestamp.IsZero() {
		snap.Timestamp = time.Now().UTC()
	}
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return PlanSnapshot{}, false, fmt.Errorf("store: marshal plan snapshot: %w", err)
	}
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return PlanSnapshot{}, false, fmt.Errorf("store: mkdir plans: %w", err)
	}
	p := filepath.Join(dir, fmt.Sprintf("%04d.json", nextSeq))
	if err := writeFileAtomic(p, data, filePerm); err != nil {
		return PlanSnapshot{}, false, err
	}
	return snap, true, nil
}

// ListPlanSnapshots implements PlanStore: every snapshot under
// runs/<id>/plans in ascending Seq order.
func (s *FilesystemRunStore) ListPlanSnapshots(_ context.Context, runID string) ([]PlanSnapshot, error) {
	dir, err := s.plansDir(runID)
	if err != nil {
		return nil, err
	}
	seqs, err := planSeqs(dir)
	if err != nil {
		return nil, err
	}
	if len(seqs) == 0 {
		return nil, nil
	}
	out := make([]PlanSnapshot, 0, len(seqs))
	for _, seq := range seqs {
		snap, rerr := readPlanFile(dir, seq)
		if rerr != nil {
			// Skip an unreadable/corrupt snapshot rather than failing the
			// whole listing — a partial plan history is more useful than
			// none, matching the store's skip-corrupt event semantics.
			continue
		}
		out = append(out, snap)
	}
	return out, nil
}

// planSeqs returns the sorted sequence numbers of the <NNNN>.json plan
// files in dir. A missing dir is (nil, nil).
func planSeqs(dir string) ([]int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: read plans dir: %w", err)
	}
	seqs := make([]int, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		n, perr := strconv.Atoi(strings.TrimSuffix(name, ".json"))
		if perr != nil {
			continue
		}
		seqs = append(seqs, n)
	}
	sort.Ints(seqs)
	return seqs, nil
}

// readPlanFile reads and decodes one plan snapshot by sequence number.
func readPlanFile(dir string, seq int) (PlanSnapshot, error) {
	p := filepath.Join(dir, fmt.Sprintf("%04d.json", seq))
	data, err := os.ReadFile(p)
	if err != nil {
		return PlanSnapshot{}, fmt.Errorf("store: read plan snapshot: %w", err)
	}
	var snap PlanSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return PlanSnapshot{}, fmt.Errorf("store: decode plan snapshot: %w", err)
	}
	return snap, nil
}
