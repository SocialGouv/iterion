package delegate

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate/claudesdk"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native/boardops"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// The stdio board server has no grant of its own: it lists and answers what
// ITERION_BOARD_CAPS tells it (cmd/iterion/mcp_board.go), and that value is
// the TASK's capabilities — the node's effective grant — joined by
// BoardServerEnv, the one derivation both CLI backends spawn it with. A wiring
// that sent any other list would open the whole board surface to a node
// holding board.read, so the value is read off the spawn configuration
// claude_code actually serialises, never re-derived here.
func TestTheStdioBoardServerCarriesTheTasksOwnGrant(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "iterion")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ITERION_BIN", bin)

	task := Task{
		NodeID:        "n",
		AllowedTools:  []string{"Read"},
		Capabilities:  []string{boardops.CapBoardRead},
		StoreDir:      "/store",
		SourceIssueID: "42",
	}
	var logs bytes.Buffer
	b := &ClaudeCodeBackend{Logger: iterlog.New(iterlog.LevelWarn, &logs)}
	var extras []string
	opts := b.wireBoardMCP(task, nil, &extras)
	var args []string
	opts = append(opts, claudesdk.WithCLIPath("fake-claude"), claudesdk.WithCommandBuilder(func(ctx context.Context, _ string, argv []string, _ string, _ map[string]string, _ bool) *exec.Cmd {
		args = append([]string(nil), argv...)
		return exec.CommandContext(ctx, "true")
	}))
	// The fake exits without a model result; only the serialized spawn
	// configuration matters.
	session := claudesdk.NewSession(opts...)
	_ = session.Send(context.Background(), "capture config")
	_ = session.Close()
	i := slices.Index(args, "--mcp-config")
	if i < 0 || i+1 >= len(args) {
		t.Fatalf("no --mcp-config in the spawn argv %q (logs: %s)", args, &logs)
	}
	var cfg struct {
		Servers map[string]struct {
			Type    string            `json:"type"`
			Command string            `json:"command"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(args[i+1]), &cfg); err != nil {
		t.Fatal(err)
	}
	board, ok := cfg.Servers[boardMCPServerName]
	if !ok {
		t.Fatalf("no %q server in the spawn configuration: %+v (logs: %s)", boardMCPServerName, cfg.Servers, &logs)
	}
	if board.Type != "stdio" || board.Command != bin {
		t.Errorf("board server = %+v, want %s over stdio", board, bin)
	}
	if want := BoardServerEnv(task); !reflect.DeepEqual(board.Env, want) {
		t.Errorf("board server env = %v, want BoardServerEnv(task) = %v", board.Env, want)
	}
	if got := board.Env["ITERION_BOARD_CAPS"]; got != boardops.CapBoardRead {
		t.Fatalf("ITERION_BOARD_CAPS = %q, want the task's own grant %q", got, boardops.CapBoardRead)
	}
	// What the env opens is what the extras advertise, and neither carries a
	// write.
	if want := BoardToolsFor(task.Capabilities); !slices.Equal(extras, want) {
		t.Errorf("extras = %v, want BoardToolsFor(task.Capabilities) = %v", extras, want)
	}
	for _, listed := range boardops.ToolsFor(boardops.NewCapabilities(board.Env["ITERION_BOARD_CAPS"])) {
		if listed.Name == "create_issue" {
			t.Fatalf("the grant the server receives lists create_issue: %q", board.Env["ITERION_BOARD_CAPS"])
		}
	}
}
