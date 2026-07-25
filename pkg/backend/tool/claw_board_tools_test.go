package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native/boardops"
)

// Guards the runview executor wiring: registering with
// boardops.AllCapabilities() must expose the FULL tool surface. The
// hand-maintained list it replaces had drifted (board.comment was
// missing, so claw nodes granted it got "unknown tool" on
// comment_issue).
func TestRegisterClawBoardTools_AllCapabilitiesExposesEveryTool(t *testing.T) {
	store, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry()
	cfg := &BoardConfig{Store: store, Capabilities: boardops.AllCapabilities()}
	if err := RegisterClawBoardTools(reg, cfg); err != nil {
		t.Fatalf("RegisterClawBoardTools: %v", err)
	}
	for _, name := range []string{
		"assign_issue", "close_issue", "comment_issue", "create_issue",
		"get_issue", "list_issues", "list_labels", "set_bot",
		"set_labels", "transition_issue",
	} {
		if _, err := reg.Resolve("mcp.iterion_board." + name); err != nil {
			t.Errorf("missing mcp.iterion_board.%s: %v", name, err)
		}
	}
}

func TestRegisterClawBoardTools_FiltersByCaps(t *testing.T) {
	store, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry()
	cfg := &BoardConfig{Store: store, Capabilities: []string{"board.read"}}
	if err := RegisterClawBoardTools(reg, cfg); err != nil {
		t.Fatalf("RegisterClawBoardTools: %v", err)
	}
	for _, name := range []string{
		"mcp.iterion_board.list_issues",
		"mcp.iterion_board.get_issue",
	} {
		if _, err := reg.Resolve(name); err != nil {
			t.Errorf("missing %s: %v", name, err)
		}
	}
	if _, err := reg.Resolve("mcp.iterion_board.create_issue"); err == nil {
		t.Errorf("create_issue should not be exposed with only board.read")
	}
}

func TestRegisterClawBoardTools_ExecuteHitsStore(t *testing.T) {
	store, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry()
	cfg := &BoardConfig{Store: store, Capabilities: []string{"board.create", "board.read"}}
	if err := RegisterClawBoardTools(reg, cfg); err != nil {
		t.Fatal(err)
	}
	td, err := reg.Resolve("mcp.iterion_board.create_issue")
	if err != nil {
		t.Fatalf("create_issue missing: %v", err)
	}
	out, err := td.Execute(context.Background(), json.RawMessage(`{"title":"Refactor X"}`))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out, "Refactor X") {
		t.Fatalf("expected title in output, got %s", out)
	}
	list, _ := store.List(native.ListFilter{})
	if len(list) != 1 || list[0].Title != "Refactor X" {
		t.Fatalf("store state unexpected: %+v", list)
	}
}

func TestRegisterClawBoardTools_NilCfgIsNoOp(t *testing.T) {
	reg := NewRegistry()
	if err := RegisterClawBoardTools(reg, nil); err != nil {
		t.Fatalf("nil cfg should be no-op, got %v", err)
	}
	if err := RegisterClawBoardTools(reg, &BoardConfig{}); err != nil {
		t.Fatalf("nil store should be no-op, got %v", err)
	}
}
