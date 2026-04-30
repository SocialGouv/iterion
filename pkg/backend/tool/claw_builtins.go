package tool

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	clawlsp "github.com/SocialGouv/claw-code-go/pkg/api/lsp"
	clawmcp "github.com/SocialGouv/claw-code-go/pkg/api/mcp"
	clawtask "github.com/SocialGouv/claw-code-go/pkg/api/task"
	clawteam "github.com/SocialGouv/claw-code-go/pkg/api/team"
	clawtools "github.com/SocialGouv/claw-code-go/pkg/api/tools"
	clawworker "github.com/SocialGouv/claw-code-go/pkg/api/worker"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
)

// RegisterClawBuiltins registers the standard claw-code-go built-in tools
// against the given Registry, making them callable by claw-backend agents
// that declare e.g. `tools: [read_file, write_file, bash]` in their .iter
// fixture.
//
// Workspace is forwarded to the bash tool for command validation; pass an
// empty string to skip workspace-based validation. Pass an empty string
// when registering on a registry that may be reused across workspaces.
//
// The set is intentionally curated — these are the seven workflow-grade
// tools that map cleanly onto file IO, shell, search, and HTTP fetch.
// Specialised tools (todo_write, plan_mode, agent, mcp_*, ...) live in
// claw-code-go's internal/tools/ and are not auto-registered here; callers
// that need them should import claw-code-go/pkg/api/tools and register
// individual entries via RegisterClawTool.
func RegisterClawBuiltins(reg *Registry, workspace string) error {
	return RegisterClawBuiltinsWithEnv(reg, workspace, nil)
}

// RegisterClawBuiltinsWithEnv is RegisterClawBuiltins plus an optional
// extraEnv slice (KEY=value entries) that the bash tool will append to
// the inherited environment on every call. Use this when iterion is
// invoked outside its devbox/nix shell and the caller wants to surface
// the project toolchain (go, gofmt, ...) to the LLM-driven shell so
// fixers can validate their patches in-loop. Pass nil for plain
// inheritance.
func RegisterClawBuiltinsWithEnv(reg *Registry, workspace string, bashExtraEnv []string) error {
	bashExec := func(ctx context.Context, input map[string]any) (string, error) {
		if len(bashExtraEnv) > 0 {
			return clawtools.ExecuteBashWithEnv(ctx, input, workspace, bashExtraEnv)
		}
		return clawtools.ExecuteBash(ctx, input, workspace)
	}

	specs := []clawBuiltinSpec{
		{tool: clawtools.ReadFileTool(), exec: clawtools.ExecuteReadFile},
		{tool: clawtools.WriteFileTool(), exec: clawtools.ExecuteWriteFile},
		{tool: clawtools.GlobTool(), exec: clawtools.ExecuteGlob},
		{tool: clawtools.GrepTool(), exec: clawtools.ExecuteGrep},
		{tool: clawtools.FileEditTool(), exec: clawtools.ExecuteFileEdit},
		{tool: clawtools.WebFetchTool(), exec: clawtools.ExecuteWebFetch},
		{tool: clawtools.BashTool(), exec: bashExec},
	}

	for _, s := range specs {
		if err := RegisterClawTool(reg, s.tool, s.exec); err != nil {
			return fmt.Errorf("register %q: %w", s.tool.Name, err)
		}
	}
	return nil
}

