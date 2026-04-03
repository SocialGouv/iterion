// Package claude provides a Go SDK for the Claude Code CLI.
//
// The SDK spawns the Claude Code CLI as a subprocess and communicates via
// NDJSON over stdin/stdout. It supports one-shot prompts, multi-turn sessions,
// streaming responses, hooks, MCP servers, and subagents.
//
// # One-shot prompt
//
//	result, err := claude.Prompt(ctx, "What is 2+2?", claude.WithModel("claude-sonnet-4-6"))
//	if err != nil {
//	    log.Fatal(err)
//	}
//	if result.Subtype == claude.ResultSuccess {
//	    fmt.Println(*result.Result)
//	}
//
// # Multi-turn session
//
//	session := claude.NewSession(claude.WithModel("claude-sonnet-4-6"))
//	defer session.Close()
//
//	session.Send(ctx, "What is 5 + 3?")
//	for msg, err := range session.Stream(ctx) {
//	    if err != nil {
//	        log.Fatal(err)
//	    }
//	    // handle msg via type switch
//	}
//
// # Resume session
//
//	session := claude.ResumeSession(sessionID, claude.WithModel("claude-sonnet-4-6"))
//	defer session.Close()
package claude
