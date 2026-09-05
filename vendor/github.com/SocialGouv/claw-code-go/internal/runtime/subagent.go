package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/SocialGouv/claw-code-go/internal/api"
	"github.com/SocialGouv/claw-code-go/internal/runtime/task"
	"github.com/SocialGouv/claw-code-go/internal/tools"
)

// Runtime-defined subagents: the model (or an embedder) can define a named
// subagent type for the current session — a system prompt plus a tool
// allow-list — and then spawn it through the agent tool. Spawns run a child
// ConversationLoop in the background, stream their transcript into the task
// registry (task_output/task_get read it), and announce completion through
// the <system-reminder> queue at the next turn boundary, so the parent never
// polls.

// subagentTimeout bounds one background subagent run end to end.
const subagentTimeout = 10 * time.Minute

// subagentOutputCap bounds the transcript stored on the task (the head is
// kept; a truncation marker is appended past the cap).
const subagentOutputCap = 256 * 1024

// SubagentDef is a session-scoped subagent type.
type SubagentDef struct {
	Name         string
	Description  string
	SystemPrompt string
	AllowedTools []string // empty = every tool (minus orchestration tools)
	Model        string   // optional model override
}

// builtinSubagentTypes are the types AllowedToolsForSubagent already ships;
// define_subagent cannot shadow them.
var builtinSubagentTypes = map[string]struct{}{
	"explore": {}, "plan": {}, "verification": {}, "general-purpose": {},
}

// orchestrationTools are excluded from every child toolset: a subagent must
// not spawn or redefine subagents nor run workflows (unbounded recursion),
// nor stop its parent's tasks. The oracle stays available — consulting a
// read-only advisor from a child is legitimate.
var orchestrationTools = map[string]struct{}{
	"agent": {}, "define_subagent": {}, "workflow": {}, "task_stop": {},
}

// DefineSubagent registers a session-scoped subagent type.
func (loop *ConversationLoop) DefineSubagent(def SubagentDef) error {
	name := strings.TrimSpace(def.Name)
	if name == "" {
		return fmt.Errorf("define_subagent: 'name' is required")
	}
	if _, builtin := builtinSubagentTypes[name]; builtin {
		return fmt.Errorf("define_subagent: %q is a built-in subagent type and cannot be redefined", name)
	}
	if strings.TrimSpace(def.SystemPrompt) == "" {
		return fmt.Errorf("define_subagent: 'system_prompt' is required")
	}
	def.Name = name

	loop.subagentMu.Lock()
	defer loop.subagentMu.Unlock()
	if loop.subagentDefs == nil {
		loop.subagentDefs = make(map[string]SubagentDef)
	}
	loop.subagentDefs[name] = def
	return nil
}

// lookupSubagent returns a runtime-defined subagent type.
func (loop *ConversationLoop) lookupSubagent(name string) (SubagentDef, bool) {
	loop.subagentMu.Lock()
	defer loop.subagentMu.Unlock()
	def, ok := loop.subagentDefs[name]
	return def, ok
}

// SubagentTypes lists the available subagent types: built-ins first, then
// session-defined ones (sorted by name for stable rendering).
func (loop *ConversationLoop) SubagentTypes() []string {
	names := []string{"explore", "plan", "verification", "general-purpose"}
	loop.subagentMu.Lock()
	defer loop.subagentMu.Unlock()
	for name := range loop.subagentDefs {
		names = append(names, name)
	}
	return names
}

// resolveSubagentTools filters the parent's toolset for a subagent type:
// a runtime-defined type uses its allow-list (empty = everything), a
// built-in type uses tools.AllowedToolsForSubagent. Orchestration tools are
// always excluded.
func (loop *ConversationLoop) resolveSubagentTools(subagentType string) []api.Tool {
	var allowed map[string]bool
	if def, ok := loop.lookupSubagent(subagentType); ok {
		if len(def.AllowedTools) > 0 {
			allowed = make(map[string]bool, len(def.AllowedTools))
			for _, name := range def.AllowedTools {
				allowed[name] = true
			}
		}
	} else {
		allowed = tools.AllowedToolsForSubagent(subagentType)
	}

	out := make([]api.Tool, 0, len(loop.Tools))
	for _, t := range loop.Tools {
		if _, excluded := orchestrationTools[t.Name]; excluded {
			continue
		}
		if allowed != nil && !allowed[t.Name] {
			continue
		}
		out = append(out, t)
	}
	return out
}