// RegisterClawComputerUse registers the vision + desktop-control tools
// against reg: read_image, screenshot, and the unified computer_use
// action dispatcher (left_click / right_click / middle_click /
// double_click / type / key / mouse_move / cursor_position /
// left_click_drag, plus screenshot). These are kept out of the
// default RegisterClawBuiltins set because most iterion workflows are
// headless; opt in via Defaults.IncludeComputerUse when you have an
// agent targeting an X11 display.
//
// read_image returns a JSON payload describing the image plus a
// base64 content block envelope; downstream agents can either inline
// it as commentary or splice the block into their next-turn message
// (multimodal models accept it directly).
//
// screenshot and computer_use shell out to xdotool + ImageMagick
// `import`. On a host without those binaries (or without a display),
// each call returns ErrComputerUseUnavailable wrapped — agents can
// detect the gap with errors.Is rather than parsing strings.
func RegisterClawComputerUse(reg *Registry) error {
	specs := []clawBuiltinSpec{
		{tool: clawtools.ReadImageTool(), exec: clawComputerUseAdapter(clawtools.ExecuteReadImage)},
		{tool: clawtools.ScreenshotTool(), exec: clawComputerUseAdapter(clawtools.ExecuteScreenshot)},
		{tool: clawtools.ComputerUseTool(), exec: clawComputerUseAdapter(clawtools.ExecuteComputerUse)},
	}
	for _, s := range specs {
		if err := RegisterClawTool(reg, s.tool, s.exec); err != nil {
			return fmt.Errorf("register %q: %w", s.tool.Name, err)
		}
	}
	return nil
}

// clawComputerUseAdapter wraps a (ctx, input) → (ReadImageResult, error)
// function into the (ctx, input) → (string, error) signature the
// iterion tool registry expects. The result is JSON-encoded so
// downstream agents see the description + blocks structure verbatim.
// Works for ReadImageResult and ComputerUseResult alike — claw declares
// them as a type alias.
func clawComputerUseAdapter(fn func(context.Context, map[string]any) (clawtools.ReadImageResult, error)) func(context.Context, map[string]any) (string, error) {
	return func(ctx context.Context, input map[string]any) (string, error) {
		res, err := fn(ctx, input)
		if err != nil {
			return "", err
		}
		buf, err := json.Marshal(res)
		if err != nil {
			return "", fmt.Errorf("encode computer-use result: %w", err)
		}
		return string(buf), nil
	}
}

// RegisterClawTool registers a single claw-code-go tool against the
// registry. Use this to add specialised tools that RegisterClawBuiltins
// does not include by default.
func RegisterClawTool(reg *Registry, t api.Tool, exec func(ctx context.Context, input map[string]any) (string, error)) error {
	schemaJSON, err := json.Marshal(t.InputSchema)
	if err != nil {
		return fmt.Errorf("marshal schema: %w", err)
	}
	wrapped := func(ctx context.Context, input json.RawMessage) (string, error) {
		var args map[string]any
		if len(input) > 0 {
			if jerr := json.Unmarshal(input, &args); jerr != nil {
				return "", fmt.Errorf("decode tool input: %w", jerr)
			}
		}
		if args == nil {
			args = map[string]any{}
		}
		return exec(ctx, args)
	}
	return reg.RegisterBuiltin(t.Name, t.Description, schemaJSON, wrapped)
}

type clawBuiltinSpec struct {
	tool api.Tool
	exec func(ctx context.Context, input map[string]any) (string, error)
}

// RegisterClawSimple registers the no-dependency claw tools that don't
// fit naturally into the file-IO/shell/HTTP set RegisterClawBuiltins
// covers: send_user_message, remote_trigger, sleep, notebook_edit,
// repl, structured_output. These are useful for nodes that want
// process-level utilities (timing, cell edits, REPL evaluation) but
// don't need a registry plumbed in.
func RegisterClawSimple(reg *Registry) error {
	specs := []clawBuiltinSpec{
		{tool: clawtools.SendUserMessageTool(), exec: clawtools.ExecuteSendUserMessage},
		{tool: clawtools.RemoteTriggerTool(), exec: clawtools.ExecuteRemoteTrigger},
		{tool: clawtools.SleepTool(), exec: clawtools.ExecuteSleep},
		{tool: clawtools.NotebookEditTool(), exec: clawtools.ExecuteNotebookEdit},
		{tool: clawtools.REPLTool(), exec: clawtools.ExecuteREPL},
		{tool: clawtools.StructuredOutputTool(), exec: clawtools.ExecuteStructuredOutput},
	}
	for _, s := range specs {
		if err := RegisterClawTool(reg, s.tool, s.exec); err != nil {
			return fmt.Errorf("register %q: %w", s.tool.Name, err)
		}
	}
	return nil
}

