package claudesdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Prompt sends a one-shot prompt to the Claude CLI and returns the result.
// The CLI process is spawned with --print mode and exits after the response.
func Prompt(ctx context.Context, prompt string, opts ...Option) (*ResultMessage, error) {
	if prompt == "" {
		return nil, ErrEmptyPrompt
	}

	cfg := applyOptions(opts)

	cliPath, err := resolveCLIPath(cfg)
	if err != nil {
		return nil, ErrCLINotFound
	}

	procCfg := configToProcess(cfg)

	// A gated task's hooks have to reach THIS spawn too. Hooks travel in an
	// `initialize` control request on stdin, and the CLI only reads stdin as a
	// protocol stream under `--input-format stream-json`: with the prompt in
	// argv it never looks. So a hooked one-shot run carries the prompt on
	// stdin as a user message instead, exactly as a Session's first turn does.
	// Without hooks nothing changes — the prompt stays the last argument and
	// stdin stays closed, so the CLI does not wait on it.
	hooked := len(cfg.hooks) > 0
	args := buildArgs(procCfg, hooked)
	if !hooked {
		args = append(args, prompt)
	}

	spOpts := spawnOptions{
		Cwd:            cfg.cwd,
		Env:            cfg.env,
		StderrCallback: cfg.stderrCallback,
		CommandBuilder: cfg.commandBuilder,
		// Unhooked one-shot Prompt(): the prompt is the last CLI argument,
		// nothing is ever written to stdin. Leave it attached to /dev/null so
		// the CLI doesn't emit "no stdin data received in 3s, ...".
		OpenStdin: hooked,
	}

	proc, err := spawnProcess(ctx, cliPath, args, spOpts)
	if err != nil {
		return nil, err
	}

	// The channel opens BEFORE the prompt is written: FIFO ordering on stdin
	// is what makes the CLI register the hooks before it processes the turn
	// their gate has to bound.
	hookCh := newHookChannel()
	ctrl := newController(proc.writeLine)
	var initRequestID string
	if hooked {
		var err error
		initRequestID, err = hookCh.sendInitialize(ctrl, cfg.hooks)
		if err != nil {
			_ = proc.close()
			return nil, fmt.Errorf("claudesdk: one-shot prompt could not register its hooks, so its gate would not bound this spawn: %w", err)
		}
		if err := proc.writeLine(map[string]any{
			"type": "user",
			"message": map[string]any{
				"role":    "user",
				"content": prompt,
			},
		}); err != nil {
			_ = proc.close()
			return nil, fmt.Errorf("claudesdk: one-shot prompt could not be written to stdin: %w", err)
		}
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
			handleControlRequestOneShot(ctx, cfg, ctrl, hookCh, rm.Data)
			continue
		}

		// A refused registration must not read as a successful one. The
		// initialize is sent without waiting, so nothing routes its answer:
		// the engine's "error" response is the ONLY place a CLI that does not
		// accept hooks on this path says so, and a spawn that proceeded past
		// it would run the task's gate nowhere while looking gated.
		if isControlResponse(rm) {
			if refusal := initializeRefusal(rm.Data, initRequestID); refusal != "" {
				_ = proc.close()
				return nil, fmt.Errorf("claudesdk: the CLI refused the one-shot hook registration (%s), so this spawn would run with the task's gate absent", refusal)
			}
			continue
		}

		if len(cfg.messageObservers) > 0 && isMessage(rm) {
			msg, err := unmarshalMessage(rm.Data)
			if err != nil {
				// The cause only: the raw line can carry what a tool printed.
				var perr *ParseError
				if errors.As(err, &perr) {
					err = perr.Err
				}
				cfg.errorf("claudesdk: a %q message reached no observer: %v", rm.Type, err)
			} else {
				for _, observe := range cfg.messageObservers {
					observe(msg)
				}
			}
		}

		if rm.Type == "result" {
			var r ResultMessage
			if err := json.Unmarshal(rm.Data, &r); err != nil {
				_ = proc.close()
				return nil, &ParseError{Raw: rm.Data, Err: err}
			}
			result = &r
			// A hooked spawn reads stdin as a protocol stream, so the CLI
			// does not exit at the end of a turn — it waits for more input.
			// Waiting for EOF here would be a circular wait: the only stdin
			// close is proc.close(), below this loop. The result is the end
			// of the one turn a one-shot prompt has, so stop reading and let
			// close() run the shutdown ladder, exactly as Session.Stream
			// returns on its ResultMessage. Stdin cannot be half-closed
			// earlier: hook callbacks are answered on it.
			if hooked {
				break
			}
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

// initializeRefusal reports the engine's error when a control response
// refuses the initialize request sent under wantID, and "" for anything else.
// An empty wantID means no registration was sent, so nothing can refuse it.
func initializeRefusal(data json.RawMessage, wantID string) string {
	if wantID == "" {
		return ""
	}
	var resp controlResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return ""
	}
	if resp.Response.RequestID != wantID || resp.Response.Subtype != "error" {
		return ""
	}
	if resp.Response.Error == "" {
		return "no reason given"
	}
	return resp.Response.Error
}

// handleControlRequestOneShot handles control requests in one-shot mode. It
// answers hook_callback through the same channel the Session path uses: a
// spawn that registered hooks and then refused their callbacks would hold no
// gate at all, only a slower one.
func handleControlRequestOneShot(ctx context.Context, cfg *config, ctrl *controller, hooks *hookChannel, data json.RawMessage) {
	var req controlRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return
	}

	subtype, err := parseRequestSubtype(req.Request)
	if err != nil {
		return
	}

	switch subtype {
	case "can_use_tool":
		handleCanUseTool(cfg, ctrl, req)
	case "hook_callback":
		hooks.handleCallback(ctx, cfg, ctrl, req)
	default:
		if err := ctrl.sendErrorResponse(req.RequestID, "unsupported request: "+subtype); err != nil {
			cfg.errorf("claudesdk: failed to deliver error response (request_id=%s, subtype=%s): %v", req.RequestID, subtype, err)
		}
	}
}

