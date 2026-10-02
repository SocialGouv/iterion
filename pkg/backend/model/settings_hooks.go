package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/SocialGouv/claw-code-go/pkg/api/hooks"
	"github.com/SocialGouv/iterion/pkg/internal/proc"
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
// Every event claw-code-go exports is mapped: the tool-use trio and Stop fire
// from the generation loop's tool/exec boundaries, UserPromptSubmit fires once
// per generation call before the first model request, and PreCompact /
// PostCompact bracket the threshold-triggered in-loop compaction
// (compactBetweenIterations). The FORCED compaction of callWithContextRetry —
// recovery from a context-window rejection — does not fire them: a PreCompact
// Block there could only turn a recoverable overflow into a failed node, and a
// hook must not be able to disable the run's last recovery.
//
// Only `command`-type hooks are bridged — claw runs them as shell commands and
// maps exit 2 to a Block. `prompt`-type hooks (LLM-evaluated) are claude_code's
// native surface and are skipped on claw, with a diagnostic (see
// diagnoseSettingsHooks): skipped in silence is what the parity addendum
// forbids.
var settingsHookEvents = map[string]hooks.Event{
	"PreToolUse":         hooks.PreToolUse,
	"PostToolUse":        hooks.PostToolUse,
	"PostToolUseFailure": hooks.PostToolUseFailure,
	"UserPromptSubmit":   hooks.UserPromptSubmit,
	"PreCompact":         hooks.PreCompact,
	"PostCompact":        hooks.PostCompact,
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

// readSettingsHooksDoc reads <workDir>/.claude/settings.json and returns the
// raw bytes of its top-level "hooks" object, or nil when the file is absent
// or declares none. An unreadable or invalid file is an error — the caller
// warns, because a hooks file that cannot be parsed is a plugin firing
// nothing the operator believes is armed.
func readSettingsHooksDoc(workDir string) (json.RawMessage, error) {
	if workDir == "" {
		return nil, nil
	}
	data, err := os.ReadFile(filepath.Join(workDir, ".claude", "settings.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var doc struct {
		Hooks json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if len(doc.Hooks) == 0 || string(doc.Hooks) == "null" {
		return nil, nil
	}
	return doc.Hooks, nil
}

// registerSettingsHooks reads <workDir>/.claude/settings.json and registers a
// claw hook Handler per event for its command-type entries. Returns the number
// of events wired. Best-effort: a missing settings file wires nothing.
func registerSettingsHooks(r *hooks.Runner, workDir string, logger *iterlog.Logger) int {
	if r == nil || workDir == "" {
		return 0
	}
	doc, err := readSettingsHooksDoc(workDir)
	if err != nil {
		if logger != nil {
			logger.Warn("model: parse .claude/settings.json hooks: %v — skipping", err)
		}
		return 0
	}
	return registerSettingsHooksJSON(r, doc, logger)
}

// registerSettingsHooksJSON is registerSettingsHooks with the hooks document
// already at hand — the form the sandboxed path uses, where the launcher read
// the host's settings.json and shipped the document over the wire (an
// in-container re-read would miss whenever the workspace is not mounted at
// its host path). Emits a diagnostic for every entry claw cannot bridge
// rather than dropping it in silence.
func registerSettingsHooksJSON(r *hooks.Runner, doc json.RawMessage, logger *iterlog.Logger) int {
	if r == nil || len(doc) == 0 {
		return 0
	}
	var hooksDoc map[string][]settingsHookGroup
	if err := json.Unmarshal(doc, &hooksDoc); err != nil {
		if logger != nil {
			logger.Warn("model: parse .claude/settings.json hooks: %v — skipping", err)
		}
		return 0
	}
	diagnoseSettingsHooks(hooksDoc, logger)
	wired := 0
	for name, event := range settingsHookEvents {
		groups := hooksDoc[name]
		if len(groups) == 0 {
			continue
		}
		r.Register(event, settingsHookHandler(name, groups, logger))
		wired++
	}
	return wired
}

// diagnoseSettingsHooks warns about everything in the hooks document claw
// does not bridge: events claw-code-go does not export (SessionStart and the
// other claude_code-only names) and non-`command` entries of mapped events.
// Without this a plugin whose UserPromptSubmit / prompt-type hook works on
// claude_code is a silent no-op on claw — the failure shape the parity
// addendum forbids. Sorted so the output is stable run over run.
func diagnoseSettingsHooks(doc map[string][]settingsHookGroup, logger *iterlog.Logger) {
	if logger == nil {
		return
	}
	names := make([]string, 0, len(doc))
	for name := range doc {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		groups := doc[name]
		if _, mapped := settingsHookEvents[name]; !mapped {
			logger.Warn("model: .claude/settings.json declares a %q hook, an event claw does not export — it fires on claude_code but is not bridged on backend: claw", name)
			continue
		}
		for _, g := range groups {
			if msg := matcherDiagnostic(name, g.Matcher); msg != "" {
				logger.Warn("model: .claude/settings.json %s hook: %s", name, msg)
			}
			for _, h := range g.Hooks {
				switch {
				case h.Type != "command":
					logger.Warn("model: .claude/settings.json %s hook entry of type %q skipped — claw bridges command-type hooks only", name, h.Type)
				case h.Command == "":
					logger.Warn("model: .claude/settings.json %s command hook entry has an empty command — skipped", name)
				}
			}
		}
	}
}

// matcherDiagnostic names the matcher configurations that cannot do what
// their author expects on claw — the bridge's share of "a hook that never
// fires must never be silent". "" when the matcher is meaningful as written.
// Only called for events settingsHookEvents maps.
func matcherDiagnostic(event, matcher string) string {
	if matcher == "" {
		return ""
	}
	switch event {
	case "UserPromptSubmit", "Stop":
		// claude_code defines no matcher for these events; the bridge
		// ignores it too — but it does not READ as ignored in the file.
		return fmt.Sprintf("matcher %q is ignored — claude_code defines no matcher for this event; the hook always fires", matcher)
	case "PreCompact", "PostCompact":
		// The matcher is the compaction SOURCE on claude_code, and claw
		// compacts only at the auto threshold.
		switch matcher {
		case "auto":
			return ""
		case "manual":
			return `matcher "manual" never fires on claw — it compacts only at the auto threshold`
		default:
			return fmt.Sprintf("matcher %q is not a compaction source (manual|auto) — the hook never fires", matcher)
		}
	default:
		if _, err := regexp.Compile(matcher); err != nil {
			return fmt.Sprintf("matcher %q is not a valid regex (%v) — the hook never fires", matcher, err)
		}
		return ""
	}
}

// settingsHookHandler builds a claw Handler that runs every command-type
// entry admitted by the group matcher — the matcher's semantics depend on
// the event (see eventMatcherMatches). A clean exit continues; exit 2 blocks
// (claude_code's "explicit denial" convention); any other failure — a
// non-zero exit, a timeout, an exec error — is logged and continues (a
// compressor/observer hook must not wedge a node, but it must not fail in
// silence either). Non-command entries were already diagnosed at
// registration; they are skipped here without a second warning per fire.
func settingsHookHandler(event string, groups []settingsHookGroup, logger *iterlog.Logger) hooks.Handler {
	return func(ctx context.Context, hctx hooks.Context) (hooks.Decision, error) {
		for _, g := range groups {
			if !eventMatcherMatches(event, g.Matcher, hctx.ToolName) {
				continue
			}
			for _, h := range g.Hooks {
				if h.Type != "command" || h.Command == "" {
					continue
				}
				blocked, reason := runCommandHook(ctx, event, hctx, h, logger)
				if blocked {
					return hooks.Decision{Action: hooks.ActionBlock, Reason: reason}, nil
				}
			}
		}
		return hooks.Decision{Action: hooks.ActionContinue}, nil
	}
}

// eventMatcherMatches applies the group matcher's semantics FOR THE EVENT:
// claude_code defines matchers only where the event carries something to
// match against. Tool events match the regex on the tool name;
// UserPromptSubmit and Stop define no matcher at all — the hook ALWAYS
// fires, and a matcher set there is ignored (diagnosed at registration);
// PreCompact/PostCompact match the compaction SOURCE (manual|auto), and
// claw compacts only at the auto threshold — a "manual" matcher, or any
// other value, never matches. Without this split every non-tool event
// evaluated its matcher against an empty tool name, silently killing the
// documented `"matcher": "auto"` PreCompact configuration.
func eventMatcherMatches(event, matcher, toolName string) bool {
	switch event {
	case "UserPromptSubmit", "Stop":
		return true
	case "PreCompact", "PostCompact":
		return matcher == "" || matcher == "auto"
	default:
		return matcherMatches(matcher, toolName)
	}
}

// matcherMatches reports whether a group matcher (a regex; "" = match all)
// matches the tool name. An invalid regex matches nothing (fail-open to
// Continue, never spuriously block; diagnosed at registration).
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

// hookEnvMaxBytes caps one HOOK_* environment value below the kernel's
// per-argument limit (MAX_ARG_STRLEN, 128 KiB on Linux): a larger value
// fails the whole exec with E2BIG — which on a SCREENING hook
// (UserPromptSubmit) is a silent fail-open, the prompt reaching the model
// unscreened. The marker tells the hook it received a prefix; the cut may
// split a UTF-8 sequence, which an env payload survives.
const hookEnvMaxBytes = 100 * 1024

// truncateHookEnvValue caps s at hookEnvMaxBytes with an explicit marker.
func truncateHookEnvValue(s string) string {
	if len(s) <= hookEnvMaxBytes {
		return s
	}
	return fmt.Sprintf("%s\n…[hook payload truncated: %d of %d bytes shown]", s[:hookEnvMaxBytes], hookEnvMaxBytes, len(s))
}

// runCommandHook runs one command-type hook entry via `sh -c`, passing the
// claude_code-compatible HOOK_* env. Returns (blocked, reason): blocked is true
// only on exit code 2. Every other failure — non-zero exit, timeout, an exec
// error such as E2BIG — is logged and continues: a hook must not wedge a
// node, and must not fail in silence either.
func runCommandHook(ctx context.Context, event string, hctx hooks.Context, h settingsHookEntry, logger *iterlog.Logger) (bool, string) {
	timeout := settingsHookCommandTimeout
	if h.Timeout > 0 {
		timeout = time.Duration(h.Timeout) * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	inputJSON, _ := json.Marshal(hctx.ToolInput)
	c := exec.CommandContext(cctx, "sh", "-c", h.Command)
	// A hook's timeout has to end the hook's WORK, not only iterion's wait:
	// a `sh -c` that backgrounds a job leaves it holding the buffers below,
	// so c.Run would block past the timeout on a tool call the gate has
	// already decided about.
	proc.TerminateGroupOnCancel(c)
	env := append(os.Environ(),
		"HOOK_EVENT="+event,
		"HOOK_TOOL_NAME="+hctx.ToolName,
		"HOOK_TOOL_INPUT="+truncateHookEnvValue(string(inputJSON)),
	)
	// Non-tool events carry their payload under their own variables: the
	// submitted prompt for UserPromptSubmit, the history size for the
	// compaction pair.
	if hctx.UserPrompt != "" {
		env = append(env, "HOOK_USER_PROMPT="+truncateHookEnvValue(hctx.UserPrompt))
	}
	if hctx.MessageCount > 0 {
		env = append(env, fmt.Sprintf("HOOK_MESSAGE_COUNT=%d", hctx.MessageCount))
	}
	c.Env = env
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr
	err := c.Run()
	if cctx.Err() != nil {
		// Warn only for the hook's OWN timeout: when the parent ctx is
		// done the run itself was cancelled, and a "hook timed out" line
		// would misname the operator's interruption as a hook failure.
		if ctx.Err() == nil && logger != nil {
			logger.Warn("model: settings hook on %s timed out after %s — continuing", event, timeout)
		}
		return false, "" // timeout / cancellation → don't block
	}
	if exitCodeOf(err) == 2 {
		reason := firstNonEmpty(stderr.String(), stdout.String())
		if reason == "" {
			reason = fmt.Sprintf("settings hook blocked %s on %s", event, hctx.ToolName)
		}
		return true, reason
	}
	if err != nil && logger != nil {
		logger.Warn("model: settings hook on %s failed (%v): %s — continuing",
			event, err, truncateLogLine(firstNonEmpty(stderr.String(), stdout.String())))
	}
	return false, ""
}

// truncateLogLine bounds a hook's captured output in a log line.
func truncateLogLine(s string) string {
	const max = 200
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
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
