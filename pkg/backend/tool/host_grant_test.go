package tool

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native/boardops"
	"github.com/SocialGouv/iterion/pkg/runops"
	"github.com/SocialGouv/iterion/pkg/store"
)

type countingWatchStore struct{ calls int }

func (s *countingWatchStore) AddWatchedIssues(context.Context, string, []string) ([]string, error) {
	s.calls++
	return nil, nil
}
func (s *countingWatchStore) RemoveWatchedIssues(context.Context, string, []string) ([]string, error) {
	s.calls++
	return nil, nil
}

// TestHostTools_CheckTheCallersGrantWhenCalled.
//
// Every host tool — registered for the whole run with its whole family, as
// runview does — refuses a call whose context does not carry the capability it
// needs: no grant at all (a call that did not come through a node execution),
// a grant for a DIFFERENT capability; and serves the one that holds it. The
// stores are touched only by the served calls.
func TestHostTools_CheckTheCallersGrantWhenCalled(t *testing.T) {
	ns, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ns.Close() }()
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rs.CreateRun(context.Background(), "run-1", "demo", nil); err != nil {
		t.Fatal(err)
	}
	ws := &countingWatchStore{}
	reg := NewRegistry()
	if err := RegisterClawBoardTools(reg, &BoardConfig{Store: ns, Capabilities: boardops.AllCapabilities()}); err != nil {
		t.Fatal(err)
	}
	if err := RegisterClawRunTools(reg, &RunConfig{Store: rs, Capabilities: []string{runops.CapRunsRead}}); err != nil {
		t.Fatal(err)
	}
	if err := RegisterClawWatchTools(reg, &WatchConfig{Store: ws, RunID: "run-1", Capabilities: []string{"watch.subscribe", "watch.unsubscribe"}}); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct{ tool, needs, other, input string }{
		{"mcp.iterion_board.create_issue", "board.create", "board.read", `{"title":"t"}`},
		{"mcp.iterion_runs.run_get", runops.CapRunsRead, "board.read", `{"run_id":"run-1"}`},
		{"mcp.iterion_watch.subscribe", "watch.subscribe", "watch.unsubscribe", `{"issue_id":"i"}`},
	} {
		td, err := reg.Resolve(row.tool)
		if err != nil {
			t.Fatalf("resolve %s: %v", row.tool, err)
		}
		for _, grant := range []struct {
			name string
			ctx  context.Context
			ok   bool
		}{
			{"no grant", context.Background(), false},
			{"another capability", WithHostCapabilities(context.Background(), []string{row.other}, nil), false},
			{"its capability, withdrawn", WithHostCapabilities(context.Background(), nil, []string{row.needs}), false},
			{"its capability", WithHostCapabilities(context.Background(), []string{row.needs}, nil), true},
		} {
			_, err := td.Execute(grant.ctx, json.RawMessage(row.input))
			if refused := errors.Is(err, ErrHostCapabilityNotHeld); refused == grant.ok {
				t.Errorf("%s with %s: err = %v, want refused=%v", row.tool, grant.name, err, !grant.ok)
			}
			// The refusal says what decided it: the floor, for a capability
			// the node declares; the program, for one it does not.
			if withdrawn := errors.Is(err, ErrHostCapabilityWithdrawn); withdrawn != (grant.name == "its capability, withdrawn") {
				t.Errorf("%s with %s: err = %v, withdrawn=%v", row.tool, grant.name, err, withdrawn)
			}
		}
	}
	issues, err := ns.List(native.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 1 {
		t.Errorf("cards = %d, want exactly the one the granted call filed", len(issues))
	}
	if ws.calls != 1 {
		t.Errorf("watch store calls = %d, want exactly the granted one", ws.calls)
	}
}