// RegisterClawTodo registers the `todo_write` tool for read/write of
// the project todo list at .claude/todos.json. No dependency.
func RegisterClawTodo(reg *Registry) error {
	return RegisterClawTool(reg, clawtools.TodoWriteTool(), clawtools.ExecuteTodoWrite)
}

// RegisterAskUser registers claw-code-go's native `ask_user` tool, wired
// to surface the LLM's question through iterion's interaction flow
// (ErrNeedsInteraction → pause → CLI prompt → resume). The tool is
// available to any node with `interaction:` enabled; iterion's executor
// auto-includes it in such nodes' resolved tool list so workflow
// authors don't need to add `ask_user` to their `tools:` field.
//
// The exec callback returns delegate.ErrAskUser, which propagates up
// through executeToolsDirect into claw_backend. The backend converts it
// to a Result with `_needs_interaction: true`, and iterion's existing
// pause/resume machinery handles everything from there. On resume the
// node is re-invoked with the answer mapped under
// delegate.AskUserQuestionKey.
func RegisterAskUser(reg *Registry) error {
	asker := clawtools.NewProgrammaticAsker(func(_ context.Context, q clawtools.Question) (clawtools.Answer, error) {
		return clawtools.Answer{}, &delegate.ErrAskUser{Question: q.Prompt}
	})
	return RegisterClawTool(reg, clawtools.AskUserQuestionTool(),
		func(ctx context.Context, input map[string]any) (string, error) {
			return clawtools.ExecuteAskUser(ctx, asker, input)
		})
}

// RegisterClawSubagents registers the `agent` tool. The internal
// executor only validates and returns the subagent spec; actual
// orchestration is the host's responsibility — iterion's claw backend
// already routes tool_use events back through the engine.
func RegisterClawSubagents(reg *Registry) error {
	return RegisterClawTool(reg, clawtools.AgentTool(), clawtools.ExecuteAgent)
}

// RegisterClawWebSearch registers the `web_search` tool. Reads
// BRAVE_API_KEY (or compatible) at execute time; absence surfaces as
// a tool error.
func RegisterClawWebSearch(reg *Registry) error {
	return RegisterClawTool(reg, clawtools.WebSearchTool(), clawtools.ExecuteWebSearch)
}

// RegisterClawSkill registers the `skill` tool, looking up skills at
// <workDir>/.claude/skills/. Pass an empty workDir to resolve against
// the process CWD.
func RegisterClawSkill(reg *Registry, workDir string) error {
	exec := func(ctx context.Context, input map[string]any) (string, error) {
		return clawtools.ExecuteSkill(ctx, input, workDir)
	}
	return RegisterClawTool(reg, clawtools.SkillTool(), exec)
}

// ToolSnapshot returns the set of tools the model should see as the
// search haystack. Hosts pass a closure (instead of a static slice)
// so the snapshot is captured at execute time, after every other tool
// has been registered.
type ToolSnapshot func() []api.Tool

// RegisterClawToolSearch registers the `tool_search` meta-tool. The
// snapshot closure is invoked on every call to capture the live
// tool catalog; pass nil to use an empty haystack (degraded but valid).
func RegisterClawToolSearch(reg *Registry, snapshot ToolSnapshot) error {
	exec := func(ctx context.Context, input map[string]any) (string, error) {
		var tools []api.Tool
		if snapshot != nil {
			tools = snapshot()
		}
		return clawtools.ExecuteToolSearch(ctx, input, tools)
	}
	return RegisterClawTool(reg, clawtools.ToolSearchTool(), exec)
}

// RegisterClawPlanMode registers `enter_plan_mode` and `exit_plan_mode`.
// Both tools share a *PlanModeState so they can coordinate the active
// flag and on-disk persistence directory.
func RegisterClawPlanMode(reg *Registry, state *clawtools.PlanModeState) error {
	enterExec := func(ctx context.Context, input map[string]any) (string, error) {
		return clawtools.ExecuteEnterPlanMode(ctx, input, state)
	}
	exitExec := func(ctx context.Context, input map[string]any) (string, error) {
		return clawtools.ExecuteExitPlanMode(ctx, input, state)
	}
	if err := RegisterClawTool(reg, clawtools.EnterPlanModeTool(), enterExec); err != nil {
		return fmt.Errorf("register enter_plan_mode: %w", err)
	}
	if err := RegisterClawTool(reg, clawtools.ExitPlanModeTool(), exitExec); err != nil {
		return fmt.Errorf("register exit_plan_mode: %w", err)
	}
	return nil
}

