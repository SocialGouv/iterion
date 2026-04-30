package tools

import (
	"context"

	intl "github.com/SocialGouv/claw-code-go/internal/tools"
	"github.com/SocialGouv/claw-code-go/pkg/api"
)

func TodoWriteTool() api.Tool { return intl.TodoWriteTool() }

func ExecuteTodoWrite(ctx context.Context, input map[string]any) (string, error) {
	return intl.ExecuteTodoWrite(input)
}

func WebSearchTool() api.Tool { return intl.WebSearchTool() }

// ExecuteWebSearch reads BRAVE_API_KEY (or compatible) from env;
// absence surfaces as a tool error to the model.
func ExecuteWebSearch(ctx context.Context, input map[string]any) (string, error) {
	return intl.ExecuteWebSearch(input)
}

func SendUserMessageTool() api.Tool { return intl.SendUserMessageTool() }

func ExecuteSendUserMessage(ctx context.Context, input map[string]any) (string, error) {
	return intl.ExecuteSendUserMessage(input)
}

func RemoteTriggerTool() api.Tool { return intl.RemoteTriggerTool() }

func ExecuteRemoteTrigger(ctx context.Context, input map[string]any) (string, error) {
	return intl.ExecuteRemoteTrigger(ctx, input)
}

// AskUserQuestionTool returns the schema for the ask_user tool.
func AskUserQuestionTool() api.Tool { return intl.AskUserQuestionTool() }

// Asker is the interface SDK consumers implement to deliver a question to a
// human (or a simulated batch source) and return the answer.
type Asker = intl.Asker

// Question is the structured payload passed to an Asker.
type Question = intl.Question

// Option is a single selectable answer.
type Option = intl.Option

// Answer is what the Asker returns.
type Answer = intl.Answer

// ErrNoAsker is returned by ExecuteAskUser when no Asker is wired.
var ErrNoAsker = intl.ErrNoAsker

// NewStdinAsker returns an Asker bound to os.Stdin / os.Stdout.
func NewStdinAsker() *intl.StdinAsker { return intl.NewStdinAsker() }

// NewProgrammaticAsker wraps a handler closure as an Asker.
func NewProgrammaticAsker(h func(ctx context.Context, q Question) (Answer, error)) *intl.ProgrammaticAsker {
	return &intl.ProgrammaticAsker{Handler: h}
}

// ExecuteAskUser runs the ask_user tool with the supplied Asker.
func ExecuteAskUser(ctx context.Context, asker Asker, input map[string]any) (string, error) {
	return intl.ExecuteAskUser(ctx, asker, input)
}

func SleepTool() api.Tool { return intl.SleepTool() }

func ExecuteSleep(ctx context.Context, input map[string]any) (string, error) {
	return intl.ExecuteSleep(input)
}

func NotebookEditTool() api.Tool { return intl.NotebookEditTool() }

func ExecuteNotebookEdit(ctx context.Context, input map[string]any) (string, error) {
	return intl.ExecuteNotebookEdit(input)
}

func REPLTool() api.Tool { return intl.REPLTool() }

func ExecuteREPL(ctx context.Context, input map[string]any) (string, error) {
	return intl.ExecuteREPL(input)
}

// AgentTool returns the subagent-spawn tool. The internal executor
// only validates and returns metadata; orchestration is the host's
// job (iterion's claw backend already routes tool_use back).
func AgentTool() api.Tool { return intl.AgentTool() }

func ExecuteAgent(ctx context.Context, input map[string]any) (string, error) {
	return intl.ExecuteAgent(input)
}

// AgentSpec is the validated form of agent tool input. Hosts that
// override the default (metadata-only) agent executor with a real
// subagent runner use ValidateAgentInput to parse the raw input map
// before spawning the child loop.
type AgentSpec = intl.AgentSpec

// ValidateAgentInput parses and validates the raw input map for the
// agent tool. Returns the spec on success or a descriptive error.
func ValidateAgentInput(input map[string]any) (*AgentSpec, error) {
	return intl.ValidateAgentInput(input)
}

// AllowedToolsForSubagent returns the set of tool names a subagent
// of the given type is permitted to call. Returns nil for the
// "general-purpose" / unrecognised types — a nil map signals
// "no restriction" to callers.
func AllowedToolsForSubagent(subagentType string) map[string]bool {
	return intl.AllowedToolsForSubagent(subagentType)
}

// ConfigTool returns the `config` tool that reads from a host-supplied
// configuration map. Hosts wire it via ExecuteConfig with a populated
// map; without a map the tool returns "no configuration available".
func ConfigTool() api.Tool { return intl.ConfigTool() }

// ExecuteConfig reads a value out of `configMap` for the requested key.
func ExecuteConfig(ctx context.Context, input map[string]any, configMap map[string]any) (string, error) {
	return intl.ExecuteConfig(input, configMap)
}

func StructuredOutputTool() api.Tool { return intl.StructuredOutputTool() }

func ExecuteStructuredOutput(ctx context.Context, input map[string]any) (string, error) {
	return intl.ExecuteStructuredOutput(input)
}
