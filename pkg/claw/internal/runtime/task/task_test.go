package task

import (
	"errors"
	"strings"
	"sync"
	"testing"
)

func TestCreatesAndRetrievesTasks(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	desc := "A test task"
	task := reg.Create("Do something", &desc)

	if task.Status != StatusCreated {
		t.Errorf("Status = %v, want %v", task.Status, StatusCreated)
	}
	if task.Prompt != "Do something" {
		t.Errorf("Prompt = %q, want %q", task.Prompt, "Do something")
	}
	if task.Description == nil || *task.Description != "A test task" {
		t.Errorf("Description = %v, want %q", task.Description, "A test task")
	}
	if task.TaskPacket != nil {
		t.Errorf("TaskPacket = %v, want nil", task.TaskPacket)
	}

	fetched, ok := reg.Get(task.TaskID)
	if !ok {
		t.Fatal("task should exist")
	}
	if fetched.TaskID != task.TaskID {
		t.Errorf("TaskID = %q, want %q", fetched.TaskID, task.TaskID)
	}
}

func TestCreatesTaskFromPacket(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	packet := TaskPacket{
		Objective:         "Ship task packet support",
		Scope:             "runtime/task system",
		Repo:              "claw-code-parity",
		BranchPolicy:      "origin/main only",
		AcceptanceTests:   []string{"cargo test --workspace"},
		CommitPolicy:      "single commit",
		ReportingContract: "print commit sha",
		EscalationPolicy:  "manual escalation",
	}

	task, err := reg.CreateFromPacket(packet)
	if err != nil {
		t.Fatalf("CreateFromPacket() error: %v", err)
	}

	if task.Prompt != "Ship task packet support" {
		t.Errorf("Prompt = %q, want %q", task.Prompt, "Ship task packet support")
	}
	if task.Description == nil || *task.Description != "runtime/task system" {
		t.Errorf("Description = %v, want %q", task.Description, "runtime/task system")
	}
	if task.TaskPacket == nil {
		t.Fatal("TaskPacket should not be nil")
	}
	if task.TaskPacket.Objective != packet.Objective {
		t.Errorf("TaskPacket.Objective = %q, want %q", task.TaskPacket.Objective, packet.Objective)
	}
}

func TestListsTasksWithOptionalFilter(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	reg.Create("Task A", nil)
	taskB := reg.Create("Task B", nil)
	reg.SetStatus(taskB.TaskID, StatusRunning)

	all := reg.List(nil)
	if len(all) != 2 {
		t.Errorf("len(all) = %d, want 2", len(all))
	}

	running := StatusRunning
	runningList := reg.List(&running)
	if len(runningList) != 1 {
		t.Errorf("len(running) = %d, want 1", len(runningList))
	}
	if runningList[0].TaskID != taskB.TaskID {
		t.Errorf("running[0].TaskID = %q, want %q", runningList[0].TaskID, taskB.TaskID)
	}

	created := StatusCreated
	createdList := reg.List(&created)
	if len(createdList) != 1 {
		t.Errorf("len(created) = %d, want 1", len(createdList))
	}
}

func TestStopsRunningTask(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	task := reg.Create("Stoppable", nil)
	reg.SetStatus(task.TaskID, StatusRunning)

	stopped, err := reg.Stop(task.TaskID)
	if err != nil {
		t.Fatalf("Stop() error: %v", err)
	}
	if stopped.Status != StatusStopped {
		t.Errorf("Status = %v, want %v", stopped.Status, StatusStopped)
	}

	// Stopping again should fail
	_, err = reg.Stop(task.TaskID)
	if err == nil {
		t.Fatal("expected error on second stop")
	}
	if !errors.Is(err, ErrTerminalState) {
		t.Errorf("expected ErrTerminalState, got %v", err)
	}
}

func TestUpdatesTaskWithMessages(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	task := reg.Create("Messageable", nil)
	updated, err := reg.Update(task.TaskID, "Here's more context")
	if err != nil {
		t.Fatalf("Update() error: %v", err)
	}
	if len(updated.Messages) != 1 {
		t.Fatalf("len(Messages) = %d, want 1", len(updated.Messages))
	}
	if updated.Messages[0].Content != "Here's more context" {
		t.Errorf("Messages[0].Content = %q, want %q", updated.Messages[0].Content, "Here's more context")
	}
	if updated.Messages[0].Role != "user" {
		t.Errorf("Messages[0].Role = %q, want %q", updated.Messages[0].Role, "user")
	}
}

