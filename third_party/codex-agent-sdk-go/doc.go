// Package codexsdk provides a Go SDK for interacting with the Codex CLI agent.
//
// This SDK enables Go applications to programmatically communicate with the
// Codex CLI tool. It supports both one-shot queries and interactive multi-turn
// conversations.
//
// # Basic Usage
//
// For simple, one-shot queries, use the Query function:
//
//	ctx := context.Background()
//	for msg, err := range codexsdk.Query(ctx, codexsdk.Text("What is 2+2?"),
//	    codexsdk.WithPermissionMode("acceptEdits"),
//	) {
//	    if err != nil {
//	        log.Fatal(err)
//	    }
//
//	    switch m := msg.(type) {
//	    case *codexsdk.AssistantMessage:
//	        for _, block := range m.Content {
//	            if text, ok := block.(*codexsdk.TextBlock); ok {
//	                fmt.Println(text.Text)
//	            }
//	        }
//	    case *codexsdk.ResultMessage:
//	        if m.Usage != nil {
//	            fmt.Printf("Tokens: %d in / %d out\n", m.Usage.InputTokens, m.Usage.OutputTokens)
//	        }
//	    }
//	}
//
// # Interactive Sessions
//
// For multi-turn conversations, use NewClient or the WithClient helper:
//
//	// Using WithClient for automatic lifecycle management
//	err := codexsdk.WithClient(ctx, func(c codexsdk.Client) error {
//	    if err := c.Query(ctx, codexsdk.Text("Hello Codex")); err != nil {
//	        return err
//	    }
//	    for msg, err := range c.ReceiveResponse(ctx) {
//	        if err != nil {
//	            return err
//	        }
//	        // process message...
//	    }
//	    return nil
//	},
//	    codexsdk.WithLogger(slog.Default()),
//	    codexsdk.WithPermissionMode("acceptEdits"),
//	)
//
//	// Or using NewClient directly for more control
//	client := codexsdk.NewClient()
//	defer func() {
//	    if err := client.Close(); err != nil {
//	        log.Printf("failed to close client: %v", err)
//	    }
//	}()
//
//	err := client.Start(ctx,
//	    codexsdk.WithLogger(slog.Default()),
//	    codexsdk.WithPermissionMode("acceptEdits"),
//	)
//
// # Streaming Deltas
//
// By default, only completed AssistantMessage and ResultMessage are emitted.
// To receive token-by-token streaming deltas as StreamEvent messages, enable
// WithIncludePartialMessages:
//
//	for msg, err := range codexsdk.Query(ctx, codexsdk.Text("Hello"),
//	    codexsdk.WithIncludePartialMessages(true),
//	) {
//	    if err != nil {
//	        log.Fatal(err)
//	    }
//	    if se, ok := msg.(*codexsdk.StreamEvent); ok {
//	        // se.Event contains content_block_delta data. The nested
//	        // delta.type identifies the chunk source: text_delta for
//	        // assistant prose, thinking_delta for reasoning,
//	        // command_output_delta for shell stdout/stderr, and
//	        // file_change_delta for diff output. command_output_delta
//	        // and file_change_delta carry an item_id that correlates
//	        // back to the corresponding ToolUseBlock.
//	    }
//	}
//
// # Logging
//
// For detailed operation tracking, use WithLogger:
//
//	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
//	messages, err := codexsdk.Query(ctx, codexsdk.Text("Hello Codex"),
//	    codexsdk.WithLogger(logger),
//	)
//
// # Error Handling
//
// The SDK provides typed errors for different failure scenarios:
//
//	messages, err := codexsdk.Query(ctx, codexsdk.Text(prompt), codexsdk.WithPermissionMode("acceptEdits"))
//	if err != nil {
//	    if cliErr, ok := errors.AsType[*codexsdk.CLINotFoundError](err); ok {
//	        log.Fatalf("Codex CLI not installed, searched: %v", cliErr.SearchedPaths)
//	    }
//	    if procErr, ok := errors.AsType[*codexsdk.ProcessError](err); ok {
//	        log.Fatalf("CLI process failed with exit code %d: %s", procErr.ExitCode, procErr.Stderr)
//	    }
//	    log.Fatal(err)
//	}
//
// # SDK Tools
//
// Register custom tools that the agent can call back into your Go code using
// NewTool and WithSDKTools. Tools are sent as dynamicTools and dispatched via
// the item/tool/call RPC:
//
//	add := codexsdk.NewTool("add", "Add two numbers",
//	    map[string]any{
//	        "type": "object",
//	        "properties": map[string]any{
//	            "a": map[string]any{"type": "number"},
//	            "b": map[string]any{"type": "number"},
//	        },
//	        "required": []string{"a", "b"},
//	    },
//	    func(_ context.Context, input map[string]any) (map[string]any, error) {
//	        a, _ := input["a"].(float64)
//	        b, _ := input["b"].(float64)
//	        return map[string]any{"result": a + b}, nil
//	    },
//	)
//
//	for msg, err := range codexsdk.Query(ctx, codexsdk.Text("Add 5 and 3"),
//	    codexsdk.WithSDKTools(add),
//	    codexsdk.WithPermissionMode("bypassPermissions"),
//	) {
//	    // ...
//	}
//
// # Personality, Service Tier, and Developer Instructions
//
// Control the agent's response style with WithPersonality:
//
//	codexsdk.WithPersonality("pragmatic") // "none", "friendly", "pragmatic"
//
// Select the API service tier with WithServiceTier:
//
//	codexsdk.WithServiceTier("fast") // "fast" or "flex"
//
// Provide additional agent instructions with WithDeveloperInstructions,
// which maps to the Codex CLI's developerInstructions field and is
// separate from WithSystemPrompt:
//
//	codexsdk.WithDeveloperInstructions("Always respond in three bullet points.")
//
// Effort levels now include EffortNone and EffortMinimal in addition to
// EffortLow, EffortMedium, EffortHigh, and EffortMax:
//
//	codexsdk.WithEffort(codexsdk.EffortMinimal)
//
// # Plan Mode and User Input Callbacks
//
// When using plan mode, the agent can ask the user questions via
// request_user_input. Register a callback with WithOnUserInput to handle
// these requests programmatically:
//
//	callback := func(
//	    _ context.Context,
//	    req *codexsdk.UserInputRequest,
//	) (*codexsdk.UserInputResponse, error) {
//	    answers := make(map[string]*codexsdk.UserInputAnswer, len(req.Questions))
//	    for _, q := range req.Questions {
//	        if len(q.Options) > 0 {
//	            // Auto-select first option.
//	            answers[q.ID] = &codexsdk.UserInputAnswer{
//	                Answers: []string{q.Options[0].Label},
//	            }
//	        } else {
//	            answers[q.ID] = &codexsdk.UserInputAnswer{
//	                Answers: []string{"my answer"},
//	            }
//	        }
//	    }
//	    return &codexsdk.UserInputResponse{Answers: answers}, nil
//	}
//
//	client := codexsdk.NewClient()
//	err := client.Start(ctx,
//	    codexsdk.WithPermissionMode("plan"),
//	    codexsdk.WithOnUserInput(callback),
//	    codexsdk.WithCanUseTool(myPermissionCallback),
//	)
//
// WithOnUserInput requires the app-server backend and is typically paired with
// WithPermissionMode("plan") and WithCanUseTool for full control over agent
// interactions.
//
// # Session Metadata
//
// Read metadata about a local Codex session using StatSession:
//
//	stat, err := codexsdk.StatSession(ctx, "550e8400-e29b-41d4-a716-446655440000",
//	    codexsdk.WithCodexHome("/custom/.codex"),
//	)
//	if err != nil {
//	    log.Fatal(err)
//	}
//	fmt.Printf("Session: %s (tokens: %d)\n", stat.Title, stat.TokensUsed)
//
// List and inspect local sessions without a running CLI instance:
//
//	sessions, err := codexsdk.ListSessions(ctx, codexsdk.WithCwd("/repo"))
//	if err != nil {
//	    log.Fatal(err)
//	}
//	msgs, err := codexsdk.GetSessionMessages(ctx, sessions[0].SessionID)
//	if err != nil {
//	    log.Fatal(err)
//	}
//	fmt.Printf("Loaded %d persisted messages\n", len(msgs))
//
// StatSession, ListSessions, and GetSessionMessages read from the Codex CLI's
// local SQLite database and rollout files and do not require a running CLI
// instance. Persisted rollout lifecycle records are surfaced as typed system
// messages such as TaskStartedMessage and TaskCompleteMessage.
//
// # Requirements
//
// This SDK requires the Codex CLI to be installed and available in your system PATH.
// You can specify a custom CLI path using the WithCliPath option.
package codexsdk
