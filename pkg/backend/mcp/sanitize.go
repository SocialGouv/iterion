package mcp

// SanitizationRule defines a tool argument fixup applied before MCP tool calls.
// Rules are applied in order; each matching rule modifies args in-place.
type SanitizationRule struct {
	// Name identifies this rule for logging/debugging.
	Name string
	// Match returns true if this rule applies to the given tool.
	Match func(toolName string) bool
	// Apply modifies args in-place.
	Apply func(toolName string, args map[string]interface{}, workDir string)
}

// DefaultSanitizationRules returns the built-in sanitization rules that fix
// common mistakes made by LLMs when calling MCP tools.
func DefaultSanitizationRules() []SanitizationRule {
	return []SanitizationRule{
		{
			// LLMs often send optional fields as empty strings (e.g. pages: "")
			// which MCP servers reject.
			Name:  "remove-empty-strings",
			Match: func(_ string) bool { return true },
			Apply: func(_ string, args map[string]interface{}, _ string) {
				for key, val := range args {
					if s, ok := val.(string); ok && s == "" {
						delete(args, key)
					}
				}
			},
		},
		{
			// For the codex tool: force workspace and non-interactive settings.
			// These are always overridden (not just defaulted) because the LLM
			// may send incorrect values (wrong cwd, restrictive sandbox, etc.).
			Name:  "codex-workspace",
			Match: func(name string) bool { return name == "codex" },
			Apply: func(_ string, args map[string]interface{}, workDir string) {
				if workDir != "" {
					args["cwd"] = workDir
				}
				args["approval-policy"] = "never"
				args["sandbox"] = "danger-full-access"
			},
		},
		{
			// For Read tool: always ensure a limit is set to avoid "file too
			// large" errors. The Claude Code MCP server rejects reads over 10K
			// tokens. The auto-retry in the tool callback handles cases where
			// even the capped limit is too large.
			Name:  "read-limit-cap",
			Match: func(name string) bool { return name == "Read" },
			Apply: func(_ string, args map[string]interface{}, _ string) {
				if _, hasPages := args["pages"]; hasPages {
					return
				}
				limit, hasLimit := args["limit"]
				if !hasLimit {
					args["limit"] = float64(500)
				} else if limitF, ok := limit.(float64); ok && limitF > 500 {
					args["limit"] = float64(500)
				}
			},
		},
	}
}
