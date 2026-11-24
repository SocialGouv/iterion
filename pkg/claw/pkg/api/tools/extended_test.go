package tools_test

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/pkg/api/lsp"
	"github.com/SocialGouv/iterion/pkg/claw/pkg/api/mcp"
	"github.com/SocialGouv/iterion/pkg/claw/pkg/api/task"
	"github.com/SocialGouv/iterion/pkg/claw/pkg/api/team"
	"github.com/SocialGouv/iterion/pkg/claw/pkg/api/tools"
	"github.com/SocialGouv/iterion/pkg/claw/pkg/api/worker"
)

// TestExtendedToolFactories verifies that every newly-exposed tool
// factory returns a schema with the expected name. This catches
// accidental drift between the public wrappers and internal tools.
func TestExtendedToolFactories(t *testing.T) {
	cases := []struct {
		name   string
		tool   string
		actual string
	}{
		{"todo_write", "todo_write", tools.TodoWriteTool().Name},
		{"web_search", "web_search", tools.WebSearchTool().Name},
		{"send_user_message", "send_user_message", tools.SendUserMessageTool().Name},
		{"remote_trigger", "remote_trigger", tools.RemoteTriggerTool().Name},
		{"sleep", "sleep", tools.SleepTool().Name},
		{"notebook_edit", "notebook_edit", tools.NotebookEditTool().Name},
		{"repl", "repl", tools.REPLTool().Name},
		{"agent", "agent", tools.AgentTool().Name},
		{"structured_output", "structured_output", tools.StructuredOutputTool().Name},
		{"task_create", "task_create", tools.TaskCreateTool().Name},
		{"task_get", "task_get", tools.TaskGetTool().Name},
		{"task_list", "task_list", tools.TaskListTool().Name},
		{"task_output", "task_output", tools.TaskOutputTool().Name},
		{"task_stop", "task_stop", tools.TaskStopTool().Name},
		{"task_update", "task_update", tools.TaskUpdateTool().Name},
		{"run_task_packet", "run_task_packet", tools.RunTaskPacketTool().Name},
		{"worker_create", "worker_create", tools.WorkerCreateTool().Name},
		{"worker_get", "worker_get", tools.WorkerGetTool().Name},
		{"worker_observe", "worker_observe", tools.WorkerObserveTool().Name},
		{"worker_resolve_trust", "worker_resolve_trust", tools.WorkerResolveTrustTool().Name},
		{"worker_await_ready", "worker_await_ready", tools.WorkerAwaitReadyTool().Name},
		{"worker_send_prompt", "worker_send_prompt", tools.WorkerSendPromptTool().Name},
		{"worker_restart", "worker_restart", tools.WorkerRestartTool().Name},
		{"worker_terminate", "worker_terminate", tools.WorkerTerminateTool().Name},
		{"worker_observe_completion", "worker_observe_completion", tools.WorkerObserveCompletionTool().Name},
		{"team_create", "team_create", tools.TeamCreateTool().Name},
		{"team_get", "team_get", tools.TeamGetTool().Name},
		{"team_list", "team_list", tools.TeamListTool().Name},
		{"team_delete", "team_delete", tools.TeamDeleteTool().Name},
		{"cron_create", "cron_create", tools.CronCreateTool().Name},
		{"cron_get", "cron_get", tools.CronGetTool().Name},
		{"cron_list", "cron_list", tools.CronListTool().Name},
		{"cron_delete", "cron_delete", tools.CronDeleteTool().Name},
		{"list_mcp_resources", "list_mcp_resources", tools.ListMcpResourcesTool().Name},
		{"read_mcp_resource", "read_mcp_resource", tools.ReadMcpResourceTool().Name},
		{"mcp_auth", "mcp_auth", tools.McpAuthTool().Name},
		{"lsp", "lsp", tools.LspTool().Name},
		{"enter_plan_mode", "enter_plan_mode", tools.EnterPlanModeTool().Name},
		{"exit_plan_mode", "exit_plan_mode", tools.ExitPlanModeTool().Name},
		{"skill", "skill", tools.SkillTool().Name},
		{"tool_search", "tool_search", tools.ToolSearchTool().Name},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.actual != c.tool {
				t.Fatalf("expected name %q, got %q", c.tool, c.actual)
			}
		})
	}
}

// TestRegistriesConstructable verifies the public façade packages can
// construct their registries without panicking.
func TestRegistriesConstructable(t *testing.T) {
	if r := task.NewRegistry(); r == nil {
		t.Fatal("task.NewRegistry returned nil")
	}
	if r := worker.NewWorkerRegistry(); r == nil {
		t.Fatal("worker.NewWorkerRegistry returned nil")
	}
	if r := team.NewTeamRegistry(); r == nil {
		t.Fatal("team.NewTeamRegistry returned nil")
	}
	if r := team.NewCronRegistry(); r == nil {
		t.Fatal("team.NewCronRegistry returned nil")
	}
	if r := mcp.NewRegistry(); r == nil {
		t.Fatal("mcp.NewRegistry returned nil")
	}
	if a := mcp.NewAuthState(); a == nil {
		t.Fatal("mcp.NewAuthState returned nil")
	}
	if r := lsp.NewRegistry(); r == nil {
		t.Fatal("lsp.NewRegistry returned nil")
	}
}

// TestExecuteTodoWriteRead exercises the simple-case Execute pattern.
// It uses action=read in a tmp dir, expecting an empty list since no
// todos.json exists.
func TestExecuteTodoWriteRead(t *testing.T) {
	tmp := t.TempDir()
	t.Chdir(tmp)
	out, err := tools.ExecuteTodoWrite(context.Background(), map[string]any{"action": "read"})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if out != "[]" {
		t.Fatalf("expected empty list, got %q", out)
	}
}
