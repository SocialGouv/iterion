package delegate

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	codexsdk "github.com/ethpandaops/codex-agent-sdk-go"

	"github.com/SocialGouv/iterion/pkg/internal/clilocate"
)

const codexWebSearchModeLive = "live"
const codexWebSearchModeDisabled = "disabled"

// codexWebSearchMinCLIVersion is the oldest supported CLI release whose
// ConfigToml accepts the top-level web_search mode enum. Keep this capability
// floor independent from the SDK's general minimum even while they coincide.
const codexWebSearchMinCLIVersion = "0.103.0"
const codexWebSearchVersionTimeout = 5 * time.Second

var codexVersionPattern = regexp.MustCompile(`\d+\.\d+\.\d+`)

// codexWebSearchRequested is deliberately keyed on the DSL's canonical tool
// name. Codex's other native tools remain ambient because its SDK cannot
// narrow them, but hosted Web search has a real config switch and therefore
// can honour the DSL contract exactly.
func codexWebSearchRequested(task Task) bool {
	for _, name := range task.AllowedTools {
		if isCodexWebSearchTool(name) {
			return true
		}
	}
	return false
}

func isCodexWebSearchTool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "web_search", "websearch", "web-search":
		return true
	default:
		return false
	}
}

func codexWebSearchMode(task Task) string {
	if codexWebSearchRequested(task) {
		return codexWebSearchModeLive
	}
	return codexWebSearchModeDisabled
}

func codexWebSearchOption(task Task) codexsdk.Option {
	return codexsdk.WithConfig(map[string]string{
		"web_search": codexWebSearchMode(task),
	})
}

// validateCodexWebSearchCapability resolves and validates the CLI before the
// work process starts. Every Codex task emits the top-level web_search mode, so
// both live and disabled paths need the ConfigToml enum introduced by this
// capability floor. The returned path must be passed to WithCliPath so the SDK
// executes the same installation that was probed.
func validateCodexWebSearchCapability(ctx context.Context, explicitCommand string) (string, error) {
	path, ok := clilocate.Locate(explicitCommand, clilocate.Spec{
		Name:      "codex",
		Fallbacks: clilocate.CommonBinaryCandidates("codex"),
	})
	if !ok {
		return "", fmt.Errorf(
			"delegate: codex web_search mode unavailable: Codex CLI not found; install Codex CLI >= %s",
			codexWebSearchMinCLIVersion,
		)
	}
	// Match the SDK's documented operator escape hatch. The resolved path is
	// still returned and passed to WithCliPath so discovery cannot choose a
	// different installation from the one this function selected.
	if os.Getenv("CODEX_CLI_SKIP_VERSION_CHECK") != "" {
		return path, nil
	}

	// This check is a hard capability gate rather than the SDK's best-effort
	// warning, so allow extra headroom for npm shims and cold filesystems.
	versionCtx, cancel := context.WithTimeout(ctx, codexWebSearchVersionTimeout)
	defer cancel()
	out, err := exec.CommandContext(versionCtx, path, "--version").Output()
	if err != nil {
		return "", fmt.Errorf(
			"delegate: codex web_search mode unavailable: cannot verify %s --version: %w; web_search requires Codex CLI >= %s",
			path,
			err,
			codexWebSearchMinCLIVersion,
		)
	}

	version := codexVersionPattern.FindString(string(out))
	if version == "" {
		return "", fmt.Errorf(
			"delegate: codex web_search mode unavailable: cannot parse Codex CLI version from %q; web_search requires >= %s",
			strings.TrimSpace(string(out)),
			codexWebSearchMinCLIVersion,
		)
	}
	if compareCodexVersions(version, codexWebSearchMinCLIVersion) < 0 {
		return "", fmt.Errorf(
			"delegate: codex web_search mode unavailable: Codex CLI %s is too old; web_search requires >= %s (upgrade Codex CLI)",
			version,
			codexWebSearchMinCLIVersion,
		)
	}
	return path, nil
}

func compareCodexVersions(a, b string) int {
	aParts := strings.Split(a, ".")
	bParts := strings.Split(b, ".")
	for i := range 3 {
		var av, bv int
		if i < len(aParts) {
			av, _ = strconv.Atoi(aParts[i])
		}
		if i < len(bParts) {
			bv, _ = strconv.Atoi(bParts[i])
		}
		if av < bv {
			return -1
		}
		if av > bv {
			return 1
		}
	}
	return 0
}

