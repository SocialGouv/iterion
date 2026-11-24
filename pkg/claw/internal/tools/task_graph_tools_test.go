package tools

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/internal/runtime/task"
)

func TestTaskCreateWithGraphFields(t *testing.T) {
	reg := task.NewRegistry()

	out, err := ExecuteTaskCreate(map[string]any{"subject": "design"}, reg)
	if err != nil {
		t.Fatal(err)
	}
	var a task.Task
	if err := json.Unmarshal([]byte(out), &a); err != nil {
		t.Fatal(err)
	}

	out, err = ExecuteTaskCreate(map[string]any{
		"subject":     "implement",
		"active_form": "Implementing",
		"owner":       "coder",
		"blocked_by":  []any{a.TaskID},
	}, reg)
	if err != nil {
		t.Fatal(err)
	}
	var b task.Task
	if err := json.Unmarshal([]byte(out), &b); err != nil {
		t.Fatal(err)
	}
	if !b.Blocked || b.ActiveForm != "Implementing" || b.Owner != "coder" {
		t.Errorf("graph fields wrong: %+v", b)
	}

	// Neither prompt nor subject → explicit error.
	if _, err := ExecuteTaskCreate(map[string]any{}, reg); err == nil || !strings.Contains(err.Error(), "'prompt' or 'subject'") {
		t.Errorf("want prompt-or-subject error, got %v", err)
	}
	// Unknown dependency → explicit error.
	if _, err := ExecuteTaskCreate(map[string]any{"subject": "x", "blocks": []any{"ghost"}}, reg); err == nil {
		t.Error("unknown dependency must error")
	}
}

func TestTaskUpdateGraphOperations(t *testing.T) {
	reg := task.NewRegistry()
	a, _ := reg.CreateWithSpec(task.TaskSpec{Subject: "a"})
	b, _ := reg.CreateWithSpec(task.TaskSpec{Subject: "b"})

	// Status alias + edge append in one update.
	out, err := ExecuteTaskUpdate(map[string]any{
		"task_id":        b.TaskID,
		"status":         "in_progress",
		"add_blocked_by": []any{a.TaskID},
	}, reg)
	if err != nil {
		t.Fatal(err)
	}
	var got task.Task
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if got.WorkStatus != "in_progress" || !got.Blocked {
		t.Errorf("update result wrong: %+v", got)
	}

	// completed alias.
	if _, err := ExecuteTaskUpdate(map[string]any{"task_id": a.TaskID, "status": "completed"}, reg); err != nil {
		t.Fatal(err)
	}
	a2, _ := reg.Get(a.TaskID)
	if a2.Status != task.StatusCompleted {
		t.Errorf("completed alias not applied: %v", a2.Status)
	}

	// deleted removes and reports.
	out, err = ExecuteTaskUpdate(map[string]any{"task_id": a.TaskID, "status": "deleted"}, reg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "deleted") {
		t.Errorf("deletion must be reported: %s", out)
	}
	if _, ok := reg.Get(a.TaskID); ok {
		t.Error("a must be removed")
	}

	// No change at all → explicit error.
	if _, err := ExecuteTaskUpdate(map[string]any{"task_id": b.TaskID}, reg); err == nil {
		t.Error("empty update must error")
	}
	// Message-only keeps working (legacy behavior).
	if _, err := ExecuteTaskUpdate(map[string]any{"task_id": b.TaskID, "message": "note"}, reg); err != nil {
		t.Errorf("message-only update must keep working: %v", err)
	}
}

func TestTaskListStatusAliases(t *testing.T) {
	reg := task.NewRegistry()
	_, _ = reg.CreateWithSpec(task.TaskSpec{Subject: "a"})

	out, err := ExecuteTaskList(map[string]any{"status": "pending"}, reg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"count": 1`) {
		t.Errorf("pending alias filter must match created tasks: %s", out)
	}
	if _, err := ExecuteTaskList(map[string]any{"status": "bogus"}, reg); err == nil {
		t.Error("bogus filter must error")
	}
}

func TestTodoWriteCompletedAliasAndGraphFields(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("CLAW_TODOS_DIR", t.TempDir())

	_, err := ExecuteTodoWrite(map[string]any{
		"action": "write",
		"todos": []any{
			map[string]any{"id": "1", "content": "step one", "status": "completed", "priority": "high",
				"active_form": "Stepping", "blocks": []any{"2"}},
			map[string]any{"id": "2", "content": "step two", "status": "pending", "priority": "low",
				"blocked_by": []any{"1"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := ExecuteTodoWrite(map[string]any{"action": "read"})
	if err != nil {
		t.Fatal(err)
	}
	var items []TodoItem
	if err := json.Unmarshal([]byte(out), &items); err != nil {
		t.Fatal(err)
	}
	if items[0].Status != "done" {
		t.Errorf("completed alias must normalize to done: %+v", items[0])
	}
	if items[0].ActiveForm != "Stepping" || len(items[0].Blocks) != 1 || len(items[1].BlockedBy) != 1 {
		t.Errorf("graph fields must persist: %+v", items)
	}
}
