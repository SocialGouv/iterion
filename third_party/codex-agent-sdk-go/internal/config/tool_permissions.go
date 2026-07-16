package config

import (
	"context"
	"fmt"
	"strings"

	"github.com/ethpandaops/codex-agent-sdk-go/internal/permission"
)

func normalizeToolName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

func equivalentToolNames(name string) []string {
	normalized := normalizeToolName(name)
	switch normalized {
	case "", "permissions":
		return []string{normalized}
	case "edit", "write":
		return []string{"edit", "write"}
	default:
		return []string{normalized}
	}
}

func matchesToolSet(set map[string]struct{}, toolName string) bool {
	for _, candidate := range equivalentToolNames(toolName) {
		if _, ok := set[candidate]; ok {
			return true
		}
	}

	return false
}

func toolSet(names []string) map[string]struct{} {
	set := make(map[string]struct{}, len(names))
	for _, name := range names {
		normalized := normalizeToolName(name)
		if normalized == "" {
			continue
		}

		set[normalized] = struct{}{}
	}

	return set
}

// ConfigureToolPermissionPolicy prepares callback/tool-related options for both
// Query and Client flows. It preserves existing callback behavior and adds
// emulation for AllowedTools/DisallowedTools/ToolsList in app-server mode.
func ConfigureToolPermissionPolicy(opts *Options) error {
	if opts == nil {
		return nil
	}

	if opts.CanUseTool != nil && opts.PermissionPromptToolName != "" {
		return fmt.Errorf(
			"can_use_tool callback cannot be used with permission_prompt_tool_name",
		)
	}

	allowedSet := toolSet(opts.AllowedTools)
	disallowedSet := toolSet(opts.DisallowedTools)

	if list, ok := opts.Tools.(ToolsList); ok {
		for _, toolName := range list {
			normalized := normalizeToolName(toolName)
			if normalized == "" {
				continue
			}

			allowedSet[normalized] = struct{}{}
		}
	}

	shouldWrap := len(allowedSet) > 0 || len(disallowedSet) > 0
	upstream := opts.CanUseTool

	if shouldWrap {
		opts.CanUseTool = func(
			ctx context.Context,
			toolName string,
			input map[string]any,
			permCtx *permission.Context,
		) (permission.Result, error) {
			normalized := normalizeToolName(toolName)
			if normalized == "permissions" {
				if upstream != nil {
					return upstream(ctx, toolName, input, permCtx)
				}

				return &permission.ResultAllow{Behavior: "allow"}, nil
			}

			if normalized != "" {
				if matchesToolSet(disallowedSet, toolName) {
					return &permission.ResultDeny{
						Behavior: "deny",
						Message:  fmt.Sprintf("tool %q is disallowed by SDK options", toolName),
					}, nil
				}

				if len(allowedSet) > 0 {
					if !matchesToolSet(allowedSet, toolName) {
						return &permission.ResultDeny{
							Behavior: "deny",
							Message:  fmt.Sprintf("tool %q is not in allowed tools", toolName),
						}, nil
					}
				}
			}

			if upstream != nil {
				return upstream(ctx, toolName, input, permCtx)
			}

			return &permission.ResultAllow{Behavior: "allow"}, nil
		}
	}

	if opts.CanUseTool != nil {
		opts.PermissionPromptToolName = "stdio"
	}

	return nil
}