// executeAgentSpawn is the agent tool's dispatch entry: validate, spawn in
// the background, and report the running task (task_id is the handle for
// task_get/task_output).
func (loop *ConversationLoop) executeAgentSpawn(input map[string]any) (string, error) {
	spec, err := tools.ValidateAgentInput(input)
	if err != nil {
		return "", err
	}
	t, err := loop.spawnSubagent(spec, true)
	if err != nil {
		return "", err
	}
	result := map[string]any{
		"task_id":       t.TaskID,
		"agent_id":      spec.AgentID,
		"name":          spec.Name,
		"description":   spec.Description,
		"subagent_type": spec.SubagentType,
		"model":         loop.effectiveSubagentModel(spec),
		"status":        "running",
		"note":          "The sub-agent runs in the background; you will be notified when it finishes. Check progress with task_output.",
	}
	out, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// executeDefineSubagent is the define_subagent tool's dispatch entry.
func (loop *ConversationLoop) executeDefineSubagent(input map[string]any) (string, error) {
	spec, err := tools.ValidateDefineSubagentInput(input)
	if err != nil {
		return "", err
	}
	def := SubagentDef{
		Name:         spec.Name,
		Description:  spec.Description,
		SystemPrompt: spec.SystemPrompt,
		AllowedTools: spec.AllowedTools,
		Model:        spec.Model,
	}
	if err := loop.DefineSubagent(def); err != nil {
		return "", err
	}
	toolCount := len(loop.resolveSubagentTools(spec.Name))
	result := map[string]any{
		"defined":       spec.Name,
		"tools_visible": toolCount,
		"note":          fmt.Sprintf("Spawn it with the agent tool: subagent_type=%q.", spec.Name),
	}
	out, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// spawnSubagent registers a task for the spec and launches the child loop in
// the background. Returns the task snapshot the agent tool reports. notify
// controls the completion <system-reminder>: direct agent-tool spawns want
// it; workflow-orchestrated spawns don't (the workflow aggregates results
// itself and N reminders would be noise).
func (loop *ConversationLoop) spawnSubagent(spec *tools.AgentSpec, notify bool) (task.Task, error) {
	if loop.TaskRegistry == nil {
		return task.Task{}, fmt.Errorf("agent: task registry not available")
	}
	def, isCustom := loop.lookupSubagent(spec.SubagentType)
	if !isCustom {
		if _, builtin := builtinSubagentTypes[spec.SubagentType]; !builtin {
			return task.Task{}, fmt.Errorf("agent: unknown subagent_type %q (built-ins: explore, plan, verification, general-purpose; define others with define_subagent)", spec.SubagentType)
		}
	}

	t, err := loop.TaskRegistry.CreateWithSpec(task.TaskSpec{
		Prompt:     spec.Prompt,
		Subject:    spec.Description,
		Owner:      spec.Name,
		ActiveForm: "Running sub-agent " + spec.Name,
	})
	if err != nil {
		return task.Task{}, err
	}
	if err := loop.TaskRegistry.SetStatus(t.TaskID, task.StatusRunning); err != nil {
		return task.Task{}, err
	}

	go loop.runSubagent(t.TaskID, spec, def, isCustom, notify)

	got, _ := loop.TaskRegistry.Get(t.TaskID)
	return got, nil
}

// runSubagent executes the child conversation loop to completion and
// reports back: transcript → task output, status → completed/failed, and a
// queued <system-reminder> so the parent learns of the completion at its
// next turn boundary without polling.
func (loop *ConversationLoop) runSubagent(taskID string, spec *tools.AgentSpec, def SubagentDef, isCustom bool, notify bool) {
	ctx, cancel := context.WithTimeout(context.Background(), subagentTimeout)
	defer cancel()

	childCfg := *loop.Config
	childCfg.NoSave = true
	childCfg.SessionDir = ""
	if isCustom && def.SystemPrompt != "" {
		// The definition's prompt replaces the authored base (custom
		// personas own their posture); context sections keep their toggles.
		childCfg.SystemPrompt = def.SystemPrompt
	}
	switch {
	case spec.Model != "":
		childCfg.Model = spec.Model
	case isCustom && def.Model != "":
		childCfg.Model = def.Model
	}

	child := &ConversationLoop{
		Client:         loop.Client,
		Session:        NewSession(),
		Tools:          loop.resolveSubagentTools(spec.SubagentType),
		Permissions:    loop.Permissions,
		PermManager:    loop.PermManager,
		Config:         &childCfg,
		CtxAssembler:   loop.CtxAssembler,
		HookRunner:     loop.HookRunner,
		LifecycleHooks: loop.LifecycleHooks,
	}

	events := make(chan TurnEvent, 64)
	done := make(chan error, 1)
	go func() {
		done <- child.SendMessageStreaming(ctx, spec.Prompt, events)
		close(events)
	}()

	var out strings.Builder
	drainSubagentEvents(events, &out)
	err := <-done

	// A child that returned its result through structured_output hands the
	// parent a typed value, not only a transcript (the workflow tool's
	// agent(…, {schema}) resolves with it).
	if payload := child.lastStructuredOutput(); payload != nil {
		loop.storeSubagentStructured(taskID, payload)
	}

	transcript := out.String()
	if len(transcript) > subagentOutputCap {
		transcript = transcript[:subagentOutputCap] + "\n… [output truncated]"
	}
	if appendErr := loop.TaskRegistry.AppendOutput(taskID, transcript); appendErr != nil {
		err = appendErr
	}

	status := task.StatusCompleted
	verdict := "completed"
	if err != nil {
		status = task.StatusFailed
		verdict = "failed: " + err.Error()
		_ = loop.TaskRegistry.AppendOutput(taskID, "\n[subagent error] "+err.Error())
	}
	_ = loop.TaskRegistry.SetStatus(taskID, status)

	if notify {
		loop.QueueSystemReminder(fmt.Sprintf(
			"Sub-agent %q (%s) %s — task %s. Preview:\n%s\nRetrieve the full report with task_output; it is not shown to the user, relay what matters.",
			spec.Name, spec.SubagentType, verdict, taskID, excerpt(transcript, 400)))
	}
}

// effectiveSubagentModel resolves the model a spawn will run on: explicit
// spec override → subagent-type override → the session model.
func (loop *ConversationLoop) effectiveSubagentModel(spec *tools.AgentSpec) string {
	if spec.Model != "" {
		return spec.Model
	}
	if def, ok := loop.lookupSubagent(spec.SubagentType); ok && def.Model != "" {
		return def.Model
	}
	if loop.Config != nil {
		return loop.Config.Model
	}
	return ""
}

// drainSubagentEvents consumes a child's TurnEvents, accumulating the
// transcript and auto-answering interactive events: a background agent
// cannot prompt anyone, so permission asks are denied and ask_user receives
// an unattended notice instead of blocking forever.
func drainSubagentEvents(events <-chan TurnEvent, out *strings.Builder) {
	for ev := range events {
		switch ev.Type {
		case TurnEventTextDelta:
			out.WriteString(ev.Text)
		case TurnEventToolStart:
			fmt.Fprintf(out, "\n[tool: %s] %s\n", ev.ToolName, excerpt(ev.ToolInput, 120))
		case TurnEventError:
			if ev.Err != nil {
				fmt.Fprintf(out, "\n[error] %v\n", ev.Err)
			}
		case TurnEventPermissionAsk:
			if ev.PermReply != nil {
				ev.PermReply <- PermDecisionDeny
			}
		case TurnEventAskUser:
			if ev.AskUserReply != nil {
				ev.AskUserReply <- "This sub-agent runs unattended; no user is available to answer. Proceed with your best judgment and note the open question in your report."
			}
		}
	}
}

// excerpt returns the head of s bounded to n bytes, on a rune boundary.
func excerpt(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}