func TestAppendsAndRetrievesOutput(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	task := reg.Create("Output task", nil)
	reg.AppendOutput(task.TaskID, "line 1\n")
	reg.AppendOutput(task.TaskID, "line 2\n")

	output, err := reg.Output(task.TaskID)
	if err != nil {
		t.Fatalf("Output() error: %v", err)
	}
	if output != "line 1\nline 2\n" {
		t.Errorf("Output = %q, want %q", output, "line 1\nline 2\n")
	}
}

func TestAssignsTeamAndRemovesTask(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	task := reg.Create("Team task", nil)
	reg.AssignTeam(task.TaskID, "team_abc")

	fetched, ok := reg.Get(task.TaskID)
	if !ok {
		t.Fatal("task should exist")
	}
	if fetched.TeamID == nil || *fetched.TeamID != "team_abc" {
		t.Errorf("TeamID = %v, want team_abc", fetched.TeamID)
	}

	removed := reg.Remove(task.TaskID)
	if removed == nil {
		t.Fatal("expected removed task")
	}
	_, ok = reg.Get(task.TaskID)
	if ok {
		t.Fatal("task should be removed")
	}
	if !reg.IsEmpty() {
		t.Error("registry should be empty")
	}
}

func TestRejectsOperationsOnMissingTask(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()

	if _, err := reg.Stop("nonexistent"); err == nil {
		t.Error("Stop should fail for missing task")
	}
	if _, err := reg.Update("nonexistent", "msg"); err == nil {
		t.Error("Update should fail for missing task")
	}
	if _, err := reg.Output("nonexistent"); err == nil {
		t.Error("Output should fail for missing task")
	}
	if err := reg.AppendOutput("nonexistent", "data"); err == nil {
		t.Error("AppendOutput should fail for missing task")
	}
	if err := reg.SetStatus("nonexistent", StatusRunning); err == nil {
		t.Error("SetStatus should fail for missing task")
	}
}

func TestTaskStatusDisplayAllVariants(t *testing.T) {
	t.Parallel()
	cases := []struct {
		status TaskStatus
		want   string
	}{
		{StatusCreated, "created"},
		{StatusRunning, "running"},
		{StatusCompleted, "completed"},
		{StatusFailed, "failed"},
		{StatusStopped, "stopped"},
	}
	for _, tc := range cases {
		if tc.status.String() != tc.want {
			t.Errorf("%v.String() = %q, want %q", tc.status, tc.status.String(), tc.want)
		}
	}
}

func TestStopRejectsCompletedTask(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	task := reg.Create("done", nil)
	reg.SetStatus(task.TaskID, StatusCompleted)

	_, err := reg.Stop(task.TaskID)
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrTerminalState) {
		t.Errorf("expected ErrTerminalState, got %v", err)
	}
}

func TestStopRejectsFailedTask(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	task := reg.Create("failed", nil)
	reg.SetStatus(task.TaskID, StatusFailed)

	_, err := reg.Stop(task.TaskID)
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrTerminalState) {
		t.Errorf("expected ErrTerminalState, got %v", err)
	}
}

func TestStopSucceedsFromCreatedState(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	task := reg.Create("created task", nil)

	stopped, err := reg.Stop(task.TaskID)
	if err != nil {
		t.Fatalf("Stop() error: %v", err)
	}
	if stopped.Status != StatusStopped {
		t.Errorf("Status = %v, want %v", stopped.Status, StatusStopped)
	}
	if stopped.UpdatedAt < task.UpdatedAt {
		t.Errorf("UpdatedAt should be >= task.UpdatedAt")
	}
}

func TestNewRegistryIsEmpty(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()

	all := reg.List(nil)
	if !reg.IsEmpty() {
		t.Error("registry should be empty")
	}
	if reg.Len() != 0 {
		t.Errorf("Len = %d, want 0", reg.Len())
	}
	if len(all) != 0 {
		t.Errorf("len(all) = %d, want 0", len(all))
	}
}