// handleCanUseTool processes a permission request from the CLI.
func handleCanUseTool(cfg *config, ctrl *controller, req controlRequest) {
	if cfg.canUseTool == nil {
		// Default: allow all
		if sendErr := ctrl.sendResponse(req.RequestID, "success", map[string]any{
			"behavior": "allow",
		}); sendErr != nil {
			cfg.errorf("claudesdk: failed to deliver can_use_tool decision (request_id=%s): %v", req.RequestID, sendErr)
		}
		return
	}

	var body struct {
		ToolName string         `json:"tool_name"`
		Input    map[string]any `json:"input"`
	}
	if err := json.Unmarshal(req.Request, &body); err != nil {
		if sendErr := ctrl.sendErrorResponse(req.RequestID, "parse error: "+err.Error()); sendErr != nil {
			cfg.errorf("claudesdk: failed to deliver can_use_tool parse-error response (request_id=%s): %v", req.RequestID, sendErr)
		}
		return
	}

	decision, err := cfg.canUseTool(body.ToolName, body.Input)
	if err != nil {
		if sendErr := ctrl.sendErrorResponse(req.RequestID, err.Error()); sendErr != nil {
			cfg.errorf("claudesdk: failed to deliver can_use_tool error response (request_id=%s, tool=%s, cause=%v): %v", req.RequestID, body.ToolName, err, sendErr)
		}
		return
	}

	if sendErr := ctrl.sendResponse(req.RequestID, "success", map[string]any{
		"behavior": decision,
	}); sendErr != nil {
		cfg.errorf("claudesdk: failed to deliver can_use_tool decision (request_id=%s): %v", req.RequestID, sendErr)
	}
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
		ReplayUserMessages:     cfg.replayUserMessages,
		ThinkingDisplay:        cfg.thinkingDisplay,
		Resume:                 cfg.resume,
		ForkSession:            cfg.forkSession,
		ContinueConversation:   cfg.continueConversation,
		NoSessionPersistence:   cfg.noSessionPersistence,
		StrictMCPConfig:        cfg.strictMCPConfig,
		OutputFormat:           cfg.outputFormat,
		AddDirs:                cfg.addDirs,
		SettingSources:         cfg.settingSources,
		SettingsJSON:           cfg.settingsJSON,
	}

	if len(cfg.agents) > 0 {
		if b, err := json.Marshal(cfg.agents); err == nil {
			pc.AgentsJSON = b
		}
	}

	return pc
}
