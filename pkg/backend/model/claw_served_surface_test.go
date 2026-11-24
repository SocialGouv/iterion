package model

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// TestPromptNarrowingAndAClawGrant pins what a prompt command's
// `allowed-tools:` does to a claw node holding a board / runs-read grant,
// through the real buildTask: the narrowing never empties the served
// surface (the always-allowed trio keeps todo_write), and a grant family it
// leaves with no tool is named in one Warn per node execution — the grant
// is held, inert, and nothing else would say so.
//
// Mutations that redden it: delete the Warn (arms "a declared-empty list"
// and "a list keeping the board"); drop .withAlwaysAllowed() in
// tool.CommandNarrowedSet (the todo_write-only surface assertion); move the
// Warn up into the narrowing block (the failed-build arm reddens).
func TestPromptNarrowingAndAClawGrant(t *testing.T) {
	t.Parallel()

	build := func(t *testing.T, caps []string, sess *nodeBuildSession) (delegate.Task, string, func() string) {
		t.Helper()
		e := internalMCPExecutor(t)
		var buf bytes.Buffer
		e.logger = iterlog.New(iterlog.LevelInfo, &buf)
		node := &ir.AgentNode{
			BaseNode:     ir.BaseNode{ID: "triage"},
			Tools:        []string{"skill"},
			Capabilities: caps,
		}
		f, err := extractBackendFields(node)
		if err != nil {
			t.Fatalf("extractBackendFields: %v", err)
		}
		run := func() (delegate.Task, string) {
			before := buf.Len()
			task, err := e.buildTask(context.Background(), node, f, map[string]any{}, delegate.BackendClaw, sess)
			if err != nil {
				t.Fatalf("buildTask: %v", err)
			}
			return task, buf.String()[before:]
		}
		task, log := run()
		again := func() string { _, l := run(); return l }
		return task, log, again
	}
	warnings := func(log string) []string {
		var out []string
		for _, line := range strings.Split(log, "\n") {
			if strings.Contains(line, "narrows away every tool") {
				out = append(out, line)
			}
		}
		return out
	}
	bothGrants := []string{"board.read", "runs.read"}

	t.Run("a declared-empty list keeps todo_write and names both grants once", func(t *testing.T) {
		t.Parallel()
		sess := &nodeBuildSession{}
		sess.noteCommandNarrowing("audit", []string{}, true)
		task, log, again := build(t, bothGrants, sess)
		if got := toolDefNames(task.ToolDefs); !slices.Equal(got, []string{"todo_write"}) {
			t.Fatalf("narrowed ToolDefs = %v, want exactly [todo_write]: the always-allowed trio keeps the served surface non-empty", got)
		}
		if len(task.AllowedTools) <= len(task.ToolDefs) {
			t.Fatalf("AllowedTools = %v: the scenario needs the CLI allowlist wider than the served surface", task.AllowedTools)
		}
		w := warnings(log)
		if len(w) != 1 || !strings.Contains(w[0], "/audit") || !strings.Contains(w[0], "board and runs read grant") {
			t.Fatalf("warnings = %q, want one naming /audit and the board and runs read grant", w)
		}
		if w := warnings(again()); len(w) != 0 {
			t.Fatalf("a second build of the same node execution warned again: %q", w)
		}
	})

	t.Run("a list keeping the board names runs read only", func(t *testing.T) {
		t.Parallel()
		sess := &nodeBuildSession{}
		sess.noteCommandNarrowing("audit", []string{"mcp__iterion_board__*"}, true)
		task, log, _ := build(t, bothGrants, sess)
		if !slices.Contains(toolDefNames(task.ToolDefs), "mcp_iterion_board_list_issues") {
			t.Fatalf("narrowed ToolDefs = %v, want the board tools the command names kept", toolDefNames(task.ToolDefs))
		}
		w := warnings(log)
		if len(w) != 1 || !strings.Contains(w[0], "node's runs read grant") {
			t.Fatalf("warnings = %q, want one naming the runs read grant alone", w)
		}
	})

	t.Run("a list naming both families is silent", func(t *testing.T) {
		t.Parallel()
		sess := &nodeBuildSession{}
		sess.noteCommandNarrowing("audit", []string{"mcp__iterion_board__list_issues", "mcp__iterion_runs__*"}, true)
		_, log, _ := build(t, bothGrants, sess)
		if w := warnings(log); len(w) != 0 {
			t.Fatalf("warnings = %q, want none: each grant keeps a tool", w)
		}
	})

	t.Run("a build that fails after the narrowing does not spend the Warn", func(t *testing.T) {
		t.Parallel()
		e := internalMCPExecutor(t)
		var buf bytes.Buffer
		e.logger = iterlog.New(iterlog.LevelInfo, &buf)
		node := &ir.AgentNode{
			BaseNode:     ir.BaseNode{ID: "triage"},
			Tools:        []string{"skill"},
			Capabilities: []string{"board.read"},
		}
		node.Session = ir.SessionPersist
		f, err := extractBackendFields(node)
		if err != nil {
			t.Fatalf("extractBackendFields: %v", err)
		}
		sess := &nodeBuildSession{}
		sess.noteCommandNarrowing("audit", []string{}, true)
		// A persisted resume answer with no conversation fails the build
		// after the narrowing ran.
		_, err = e.buildTask(context.Background(), node, f, map[string]any{delegate.ResumeAnswerKey: "x"}, delegate.BackendClaw, sess)
		if err == nil || !strings.Contains(err.Error(), "persisted resume") {
			t.Fatalf("buildTask with a resume answer and no persisted conversation: err = %v, want the persisted-resume failure", err)
		}
		if w := warnings(buf.String()); len(w) != 0 {
			t.Fatalf("a failed build warned: %q", w)
		}
		before := buf.Len()
		if _, err := e.buildTask(context.Background(), node, f, map[string]any{}, delegate.BackendClaw, sess); err != nil {
			t.Fatalf("buildTask: %v", err)
		}
		if w := warnings(buf.String()[before:]); len(w) != 1 {
			t.Fatalf("warnings = %q, want exactly one on the first successful build", w)
		}
	})

	t.Run("a node without a grant is silent", func(t *testing.T) {
		t.Parallel()
		sess := &nodeBuildSession{}
		sess.noteCommandNarrowing("audit", []string{}, true)
		_, log, _ := build(t, nil, sess)
		if w := warnings(log); len(w) != 0 {
			t.Fatalf("warnings = %q, want none: the node holds no grant", w)
		}
	})
}