func TestCreateWithoutDescription(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	task := reg.Create("Do the thing", nil)

	if !strings.HasPrefix(task.TaskID, "task_") {
		t.Errorf("TaskID = %q, want prefix task_", task.TaskID)
	}
	if task.Description != nil {
		t.Errorf("Description = %v, want nil", task.Description)
	}
	if task.TaskPacket != nil {
		t.Errorf("TaskPacket = %v, want nil", task.TaskPacket)
	}
	if len(task.Messages) != 0 {
		t.Errorf("len(Messages) = %d, want 0", len(task.Messages))
	}
	if task.Output != "" {
		t.Errorf("Output = %q, want empty", task.Output)
	}
	if task.TeamID != nil {
		t.Errorf("TeamID = %v, want nil", task.TeamID)
	}
}

func TestRemoveNonexistentReturnsNil(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	if removed := reg.Remove("missing"); removed != nil {
		t.Errorf("expected nil, got %v", removed)
	}
}

func TestAssignTeamRejectsMissingTask(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	err := reg.AssignTeam("missing", "team_123")
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestValidatePacketRejectsEmptyFields(t *testing.T) {
	t.Parallel()
	packet := TaskPacket{
		Objective:         " ",
		Scope:             "",
		Repo:              "",
		BranchPolicy:      "\t",
		AcceptanceTests:   []string{"ok", " "},
		CommitPolicy:      "",
		ReportingContract: "",
		EscalationPolicy:  "",
	}

	_, err := ValidatePacket(packet)
	if err == nil {
		t.Fatal("expected error")
	}
	var valErr *TaskPacketValidationError
	if !errors.As(err, &valErr) {
		t.Fatalf("expected TaskPacketValidationError, got %T", err)
	}
	if len(valErr.Errors) < 7 {
		t.Errorf("len(Errors) = %d, want >= 7", len(valErr.Errors))
	}
}

func TestConcurrentRegistryAccess(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			task := reg.Create("concurrent", nil)
			reg.Get(task.TaskID)
			reg.List(nil)
			reg.AppendOutput(task.TaskID, "data")
			reg.Update(task.TaskID, "msg")
		}()
	}
	wg.Wait()

	if reg.Len() != 100 {
		t.Errorf("Len = %d, want 100", reg.Len())
	}
}

// TestSetStatusAllowsTerminalStateTransition verifies that SetStatus does NOT
// guard against terminal states — matching Rust's set_status() which allows
// unconditional status changes for recovery-driven transitions.
func TestSetStatusAllowsTerminalStateTransition(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()

	task := reg.Create("recovery target", nil)
	// Move to Completed (terminal).
	if err := reg.SetStatus(task.TaskID, StatusCompleted); err != nil {
		t.Fatalf("SetStatus(Completed) error: %v", err)
	}
	// SetStatus on a terminal task must succeed (Rust parity).
	if err := reg.SetStatus(task.TaskID, StatusRunning); err != nil {
		t.Fatalf("SetStatus(Running) on completed task should succeed, got: %v", err)
	}
	got, ok := reg.Get(task.TaskID)
	if !ok {
		t.Fatal("task should exist")
	}
	if got.Status != StatusRunning {
		t.Errorf("Status = %v, want %v", got.Status, StatusRunning)
	}
}

// TestUpdateAllowsTerminalStateMessages verifies that Update does NOT guard
// against terminal states — matching Rust's update() which allows post-mortem
// annotations on completed/failed tasks.
func TestUpdateAllowsTerminalStateMessages(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()

	task := reg.Create("annotatable", nil)
	reg.SetStatus(task.TaskID, StatusCompleted)

	// Update on a terminal task must succeed (Rust parity).
	updated, err := reg.Update(task.TaskID, "post-mortem note")
	if err != nil {
		t.Fatalf("Update on completed task should succeed, got: %v", err)
	}
	if len(updated.Messages) != 1 {
		t.Fatalf("len(Messages) = %d, want 1", len(updated.Messages))
	}
	if updated.Messages[0].Content != "post-mortem note" {
		t.Errorf("Messages[0].Content = %q, want %q", updated.Messages[0].Content, "post-mortem note")
	}
}

func TestSetStatusAllowsNonTerminalTransition(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	task := reg.Create("transition", nil)

	// Created → Running should work
	if err := reg.SetStatus(task.TaskID, StatusRunning); err != nil {
		t.Fatalf("SetStatus(Running) error: %v", err)
	}

	got, ok := reg.Get(task.TaskID)
	if !ok {
		t.Fatal("task should exist")
	}
	if got.Status != StatusRunning {
		t.Errorf("Status = %v, want %v", got.Status, StatusRunning)
	}
}