// emitCodexToolHooks converts Codex app-server item lifecycle messages into
// Iterion's backend-neutral tool_started/tool_called hooks. The SDK emits a
// ToolUseBlock again on item.completed; it is a completion, not a second call.
// Some hosted tools (notably WebSearch) have no ToolResultBlock, so their
// completed audit payload is the best available result/URL detail.
func emitCodexToolHooks(hooks TaskHooks, msg *codexsdk.AssistantMessage, inFlight map[string]string) {
	if msg == nil || (hooks.OnToolStarted == nil && hooks.OnToolCalled == nil) {
		return
	}
	eventType := ""
	if msg.Audit != nil {
		eventType = msg.Audit.EventType
	}

	uses := make(map[string]*codexsdk.ToolUseBlock)
	completed := make(map[string]bool)
	for _, block := range msg.Content {
		switch typed := block.(type) {
		case *codexsdk.ToolUseBlock:
			uses[typed.ID] = typed
			if (eventType == "" || eventType == "item.started") && inFlight[typed.ID] == "" {
				inFlight[typed.ID] = typed.Name
				if hooks.OnToolStarted != nil {
					hooks.OnToolStarted(typed.Name, typed.ID, marshalCodexToolInput(typed.Input))
				}
			}
		case *codexsdk.ToolResultBlock:
			name := inFlight[typed.ToolUseID]
			if use := uses[typed.ToolUseID]; name == "" && use != nil {
				name = use.Name
			}
			delete(inFlight, typed.ToolUseID)
			completed[typed.ToolUseID] = true
			if hooks.OnToolCalled != nil {
				hooks.OnToolCalled(name, typed.ToolUseID, typed.IsError, codexToolResultText(typed.Content))
			}
		}
	}

	if eventType != "item.completed" || hooks.OnToolCalled == nil {
		return
	}
	for id, use := range uses {
		if completed[id] {
			continue
		}
		name := inFlight[id]
		if name == "" {
			name = use.Name
		}
		delete(inFlight, id)
		hooks.OnToolCalled(name, id, codexCompletedToolErrored(msg.Audit), codexCompletedToolOutput(msg.Audit))
	}
}

func marshalCodexToolInput(input map[string]any) json.RawMessage {
	if len(input) == 0 {
		return nil
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return nil
	}
	return raw
}

func codexToolResultText(blocks []codexsdk.ContentBlock) string {
	if len(blocks) == 0 {
		return ""
	}
	var parts []string
	for _, block := range blocks {
		switch typed := block.(type) {
		case *codexsdk.TextBlock:
			parts = append(parts, typed.Text)
		default:
			parts = append(parts, "<"+block.BlockType()+">")
		}
	}
	return strings.Join(parts, "\n")
}

func codexCompletedToolOutput(audit *codexsdk.AuditEnvelope) string {
	if audit == nil || len(audit.Payload) == 0 {
		return ""
	}
	return string(audit.Payload)
}

// codexCompletedToolErrored recovers completion state for item types that the
// SDK currently represents with a ToolUseBlock only (notably file changes and
// WebSearch). App-server audit payloads preserve the original JSON-RPC
// envelope, while exec/custom transports may expose the item at top level, so
// accept both shapes.
func codexCompletedToolErrored(audit *codexsdk.AuditEnvelope) bool {
	if audit == nil || len(audit.Payload) == 0 {
		return false
	}
	var payload map[string]any
	if err := json.Unmarshal(audit.Payload, &payload); err != nil {
		return false
	}
	item, _ := payload["item"].(map[string]any)
	if item == nil {
		if params, ok := payload["params"].(map[string]any); ok {
			item, _ = params["item"].(map[string]any)
		}
	}
	if item == nil {
		return false
	}
	if success, ok := item["success"].(bool); ok {
		return !success
	}
	status, _ := item["status"].(string)
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "failed", "error", "cancelled", "canceled", "rejected":
		return true
	}
	return nonEmptyCodexError(item["error"])
}

func nonEmptyCodexError(value any) bool {
	if value == nil {
		return false
	}
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed) != ""
	case map[string]any:
		return len(typed) > 0
	case []any:
		return len(typed) > 0
	default:
		return true
	}
}
