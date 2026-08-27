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

func TestAllCapabilities_CoversEveryDeclaredCap(t *testing.T) {
	got := AllCapabilities()
	want := []string{
		CapBoardRead, CapBoardCreate, CapBoardMove, CapBoardAssign,
		CapBoardLabel, CapBoardClose, CapBoardComment,
	}
	gotSet := map[string]bool{}
	for _, c := range got {
		if gotSet[c] {
			t.Errorf("duplicate capability %q", c)
		}
		gotSet[c] = true
	}
	for _, c := range want {
		if !gotSet[c] {
			t.Errorf("AllCapabilities missing %q — a board tool's capability is not covered", c)
		}
	}
	if len(got) != len(want) {
		t.Errorf("AllCapabilities returned %d caps, want %d: %v", len(got), len(want), got)
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

func TestCommentIssue_CapGatedAndPersists(t *testing.T) {
	s := newStore(t)

	// Create an issue to comment on.
	res, err := Call(s, NewCapabilities("board.create"), "create_issue", json.RawMessage(`{"title":"Improve contrast"}`))
	if err != nil {
		t.Fatalf("create_issue: %v", err)
	}
	var created native.Issue
	if err := json.Unmarshal(res, &created); err != nil {
		t.Fatal(err)
	}

	// Without board.comment the tool is denied.
	args, _ := json.Marshal(map[string]string{"id": created.ID, "body": "Opened MR: http://x/1"})
	if _, err := Call(s, NewCapabilities("board.read"), "comment_issue", args); !errors.Is(err, ErrCapabilityDenied) {
		t.Fatalf("want ErrCapabilityDenied, got %v", err)
	}

	// With board.comment it persists.
	if _, err := Call(s, NewCapabilities("board.comment"), "comment_issue", args); err != nil {
		t.Fatalf("comment_issue: %v", err)
	}
	got, err := s.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.Comments) != 1 || got.Comments[0].Body != "Opened MR: http://x/1" {
		t.Fatalf("comment not persisted: %+v", got.Comments)
	}

	// Empty body rejected.
	bad, _ := json.Marshal(map[string]string{"id": created.ID, "body": "  "})
	if _, err := Call(s, NewCapabilities("board.comment"), "comment_issue", bad); err == nil {
		t.Fatal("empty body should be rejected")
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

func TestSetBot_SetsBotFieldNotAssignee(t *testing.T) {
	s := newStore(t)
	caps := NewCapabilities("board.create,board.assign")
	res, err := Call(s, caps, "create_issue", json.RawMessage(`{"title":"x","assignee":"alice"}`))
	if err != nil {
		t.Fatal(err)
	}
	var iss native.Issue
	_ = json.Unmarshal(res, &iss)

	args, _ := json.Marshal(map[string]string{"id": iss.ID, "bot": "feature_dev"})
	if _, err := Call(s, caps, "set_bot", args); err != nil {
		t.Fatalf("set_bot: %v", err)
	}
	got, _ := s.Get(iss.ID)
	if got.Bot != "feature_dev" {
		t.Errorf("Bot = %q, want feature_dev", got.Bot)
	}
	if got.Assignee != "alice" {
		t.Errorf("Assignee = %q, want unchanged 'alice' — set_bot must not touch the owner", got.Assignee)
	}
}

func TestCreateIssue_SetsBotAndBotArgs(t *testing.T) {
	s := newStore(t)
	caps := NewCapabilities("board.create")
	// The MCP create_issue must reach parity with REST POST /issues, which
	// already accepts bot/bot_args: a board-fed pipeline needs its bot pinned
	// at create time, without a follow-up set_bot round trip.
	res, err := Call(s, caps, "create_issue", json.RawMessage(
		`{"title":"Ship feature","bot":"feature-dev","bot_args":{"feature_prompt":"add X"}}`))
	if err != nil {
		t.Fatalf("create_issue: %v", err)
	}
	var iss native.Issue
	if err := json.Unmarshal(res, &iss); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(iss.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Bot != "feature-dev" {
		t.Errorf("Bot = %q, want feature-dev", got.Bot)
	}
	if got.BotArgs["feature_prompt"] != "add X" {
		t.Errorf("BotArgs = %+v, want feature_prompt=add X", got.BotArgs)
	}
}

func TestSetBot_RequiresAssignCapability(t *testing.T) {
	s := newStore(t)
	// Only board.read granted → set_bot (needs board.assign) must be denied.
	_, err := Call(s, NewCapabilities("board.read"), "set_bot", json.RawMessage(`{"id":"x","bot":"feature_dev"}`))
	if err == nil || !errors.Is(err, ErrCapabilityDenied) {
		t.Fatalf("expected ErrCapabilityDenied, got %v", err)
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

func TestCreateIssue_ParentIDAndAutoSpawn(t *testing.T) {
	dir := t.TempDir()
	s, err := native.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	caps := NewCapabilities("board.create,board.read")

	// Explicit parent_id
	raw, err := Call(s, caps, "create_issue", json.RawMessage(
		`{"title":"child","parent_id":"native:planner-1","bot":"producer"}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	var child native.Issue
	if err := json.Unmarshal(raw, &child); err != nil {
		t.Fatal(err)
	}
	if child.ParentID != "native:planner-1" {
		t.Fatalf("ParentID = %q", child.ParentID)
	}
	if child.BotArgs[native.BotArgSpawnedFrom] != "native:planner-1" {
		t.Fatalf("spawned_from = %v", child.BotArgs)
	}

	// Auto from CallEnv
	raw2, err := CallWithEnv(s, caps, "create_issue", json.RawMessage(`{"title":"auto-child"}`), CallEnv{SpawnParentID: "native:auto-parent"})
	if err != nil {
		t.Fatal(err)
	}
	var child2 native.Issue
	_ = json.Unmarshal(raw2, &child2)
	if child2.ParentID != "native:auto-parent" {
		t.Fatalf("auto ParentID = %q", child2.ParentID)
	}
}

// Closing a ticket the DISPATCHER filed (retry budget exhausted) is the
// operator's acknowledgement of that give-up, so it must drop the stamp —
// including when the close target is the state the give-up already wrote,
// where the state change is a no-op and nothing else can expire the stamp.
// Otherwise the card stays in the pipeline board's needs-attention lane after
// being closed.
func TestClose_AcknowledgesADispatcherGiveUp(t *testing.T) {
	s := newStore(t)
	caps := NewCapabilities("board.create,board.close,board.read")
	res, err := Call(s, caps, "create_issue", json.RawMessage(`{"title":"doomed"}`))
	if err != nil {
		t.Fatal(err)
	}
	var iss native.Issue
	_ = json.Unmarshal(res, &iss)

	if _, err := s.SetState(iss.ID, native.StateBlocked); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	if err := s.SetGaveUp(iss.ID, &native.GiveUp{RunID: "run-x", State: native.StateBlocked, Attempts: 3}); err != nil {
		t.Fatalf("SetGaveUp: %v", err)
	}

	args, _ := json.Marshal(map[string]string{"id": iss.ID, "to": native.StateBlocked})
	closed, err := Call(s, caps, "close_issue", args)
	if err != nil {
		t.Fatalf("close_issue: %v", err)
	}
	got, err := s.Get(iss.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.GaveUp != nil {
		t.Errorf("give-up stamp survived close_issue: %+v", got.GaveUp)
	}
	// The tool's own result must agree with what was persisted.
	var reported native.Issue
	if err := json.Unmarshal(closed, &reported); err != nil {
		t.Fatalf("decode close_issue result: %v", err)
	}
	if reported.GaveUp != nil {
		t.Errorf("close_issue reported a stamp it just cleared: %+v", reported.GaveUp)
	}
}