// RegisterClawTasks registers the seven task_* tools (task_create,
// task_get, task_list, task_output, task_stop, task_update,
// run_task_packet). All share the same *task.Registry; pass a single
// instance built via clawtask.NewRegistry().
func RegisterClawTasks(reg *Registry, taskReg *clawtask.Registry) error {
	if taskReg == nil {
		return fmt.Errorf("register tasks: task registry is nil")
	}
	specs := []struct {
		tool api.Tool
		exec func(ctx context.Context, input map[string]any) (string, error)
	}{
		{clawtools.TaskCreateTool(), func(ctx context.Context, in map[string]any) (string, error) {
			return clawtools.ExecuteTaskCreate(ctx, in, taskReg)
		}},
		{clawtools.TaskGetTool(), func(ctx context.Context, in map[string]any) (string, error) {
			return clawtools.ExecuteTaskGet(ctx, in, taskReg)
		}},
		{clawtools.TaskListTool(), func(ctx context.Context, in map[string]any) (string, error) {
			return clawtools.ExecuteTaskList(ctx, in, taskReg)
		}},
		{clawtools.TaskOutputTool(), func(ctx context.Context, in map[string]any) (string, error) {
			return clawtools.ExecuteTaskOutput(ctx, in, taskReg)
		}},
		{clawtools.TaskStopTool(), func(ctx context.Context, in map[string]any) (string, error) {
			return clawtools.ExecuteTaskStop(ctx, in, taskReg)
		}},
		{clawtools.TaskUpdateTool(), func(ctx context.Context, in map[string]any) (string, error) {
			return clawtools.ExecuteTaskUpdate(ctx, in, taskReg)
		}},
		{clawtools.RunTaskPacketTool(), func(ctx context.Context, in map[string]any) (string, error) {
			return clawtools.ExecuteRunTaskPacket(ctx, in, taskReg)
		}},
	}
	for _, s := range specs {
		if err := RegisterClawTool(reg, s.tool, s.exec); err != nil {
			return fmt.Errorf("register %q: %w", s.tool.Name, err)
		}
	}
	return nil
}

// RegisterClawWorkers registers the nine worker_* tools that share a
// *worker.WorkerRegistry. Workers are subprocess agents the runtime
// spawns and observes; the registry tracks their lifecycle.
func RegisterClawWorkers(reg *Registry, workerReg *clawworker.WorkerRegistry) error {
	if workerReg == nil {
		return fmt.Errorf("register workers: worker registry is nil")
	}
	specs := []struct {
		tool api.Tool
		exec func(ctx context.Context, input map[string]any) (string, error)
	}{
		{clawtools.WorkerCreateTool(), func(ctx context.Context, in map[string]any) (string, error) {
			return clawtools.ExecuteWorkerCreate(ctx, in, workerReg)
		}},
		{clawtools.WorkerGetTool(), func(ctx context.Context, in map[string]any) (string, error) {
			return clawtools.ExecuteWorkerGet(ctx, in, workerReg)
		}},
		{clawtools.WorkerObserveTool(), func(ctx context.Context, in map[string]any) (string, error) {
			return clawtools.ExecuteWorkerObserve(ctx, in, workerReg)
		}},
		{clawtools.WorkerResolveTrustTool(), func(ctx context.Context, in map[string]any) (string, error) {
			return clawtools.ExecuteWorkerResolveTrust(ctx, in, workerReg)
		}},
		{clawtools.WorkerAwaitReadyTool(), func(ctx context.Context, in map[string]any) (string, error) {
			return clawtools.ExecuteWorkerAwaitReady(ctx, in, workerReg)
		}},
		{clawtools.WorkerSendPromptTool(), func(ctx context.Context, in map[string]any) (string, error) {
			return clawtools.ExecuteWorkerSendPrompt(ctx, in, workerReg)
		}},
		{clawtools.WorkerRestartTool(), func(ctx context.Context, in map[string]any) (string, error) {
			return clawtools.ExecuteWorkerRestart(ctx, in, workerReg)
		}},
		{clawtools.WorkerTerminateTool(), func(ctx context.Context, in map[string]any) (string, error) {
			return clawtools.ExecuteWorkerTerminate(ctx, in, workerReg)
		}},
		{clawtools.WorkerObserveCompletionTool(), func(ctx context.Context, in map[string]any) (string, error) {
			return clawtools.ExecuteWorkerObserveCompletion(ctx, in, workerReg)
		}},
	}
	for _, s := range specs {
		if err := RegisterClawTool(reg, s.tool, s.exec); err != nil {
			return fmt.Errorf("register %q: %w", s.tool.Name, err)
		}
	}
	return nil
}

