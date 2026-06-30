package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/SocialGouv/claw-code-go/pkg/api/hooks"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// settingsHookEvents are the .claude/settings.json hook events the claw backend
// honours, mapped to the claw hooks.Event they fire on. This is the claw half
// of the plugin `hooks` kind: iterion already merges plugin hooks into
// <workspace>/.claude/settings.json (claude_code reads them via
// --setting-sources project); here the claw backend reads the SAME file and
// runs the command-type entries through claw's hook Runner, so a hooks plugin
// behaves identically on either backend.
//
// Only `command`-type hooks are bridged — claw runs them as shell commands and
// maps exit 2 to a Block. `prompt`-type hooks (LLM-evaluated) are claude_code's
// native surface and are skipped on claw.
var settingsHookEvents = map[string]hooks.Event{
	"PreToolUse":         hooks.PreToolUse,
	"PostToolUse":        hooks.PostToolUse,
	"PostToolUseFailure": hooks.PostToolUseFailure,
	"Stop":               hooks.Stop,
}

// settingsHookCommandTimeout bounds a single command-hook invocation when the
// entry sets no timeout.
const settingsHookCommandTimeout = 30 * time.Second

type settingsHookEntry struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout"` // seconds; 0 → default
}

type settingsHookGroup struct {
	Matcher string              `json:"matcher"` // regex on ToolName; "" = all
	Hooks   []settingsHookEntry `json:"hooks"`
}

// registerSettingsHooks reads <workDir>/.claude/settings.json and registers a
// claw hook Handler per event for its command-type entries. Returns the number
// of events wired. Best-effort: a missing/invalid settings file wires nothing.
func registerSettingsHooks(r *hooks.Runner, workDir string, logger *iterlog.Logger) int {
	if r == nil || workDir == "" {
		return 0
	}
	data, err := os.ReadFile(filepath.Join(workDir, ".claude", "settings.json"))
	if err != nil {
		return 0
	}
	var doc struct {
		Hooks map[string][]settingsHookGroup `json:"hooks"`
	}
	if jerr := json.Unmarshal(data, &doc); jerr != nil {
		if logger != nil {
			logger.Warn("model: parse .claude/settings.json hooks: %v — skipping", jerr)
		}
		return 0
	}
	wired := 0
	for name, event := range settingsHookEvents {
		groups := doc.Hooks[name]
		if len(groups) == 0 {
			continue
		}
		r.Register(event, settingsHookHandler(name, groups, logger))
		wired++
	}
	return wired
}

// settingsHookHandler builds a claw Handler that runs every command-type entry
// whose group matcher matches the tool name. A clean exit continues; exit 2
// blocks (claude_code's "explicit denial" convention); any other failure is
// logged and continues (a compressor/observer hook must not wedge a node).
func settingsHookHandler(event string, groups []settingsHookGroup, logger *iterlog.Logger) hooks.Handler {
	return func(ctx context.Context, hctx hooks.Context) (hooks.Decision, error) {
		for _, g := range groups {
			if !matcherMatches(g.Matcher, hctx.ToolName) {
				continue
			}
			for _, h := range g.Hooks {
				if h.Type != "command" || h.Command == "" {
					continue
				}
				blocked, reason := runCommandHook(ctx, event, hctx, h)
				if blocked {
					return hooks.Decision{Action: hooks.ActionBlock, Reason: reason}, nil
				}
			}
		}
		return hooks.Decision{Action: hooks.ActionContinue}, nil
	}
}

// matcherMatches reports whether a group matcher (a regex; "" = match all)
// matches the tool name. An invalid regex matches nothing (fail-open to
// Continue, never spuriously block).
func matcherMatches(matcher, toolName string) bool {
	if matcher == "" {
		return true
	}
	re, err := regexp.Compile(matcher)
	if err != nil {
		return false
	}
	return re.MatchString(toolName)
}

// runCommandHook runs one command-type hook entry via `sh -c`, passing the
// claude_code-compatible HOOK_* env. Returns (blocked, reason): blocked is true
// only on exit code 2.
func runCommandHook(ctx context.Context, event string, hctx hooks.Context, h settingsHookEntry) (bool, string) {
	timeout := settingsHookCommandTimeout
	if h.Timeout > 0 {
		timeout = time.Duration(h.Timeout) * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	inputJSON, _ := json.Marshal(hctx.ToolInput)
	c := exec.CommandContext(cctx, "sh", "-c", h.Command)
	c.Env = append(os.Environ(),
		"HOOK_EVENT="+event,
		"HOOK_TOOL_NAME="+hctx.ToolName,
		"HOOK_TOOL_INPUT="+string(inputJSON),
	)
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr
	err := c.Run()
	if cctx.Err() != nil {
		return false, "" // timeout → don't block
	}
	if exitCodeOf(err) == 2 {
		reason := firstNonEmpty(stderr.String(), stdout.String())
		if reason == "" {
			reason = fmt.Sprintf("settings hook blocked %s on %s", event, hctx.ToolName)
		}
		return true, reason
	}
	return false, ""
}

func exitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}
