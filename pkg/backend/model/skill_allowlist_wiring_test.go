package model

import (
	"os"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// The executor half of the pinned skill mode (iterion#1719): armed by the
// engine, the set reaches every task the run builds, and the sandbox
// crossing re-roots it workspace-relative — the host's absolute paths
// would not exist in the container.
func TestArmedSkillAllowlistReachesTheTaskAndTheWire(t *testing.T) {
	t.Parallel()
	work := t.TempDir()
	e := nodeRulesExecutor(t, &ir.Workflow{}, WithWorkDir(work))
	pinned := map[string]string{"review": work + "/.claude/iterion-run-skills/.claude/skills/review/SKILL.md"}
	e.SetSkillAllowlist(pinned)
	e.SetSkillChannelRoot(work + "/.claude/iterion-run-skills")

	task := buildNodeTask(t, e, nodeRulesAgent(nil))
	if got := task.SkillAllowlist["review"]; got != pinned["review"] {
		t.Fatalf("task.SkillAllowlist = %v, want the pinned set", task.SkillAllowlist)
	}
	if task.SkillChannelRoot == "" {
		t.Fatal("task.SkillChannelRoot is empty — the engine armed it")
	}

	backend := NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{})
	ioTask := backend.runnerIOTask(task, nil)
	if !ioTask.SkillAllowlistArmed {
		t.Fatal("the crossing dropped the armed bit")
	}
	want := ".claude/iterion-run-skills/.claude/skills/review/SKILL.md"
	if got := ioTask.SkillAllowlist["review"]; got != want {
		t.Errorf("the wire path = %q, want workspace-relative %q", got, want)
	}

	// Disarmed, the task carries no pin and the wire stays silent — a
	// trusted run's `skill` tool walks the workspace, unchanged.
	e2 := nodeRulesExecutor(t, &ir.Workflow{}, WithWorkDir(work))
	task2 := buildNodeTask(t, e2, nodeRulesAgent(nil))
	if task2.SkillAllowlist != nil || task2.SkillChannelRoot != "" {
		t.Errorf("an unarmed executor stamped %v / %q, want neither", task2.SkillAllowlist, task2.SkillChannelRoot)
	}
	ioTask2 := backend.runnerIOTask(task2, nil)
	if ioTask2.SkillAllowlistArmed || ioTask2.SkillAllowlist != nil {
		t.Errorf("the wire = (armed %v, %v), want disarmed", ioTask2.SkillAllowlistArmed, ioTask2.SkillAllowlist)
	}
}

// An allowlist entry outside the workspace cannot relativize: it crosses
// absolute — the runner's in-container read of it fails loudly — rather
// than being dropped into a silently smaller pin.
func TestRunnerIOTaskKeepsAnUnrelativizableSkillPinAbsolute(t *testing.T) {
	t.Parallel()
	work := t.TempDir()
	e := nodeRulesExecutor(t, &ir.Workflow{}, WithWorkDir(work))
	e.SetSkillAllowlist(map[string]string{"odd": "/elsewhere/skill/SKILL.md"})
	task := buildNodeTask(t, e, nodeRulesAgent(nil))

	backend := NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{})
	ioTask := backend.runnerIOTask(task, nil)
	// filepath.Rel never invents a container path: the entry crosses as sent.
	if got := ioTask.SkillAllowlist["odd"]; got != "/elsewhere/skill/SKILL.md" {
		t.Errorf("the unrelativizable pin = %q, want it absolute", got)
	}
	if !ioTask.SkillAllowlistArmed {
		t.Error("the armed bit did not cross")
	}

	// A sibling whose name merely BEGINS with ".." is not an escape: it
	// relativizes and crosses relative like any in-workspace entry.
	if err := os.MkdirAll(work+"/..foo", 0o755); err != nil {
		t.Fatal(err)
	}
	e2 := nodeRulesExecutor(t, &ir.Workflow{}, WithWorkDir(work))
	e2.SetSkillAllowlist(map[string]string{"edge": work + "/..foo/skill.md"})
	ioTask2 := backend.runnerIOTask(buildNodeTask(t, e2, nodeRulesAgent(nil)), nil)
	if got := ioTask2.SkillAllowlist["edge"]; got != "..foo/skill.md" {
		t.Errorf("a ..foo sibling = %q, want it relativized — it is inside the workspace", got)
	}
}