// RegisterClawTeams registers the four team_* tools that share a
// *team.TeamRegistry. Teams are named groupings of task IDs.
func RegisterClawTeams(reg *Registry, teamReg *clawteam.TeamRegistry) error {
	if teamReg == nil {
		return fmt.Errorf("register teams: team registry is nil")
	}
	specs := []struct {
		tool api.Tool
		exec func(ctx context.Context, input map[string]any) (string, error)
	}{
		{clawtools.TeamCreateTool(), func(ctx context.Context, in map[string]any) (string, error) {
			return clawtools.ExecuteTeamCreate(ctx, in, teamReg)
		}},
		{clawtools.TeamGetTool(), func(ctx context.Context, in map[string]any) (string, error) {
			return clawtools.ExecuteTeamGet(ctx, in, teamReg)
		}},
		{clawtools.TeamListTool(), func(ctx context.Context, in map[string]any) (string, error) {
			return clawtools.ExecuteTeamList(ctx, in, teamReg)
		}},
		{clawtools.TeamDeleteTool(), func(ctx context.Context, in map[string]any) (string, error) {
			return clawtools.ExecuteTeamDelete(ctx, in, teamReg)
		}},
	}
	for _, s := range specs {
		if err := RegisterClawTool(reg, s.tool, s.exec); err != nil {
			return fmt.Errorf("register %q: %w", s.tool.Name, err)
		}
	}
	return nil
}

// RegisterClawCron registers the four cron_* tools that share a
// *team.CronRegistry. Cron entries hold scheduled task prompts.
func RegisterClawCron(reg *Registry, cronReg *clawteam.CronRegistry) error {
	if cronReg == nil {
		return fmt.Errorf("register cron: cron registry is nil")
	}
	specs := []struct {
		tool api.Tool
		exec func(ctx context.Context, input map[string]any) (string, error)
	}{
		{clawtools.CronCreateTool(), func(ctx context.Context, in map[string]any) (string, error) {
			return clawtools.ExecuteCronCreate(ctx, in, cronReg)
		}},
		{clawtools.CronGetTool(), func(ctx context.Context, in map[string]any) (string, error) {
			return clawtools.ExecuteCronGet(ctx, in, cronReg)
		}},
		{clawtools.CronListTool(), func(ctx context.Context, in map[string]any) (string, error) {
			return clawtools.ExecuteCronList(ctx, in, cronReg)
		}},
		{clawtools.CronDeleteTool(), func(ctx context.Context, in map[string]any) (string, error) {
			return clawtools.ExecuteCronDelete(ctx, in, cronReg)
		}},
	}
	for _, s := range specs {
		if err := RegisterClawTool(reg, s.tool, s.exec); err != nil {
			return fmt.Errorf("register %q: %w", s.tool.Name, err)
		}
	}
	return nil
}

