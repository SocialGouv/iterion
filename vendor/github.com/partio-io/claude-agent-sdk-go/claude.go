package claude

import (
	"context"
	"encoding/json"
	"io"

	"github.com/partio-io/claude-agent-sdk-go/internal/process"
	"github.com/partio-io/claude-agent-sdk-go/internal/protocol"
)

// Prompt sends a one-shot prompt to the Claude CLI and returns the result.
// The CLI process is spawned with --print mode and exits after the response.
func Prompt(ctx context.Context, prompt string, opts ...Option) (*ResultMessage, error) {
	if prompt == "" {
		return nil, ErrEmptyPrompt
	}

	cfg := applyOptions(opts)

	cliPath, err := process.FindCLI(cfg.cliPath)
	if err != nil {
		return nil, ErrCLINotFound
	}

	procCfg := configToProcess(cfg)
	args := process.BuildArgs(procCfg, false)

	// For one-shot mode, append the prompt as the last argument.
	args = append(args, prompt)

	spawnOpts := process.SpawnOptions{
		Cwd:            cfg.cwd,
		Env:            cfg.env,
		StderrCallback: cfg.stderrCallback,
	}

	proc, err := process.Spawn(ctx, cliPath, args, spawnOpts)
	if err != nil {
		return nil, err
	}

	var result *ResultMessage

	for {
		line, err := proc.ReadLine()
		if err == io.EOF {
			break
		}
		if err != nil {
			_ = proc.Close()
			return nil, err
		}
		if len(line) == 0 {
			continue
		}

		rm, err := protocol.ParseLine(line)
		if err != nil {
			continue // skip malformed lines
		}

		// Emit every message to the callback for real-time observability.
		if cfg.messageCallback != nil {
			cfg.messageCallback(rm.Type, rm.Data)
		}

		if protocol.IsControlRequest(rm) {
			handleControlRequestOneShot(cfg, proc, rm.Data)
			continue
		}

		if rm.Type == "result" {
			var r ResultMessage
			if err := json.Unmarshal(rm.Data, &r); err != nil {
				_ = proc.Close()
				return nil, &ParseError{Raw: rm.Data, Err: err}
			}
			result = &r
		}
	}

	// Wait for the process to finish so ExitCode() is valid.
	_ = proc.Close()

	if result == nil {
		exitCode := proc.ExitCode()
		if exitCode != 0 {
			return nil, &ProcessError{ExitCode: exitCode}
		}
		return nil, &ProcessError{ExitCode: -1, Stderr: "no result message received"}
	}

	return result, nil
}

// handleControlRequestOneShot handles control requests in one-shot mode.
func handleControlRequestOneShot(cfg *config, proc *process.Process, data json.RawMessage) {
	var req protocol.ControlRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return
	}

	subtype, err := protocol.ParseRequestSubtype(req.Request)
	if err != nil {
		return
	}

	ctrl := protocol.NewController(proc.WriteLine)

	switch subtype {
	case "can_use_tool":
		handleCanUseTool(cfg, ctrl, req)
	default:
		_ = ctrl.SendErrorResponse(req.RequestID, "unsupported request: "+subtype)
	}
}

// handleCanUseTool processes a permission request from the CLI.
func handleCanUseTool(cfg *config, ctrl *protocol.Controller, req protocol.ControlRequest) {
	if cfg.canUseTool == nil {
		// Default: allow all
		_ = ctrl.SendResponse(req.RequestID, "success", map[string]any{
			"behavior": "allow",
		})
		return
	}

	var body struct {
		ToolName string         `json:"tool_name"`
		Input    map[string]any `json:"input"`
	}
	if err := json.Unmarshal(req.Request, &body); err != nil {
		_ = ctrl.SendErrorResponse(req.RequestID, "parse error: "+err.Error())
		return
	}

	decision, err := cfg.canUseTool(body.ToolName, body.Input)
	if err != nil {
		_ = ctrl.SendErrorResponse(req.RequestID, err.Error())
		return
	}

	_ = ctrl.SendResponse(req.RequestID, "success", map[string]any{
		"behavior": decision,
	})
}

// configToProcess converts internal config to process.Config.
func configToProcess(cfg *config) process.Config {
	pc := process.Config{
		Model:                  cfg.model,
		SystemPrompt:           cfg.systemPrompt,
		AppendSystemPrompt:     cfg.appendSystemPrompt,
		Verbose:                cfg.verbose,
		AllowedTools:           cfg.allowedTools,
		DisallowedTools:        cfg.disallowedTools,
		PermissionMode:         cfg.permissionMode,
		MaxTurns:               cfg.maxTurns,
		MaxBudgetUSD:           cfg.maxBudgetUSD,
		IncludePartialMessages: cfg.includePartialMessages,
		Resume:                 cfg.resume,
		ForkSession:            cfg.forkSession,
		ContinueConversation:   cfg.continueConversation,
		NoSessionPersistence:   cfg.noSessionPersistence,
		OutputFormat:           cfg.outputFormat,
		AddDirs:                cfg.addDirs,
	}

	if len(cfg.agents) > 0 {
		if b, err := json.Marshal(cfg.agents); err == nil {
			pc.AgentsJSON = b
		}
	}

	return pc
}
