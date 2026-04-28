package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/SocialGouv/claw-code-go/internal/api"
	"os/exec"
	"strings"
	"time"
)

func REPLTool() api.Tool {
	return api.Tool{
		Name:        "repl",
		Description: "Execute code in a REPL environment. Supports python, javascript/node, and shell/bash.",
		InputSchema: api.InputSchema{
			Type: "object",
			Properties: map[string]api.Property{
				"code":       {Type: "string", Description: "The code to execute."},
				"language":   {Type: "string", Description: "The language runtime: python, javascript, or shell."},
				"timeout_ms": {Type: "integer", Description: "Optional timeout in milliseconds."},
			},
			Required: []string{"code", "language"},
		},
	}
}

func ExecuteREPL(input map[string]any) (string, error) {
	code, ok := input["code"].(string)
	if !ok || code == "" {
		return "", fmt.Errorf("repl: 'code' is required and must not be empty")
	}
	lang, ok := input["language"].(string)
	if !ok || lang == "" {
		return "", fmt.Errorf("repl: 'language' is required")
	}

	cmdName, args, err := resolveREPLRuntime(lang)
	if err != nil {
		return "", err
	}
	args = append(args, code)

	timeoutMs := int64(30000) // default 30s
	if raw, ok := input["timeout_ms"]; ok {
		if ms, ok := toInt64(raw); ok && ms > 0 {
			timeoutMs = ms
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutMs)*time.Millisecond)
	defer cancel()

	start := time.Now()
	cmd := exec.CommandContext(ctx, cmdName, args...)

	stdout, err := cmd.Output()
	duration := time.Since(start).Milliseconds()

	var stderr string
	var exitCode int
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			stderr = string(exitErr.Stderr)
			exitCode = exitErr.ExitCode()
		} else if ctx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("repl: execution timed out after %d ms", timeoutMs)
		} else {
			return "", fmt.Errorf("repl: failed to execute %s: %v", cmdName, err)
		}
	}

	result := map[string]any{
		"language":    lang,
		"stdout":      string(stdout),
		"stderr":      stderr,
		"exit_code":   exitCode,
		"duration_ms": duration,
	}
	out, _ := json.MarshalIndent(result, "", "  ")
	return string(out), nil
}

func resolveREPLRuntime(lang string) (string, []string, error) {
	switch strings.ToLower(lang) {
	case "python", "py":
		if p, err := exec.LookPath("python3"); err == nil {
			return p, []string{"-c"}, nil
		}
		if p, err := exec.LookPath("python"); err == nil {
			return p, []string{"-c"}, nil
		}
		return "", nil, fmt.Errorf("repl: python3/python not found in PATH")
	case "javascript", "js", "node":
		if p, err := exec.LookPath("node"); err == nil {
			return p, []string{"-e"}, nil
		}
		return "", nil, fmt.Errorf("repl: node not found in PATH")
	case "sh", "shell", "bash":
		if p, err := exec.LookPath("bash"); err == nil {
			return p, []string{"-lc"}, nil
		}
		if p, err := exec.LookPath("sh"); err == nil {
			return p, []string{"-lc"}, nil
		}
		return "", nil, fmt.Errorf("repl: bash/sh not found in PATH")
	default:
		return "", nil, fmt.Errorf("repl: unsupported language %q (supported: python, javascript, shell)", lang)
	}
}