// RegisterClawMCPResources registers the three MCP resource/auth
// tools (list_mcp_resources, read_mcp_resource, mcp_auth). Hosts
// supply the *mcp.Registry and *mcp.AuthState; callers that don't
// care about per-server auth state may pass a fresh
// clawmcp.NewAuthState() — the tools handle empty state gracefully.
func RegisterClawMCPResources(reg *Registry, mcpReg *clawmcp.Registry, auth *clawmcp.AuthState) error {
	if mcpReg == nil {
		return fmt.Errorf("register mcp resources: mcp registry is nil")
	}
	if auth == nil {
		auth = clawmcp.NewAuthState()
	}
	specs := []struct {
		tool api.Tool
		exec func(ctx context.Context, input map[string]any) (string, error)
	}{
		{clawtools.ListMcpResourcesTool(), func(ctx context.Context, in map[string]any) (string, error) {
			return clawtools.ExecuteListMcpResources(ctx, in, mcpReg)
		}},
		{clawtools.ReadMcpResourceTool(), func(ctx context.Context, in map[string]any) (string, error) {
			return clawtools.ExecuteReadMcpResource(ctx, in, mcpReg)
		}},
		{clawtools.McpAuthTool(), func(ctx context.Context, in map[string]any) (string, error) {
			return clawtools.ExecuteMcpAuth(ctx, in, mcpReg, auth)
		}},
	}
	for _, s := range specs {
		if err := RegisterClawTool(reg, s.tool, s.exec); err != nil {
			return fmt.Errorf("register %q: %w", s.tool.Name, err)
		}
	}
	return nil
}

// ClawDefaults bundles the registries and per-session state the
// optional claw tool families need. Hosts (e.g. iterion's CLI) build
// one ClawDefaults per run and pass it to RegisterClawAll, which
// wires every supported tool against shared in-memory state.
//
// Field zero-values are populated lazily by RegisterClawAll so callers
// can opt into specific subsystems by leaving the rest at zero.
type ClawDefaults struct {
	// Workspace is forwarded to bash for command validation and to the
	// skill tool for skill lookup. Leave empty to skip workspace
	// gating.
	Workspace string

	// Tasks, Workers, Teams, Crons, MCP, LSP hold the registries each
	// tool family needs. Leave nil to have RegisterClawAll allocate a
	// fresh empty registry.
	Tasks   *clawtask.Registry
	Workers *clawworker.WorkerRegistry
	Teams   *clawteam.TeamRegistry
	Crons   *clawteam.CronRegistry
	MCP     *clawmcp.Registry
	MCPAuth *clawmcp.AuthState
	LSP     *clawlsp.Registry

	// PlanMode is shared by enter_plan_mode and exit_plan_mode so the
	// pair can coordinate. Nil disables plan_mode tooling.
	PlanMode *clawtools.PlanModeState

	// IncludeWebSearch toggles registration of the `web_search` tool.
	// Off by default because it requires BRAVE_API_KEY; surfacing it
	// without a key causes runtime errors visible to the model.
	IncludeWebSearch bool

	// IncludeComputerUse toggles read_image / screenshot. Off by
	// default since most workflows don't process images.
	IncludeComputerUse bool

	// BashExtraEnv, when non-empty, is appended to the inherited
	// environment of every bash tool invocation (KEY=value entries).
	// Use this to surface a project-managed toolchain (devbox / nix /
	// asdf) bin path so the LLM-driven shell can run go/gofmt/etc.
	// even when the operator did not prefix the iterion launch with
	// `devbox run --`. Nil/empty means plain os.Environ() inheritance.
	BashExtraEnv []string
}

