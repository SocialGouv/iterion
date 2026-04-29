package claudesdk

import (
	"context"
	"encoding/json"
	"io"
)

// Prompt sends a one-shot prompt to the Claude CLI and returns the result.
// The CLI process is spawned with --print mode and exits after the response.
func Prompt(ctx context.Context, prompt string, opts ...Option) (*ResultMessage, error) {
	if prompt == "" {
		return nil, ErrEmptyPrompt
	}

	cfg := applyOptions(opts)

	cliPath, err := findCLI(cfg.cliPath)
	if err != nil {
		return nil, ErrCLINotFound
	}

	procCfg := configToProcess(cfg)
	args := buildArgs(procCfg, false)

	// For one-shot mode, append the prompt as the last argument.
	args = append(args, prompt)

	spOpts := spawnOptions{
		Cwd:            cfg.cwd,
		Env:            cfg.env,
		StderrCallback: cfg.stderrCallback,
	}

	proc, err := spawnProcess(ctx, cliPath, args, spOpts)
	if err != nil {
		return nil, err
	}

	var result *ResultMessage

	for {
		line, err := proc.readLine()
		if err == io.EOF {
			break
		}
		if err != nil {
			_ = proc.close()
			return nil, err
		}
		if len(line) == 0 {
			continue
		}

		rm, err := parseLine(line)
		if err != nil {
			continue // skip malformed lines
		}

		// Invoke message callback before processing, if configured.
		if cfg.messageCallback != nil {
			cfg.messageCallback(rm.Type, rm.Data)
		}

		if isControlRequest(rm) {
			handleControlRequestOneShot(cfg, proc, rm.Data)
			continue
		}

		if rm.Type == "result" {
			var r ResultMessage
			if err := json.Unmarshal(rm.Data, &r); err != nil {
				_ = proc.close()
				return nil, &ParseError{Raw: rm.Data, Err: err}
			}
			result = &r
		}
	}

	// Wait for the process to finish so exitCode() is valid.
	_ = proc.close()

	if result == nil {
		exitCode := proc.exitCode()
		if exitCode != 0 {
			return nil, &ProcessError{ExitCode: exitCode}
		}
		return nil, &ProcessError{ExitCode: -1, Stderr: "no result message received"}
	}

	return result, nil
}

// handleControlRequestOneShot handles control requests in one-shot mode.
func handleControlRequestOneShot(cfg *config, proc *cliProcess, data json.RawMessage) {
	var req controlRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return
	}

	subtype, err := parseRequestSubtype(req.Request)
	if err != nil {
		return
	}

	ctrl := newController(proc.writeLine)

	switch subtype {
	case "can_use_tool":
		handleCanUseTool(cfg, ctrl, req)
	default:
		_ = ctrl.sendErrorResponse(req.RequestID, "unsupported request: "+subtype)
	}
}

// handleCanUseTool processes a permission request from the CLI.
func handleCanUseTool(cfg *config, ctrl *controller, req controlRequest) {
	if cfg.canUseTool == nil {
		// Default: allow all
		_ = ctrl.sendResponse(req.RequestID, "success", map[string]any{
			"behavior": "allow",
		})
		return
	}

	var body struct {
		ToolName string         `json:"tool_name"`
		Input    map[string]any `json:"input"`
	}
	if err := json.Unmarshal(req.Request, &body); err != nil {
		_ = ctrl.sendErrorResponse(req.RequestID, "parse error: "+err.Error())
		return
	}

	decision, err := cfg.canUseTool(body.ToolName, body.Input)
	if err != nil {
		_ = ctrl.sendErrorResponse(req.RequestID, err.Error())
		return
	}

	_ = ctrl.sendResponse(req.RequestID, "success", map[string]any{
		"behavior": decision,
	})
}

// configToProcess converts internal config to processConfig.
func configToProcess(cfg *config) processConfig {
	pc := processConfig{
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
