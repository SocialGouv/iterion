package boardops

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
)

func newStore(t *testing.T) *native.Store {
	t.Helper()
	dir := t.TempDir()
	s, err := native.NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s
}

func TestNewCapabilities(t *testing.T) {
	caps := NewCapabilities("board.create, board.read,,  board.move ")
	for _, want := range []string{"board.create", "board.read", "board.move"} {
		if !caps.Has(want) {
			t.Errorf("missing cap %q", want)
		}
	}
	if caps.Has("board.write") {
		t.Errorf("unexpected cap")
	}
}

func TestToolsFor_FiltersByCap(t *testing.T) {
	got := ToolsFor(NewCapabilities("board.create,board.read"))
	names := make([]string, 0, len(got))
	for _, t := range got {
		names = append(names, t.Name)
	}
	want := "create_issue,get_issue,list_issues,list_labels"
	if strings.Join(names, ",") != want {
		t.Fatalf("ToolsFor = %v, want %s", names, want)
	}
}

func TestToolsFor_EmptyCaps(t *testing.T) {
	if got := ToolsFor(Capabilities{}); len(got) != 0 {
		t.Fatalf("expected empty tool list, got %d", len(got))
	}
}

func TestCall_CapabilityDenied(t *testing.T) {
	s := newStore(t)
	_, err := Call(s, NewCapabilities("board.read"), "create_issue", json.RawMessage(`{"title":"hi"}`))
	if !errors.Is(err, ErrCapabilityDenied) {
		t.Fatalf("expected ErrCapabilityDenied, got %v", err)
	}
}

func TestCall_UnknownTool(t *testing.T) {
	s := newStore(t)
	_, err := Call(s, NewCapabilities("board.read"), "exterminate", nil)
	if err == nil || !strings.Contains(err.Error(), "unknown tool") {
		t.Fatalf("expected unknown tool error, got %v", err)
	}
}

func TestRoundTrip_CreateTransitionGetList(t *testing.T) {
	s := newStore(t)
	caps := NewCapabilities("board.create,board.move,board.read,board.label,board.assign,board.close")

	// Create.
	res, err := Call(s, caps, "create_issue", json.RawMessage(`{"title":"Refactor X","labels":["chore"]}`))
	if err != nil {
		t.Fatalf("create_issue: %v", err)
	}
	var created native.Issue
	if err := json.Unmarshal(res, &created); err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.Title != "Refactor X" {
		t.Fatalf("bad created issue: %+v", created)
	}

	// Transition.
	args, _ := json.Marshal(map[string]string{"id": created.ID, "to": "ready"})
	if _, err := Call(s, caps, "transition_issue", args); err != nil {
		t.Fatalf("transition_issue: %v", err)
	}
	got, err := s.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != "ready" {
		t.Fatalf("state after transition = %q, want ready", got.State)
	}

	// Assign.
	args, _ = json.Marshal(map[string]string{"id": created.ID, "assignee": "feature_dev"})
	if _, err := Call(s, caps, "assign_issue", args); err != nil {
		t.Fatalf("assign_issue: %v", err)
	}

	// Set labels.
	args, _ = json.Marshal(map[string]any{"id": created.ID, "labels": []string{"a", "b"}})
	if _, err := Call(s, caps, "set_labels", args); err != nil {
		t.Fatalf("set_labels: %v", err)
	}

	// List filtered.
	res, err = Call(s, caps, "list_issues", json.RawMessage(`{"state":"ready"}`))
	if err != nil {
		t.Fatalf("list_issues: %v", err)
	}
	var list []native.Issue
	if err := json.Unmarshal(res, &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != created.ID || list[0].Assignee != "feature_dev" {
		t.Fatalf("list result unexpected: %+v", list)
	}

	// Get by short prefix.
	prefix := created.ID[len("native:") : len("native:")+8]
	args, _ = json.Marshal(map[string]string{"id": prefix})
	res, err = Call(s, caps, "get_issue", args)
	if err != nil {
		t.Fatalf("get_issue: %v", err)
	}
	var fetched native.Issue
	if err := json.Unmarshal(res, &fetched); err != nil {
		t.Fatal(err)
	}
	if fetched.ID != created.ID {
		t.Fatalf("get_issue returned %s, want %s", fetched.ID, created.ID)
	}

	// Close (defaults to first terminal state).
	args, _ = json.Marshal(map[string]string{"id": created.ID})
	if _, err := Call(s, caps, "close_issue", args); err != nil {
		t.Fatalf("close_issue: %v", err)
	}
	got, _ = s.Get(created.ID)
	if !s.Board().StateByName(got.State).Terminal {
		t.Fatalf("close_issue did not land on a terminal state: %s", got.State)
	}
}

func TestClose_RejectsNonTerminalTarget(t *testing.T) {
	s := newStore(t)
	caps := NewCapabilities("board.create,board.close")
	res, err := Call(s, caps, "create_issue", json.RawMessage(`{"title":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	var iss native.Issue
	_ = json.Unmarshal(res, &iss)

	args, _ := json.Marshal(map[string]string{"id": iss.ID, "to": "ready"})
	if _, err := Call(s, caps, "close_issue", args); err == nil || !strings.Contains(err.Error(), "not terminal") {
		t.Fatalf("expected not-terminal rejection, got %v", err)
	}
}