// RegisterClawAll registers the full curated set of claw tools
// (file IO, shell, search, web fetch, simple utilities, todo,
// subagents, plan mode, tasks, workers, teams, cron, MCP resources,
// LSP, tool_search) against reg using the registries and per-session
// state in defaults. It is the preferred one-shot entry point for
// hosts that want every iterion-supported tool surfaced without
// thinking about which family to wire.
//
// Tools not yet usable from a workflow because the LLM would call
// them without context (web_search → needs BRAVE_API_KEY,
// computer_use → needs vision-capable model) are gated by explicit
// opt-in flags on ClawDefaults.
//
// Calling this twice on the same registry will fail on the second
// call due to duplicate-name detection in the registry.
func RegisterClawAll(reg *Registry, defaults ClawDefaults) error {
	if defaults.Tasks == nil {
		defaults.Tasks = clawtask.NewRegistry()
	}
	if defaults.Workers == nil {
		defaults.Workers = clawworker.NewWorkerRegistry()
	}
	if defaults.Teams == nil {
		defaults.Teams = clawteam.NewTeamRegistry()
	}
	if defaults.Crons == nil {
		defaults.Crons = clawteam.NewCronRegistry()
	}
	if defaults.MCP == nil {
		defaults.MCP = clawmcp.NewRegistry()
	}
	if defaults.MCPAuth == nil {
		defaults.MCPAuth = clawmcp.NewAuthState()
	}
	if defaults.LSP == nil {
		defaults.LSP = clawlsp.NewRegistry()
	}

	if err := RegisterClawBuiltinsWithEnv(reg, defaults.Workspace, defaults.BashExtraEnv); err != nil {
		return err
	}
	if err := RegisterClawSimple(reg); err != nil {
		return err
	}
	if err := RegisterClawTodo(reg); err != nil {
		return err
	}
	if err := RegisterAskUser(reg); err != nil {
		return err
	}
	if err := RegisterClawSubagents(reg); err != nil {
		return err
	}
	if err := RegisterClawSkill(reg, defaults.Workspace); err != nil {
		return err
	}
	if defaults.IncludeWebSearch {
		if err := RegisterClawWebSearch(reg); err != nil {
			return err
		}
	}
	if defaults.IncludeComputerUse {
		if err := RegisterClawComputerUse(reg); err != nil {
			return err
		}
	}
	if defaults.PlanMode != nil {
		if err := RegisterClawPlanMode(reg, defaults.PlanMode); err != nil {
			return err
		}
	}
	if err := RegisterClawTasks(reg, defaults.Tasks); err != nil {
		return err
	}
	if err := RegisterClawWorkers(reg, defaults.Workers); err != nil {
		return err
	}
	if err := RegisterClawTeams(reg, defaults.Teams); err != nil {
		return err
	}
	if err := RegisterClawCron(reg, defaults.Crons); err != nil {
		return err
	}
	if err := RegisterClawMCPResources(reg, defaults.MCP, defaults.MCPAuth); err != nil {
		return err
	}
	if err := RegisterClawLSP(reg, defaults.LSP); err != nil {
		return err
	}

	// tool_search is registered last so its snapshot closure observes
	// every other tool already in the registry.
	snapshot := func() []api.Tool { return registrySnapshot(reg) }
	if err := RegisterClawToolSearch(reg, snapshot); err != nil {
		return err
	}
	return nil
}

// registrySnapshot rebuilds an api.Tool slice from the registry's
// current contents. Used by tool_search to look up tools by intent.
func registrySnapshot(reg *Registry) []api.Tool {
	defs := reg.List()
	out := make([]api.Tool, 0, len(defs))
	for _, td := range defs {
		var schema api.InputSchema
		// Best-effort decode; if a tool's schema is malformed we
		// surface it as an empty-property entry rather than dropping
		// the name from search.
		_ = json.Unmarshal(td.InputSchema, &schema)
		out = append(out, api.Tool{
			Name:        td.QualifiedName,
			Description: td.Description,
			InputSchema: schema,
		})
	}
	return out
}

// RegisterClawLSP registers the `lsp` action-dispatcher tool. The
// lsp.Registry tracks connected language servers; build it once via
// clawlsp.NewRegistry().
func RegisterClawLSP(reg *Registry, lspReg *clawlsp.Registry) error {
	if lspReg == nil {
		return fmt.Errorf("register lsp: lsp registry is nil")
	}
	exec := func(ctx context.Context, in map[string]any) (string, error) {
		return clawtools.ExecuteLSP(ctx, in, lspReg)
	}
	return RegisterClawTool(reg, clawtools.LspTool(), exec)
}
