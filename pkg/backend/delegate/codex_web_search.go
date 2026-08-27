package delegate

import (
	"context"
	"encoding/json"
	"fmt"
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

var codexVersionPattern = regexp.MustCompile(`\d+\.\d+\.\d+`)

// codexWebSearchRequested is deliberately keyed on the DSL's canonical tool
// name. Codex's other native tools remain ambient because its SDK cannot
// narrow them, but hosted Web search has a real config switch and therefore
// can honour the DSL contract exactly.
func codexWebSearchRequested(task Task) bool {
	for _, name := range task.AllowedTools {
		if strings.EqualFold(strings.TrimSpace(name), "web_search") {
			return true
		}
	}
	return false
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

// validateCodexWebSearchCapability fails before the work process starts when
// the installed CLI cannot provide the app-server + webSearch item contract
// used by the pinned SDK. The SDK's ordinary discovery only warns on an old
// version, which is unsafe for a requested capability: the run could otherwise
// continue without Web access.
func validateCodexWebSearchCapability(ctx context.Context, explicitCommand string) error {
	path, ok := clilocate.Locate(explicitCommand, clilocate.Spec{
		Name:      "codex",
		Fallbacks: clilocate.CommonBinaryCandidates("codex"),
	})
	if !ok {
		return fmt.Errorf(
			"delegate: codex web_search capability unavailable: Codex CLI not found; install Codex CLI >= %s or remove web_search from tools",
			codexsdk.MinimumCLIVersion,
		)
	}

	versionCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(versionCtx, path, "--version").CombinedOutput()
	if err != nil {
		return fmt.Errorf(
			"delegate: codex web_search capability unavailable: cannot verify %s --version: %w; web_search requires Codex CLI >= %s",
			path,
			err,
			codexsdk.MinimumCLIVersion,
		)
	}

	version := codexVersionPattern.FindString(string(out))
	if version == "" {
		return fmt.Errorf(
			"delegate: codex web_search capability unavailable: cannot parse Codex CLI version from %q; web_search requires >= %s",
			strings.TrimSpace(string(out)),
			codexsdk.MinimumCLIVersion,
		)
	}
	if compareCodexVersions(version, codexsdk.MinimumCLIVersion) < 0 {
		return fmt.Errorf(
			"delegate: codex web_search capability unavailable: Codex CLI %s is too old; web_search requires >= %s (upgrade Codex CLI or remove web_search from tools)",
			version,
			codexsdk.MinimumCLIVersion,
		)
	}
	return nil
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
			if eventType != "item.completed" {
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
		hooks.OnToolCalled(name, id, false, codexCompletedToolOutput(msg.Audit))
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
