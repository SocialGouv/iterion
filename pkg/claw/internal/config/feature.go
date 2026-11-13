package config

import "encoding/json"

// RuntimeFeatureConfig holds extracted feature-specific configuration
// from the merged settings. This provides typed access to hook commands,
// plugin settings, permission rules, provider fallback chains, and sandbox.
type RuntimeFeatureConfig struct {
	Hooks             RuntimeHookConfig
	Plugins           RuntimePluginConfig
	Model             string
	Aliases           map[string]string
	PermissionMode    string
	PermissionRules   RuntimePermissionRuleConfig
	ProviderFallbacks ProviderFallbackConfig
	TrustedRoots      []string
	Sandbox           RuntimeSandboxConfig
}

// RuntimePromptConfig holds the system-prompt section toggles from the
// settings "prompt" block, decoded via Settings.Prompt (typed unmarshal,
// merged field-wise across layers — NOT via ExtractFeatureConfig, whose
// RawJSON only carries the last-loaded file). All booleans are tri-state:
// nil means "use the default" (on, unless Minimal is set). Tri-state
// pointers are required because settings merging is non-zero-wins — a
// plain bool could never carry an explicit false across layers.
type RuntimePromptConfig struct {
	// Minimal flips the default of every section to off. Explicit
	// per-section values always win over Minimal.
	Minimal             *bool `json:"minimal,omitempty"`
	Environment         *bool `json:"environment,omitempty"`
	GitStatus           *bool `json:"gitStatus,omitempty"`
	ProjectInstructions *bool `json:"projectInstructions,omitempty"`
	McpTools            *bool `json:"mcpTools,omitempty"`
	CompactionSummary   *bool `json:"compactionSummary,omitempty"`
	MemoryWalkUp        *bool `json:"memoryWalkUp,omitempty"`
	MemoryImports       *bool `json:"memoryImports,omitempty"`
	AutoMemory          *bool `json:"autoMemory,omitempty"`
	Posture             *bool `json:"posture,omitempty"`
	Communication       *bool `json:"communication,omitempty"`
	TaskManagement      *bool `json:"taskManagement,omitempty"`
	DoingTasks          *bool `json:"doingTasks,omitempty"`
	ToolPolicy          *bool `json:"toolPolicy,omitempty"`
	GitSafety           *bool `json:"gitSafety,omitempty"`
	ContextManagement   *bool `json:"contextManagement,omitempty"`
	// MemoryMaxBytes caps the combined injected memory content (0 = default).
	MemoryMaxBytes int `json:"memoryMaxBytes,omitempty"`
}

// RuntimeSandboxConfig holds sandbox-related settings extracted from config.
type RuntimeSandboxConfig struct {
	Enabled               *bool    `json:"enabled,omitempty"`
	NamespaceRestrictions *bool    `json:"namespaceRestrictions,omitempty"`
	NetworkIsolation      *bool    `json:"networkIsolation,omitempty"`
	FilesystemMode        string   `json:"filesystemMode,omitempty"`
	AllowedMounts         []string `json:"allowedMounts,omitempty"`
}

// RuntimeHookConfig holds the hook command lists extracted from settings.
type RuntimeHookConfig struct {
	PreToolUse         []string
	PostToolUse        []string
	PostToolUseFailure []string
}

// RuntimePluginConfig holds plugin-related settings.
type RuntimePluginConfig struct {
	EnabledPlugins      map[string]bool
	ExternalDirectories []string
	InstallRoot         string
	RegistryPath        string
	BundledRoot         string
	MaxOutputTokens     int
}

// RuntimePermissionRuleConfig holds permission rule lists.
type RuntimePermissionRuleConfig struct {
	Allow []string
	Deny  []string
	Ask   []string
}

// ProviderFallbackConfig describes the primary model and fallback chain.
type ProviderFallbackConfig struct {
	Primary   string
	Fallbacks []string
}

// IsEmpty returns true if no primary or fallbacks are configured.
func (c ProviderFallbackConfig) IsEmpty() bool {
	return c.Primary == "" && len(c.Fallbacks) == 0
}

// decodeInto sets *dst from raw only when raw decodes whole: a field that
// does not parse stays at its zero value, never half-filled — the contract
// ExtractFeatureConfig states.
func decodeInto[T any](raw json.RawMessage, dst *T) {
	var v T
	if json.Unmarshal(raw, &v) == nil {
		*dst = v
	}
}

// ExtractFeatureConfig extracts a RuntimeFeatureConfig from raw JSON settings data.
// Missing or unparseable fields are left at zero values.
func ExtractFeatureConfig(data []byte) RuntimeFeatureConfig {
	var cfg RuntimeFeatureConfig

	var raw map[string]json.RawMessage
	if json.Unmarshal(data, &raw) != nil {
		return cfg
	}

	// Model
	if v, ok := raw["model"]; ok {
		var s string
		if json.Unmarshal(v, &s) == nil {
			cfg.Model = s
		}
	}

	// Aliases
	if v, ok := raw["aliases"]; ok {
		var m map[string]string
		if json.Unmarshal(v, &m) == nil {
			cfg.Aliases = m
		}
	}

	// Permission mode (from permissions.defaultMode or legacy permissionMode)
	if v, ok := raw["permissions"]; ok {
		var perms map[string]json.RawMessage
		if json.Unmarshal(v, &perms) == nil {
			if dm, ok2 := perms["defaultMode"]; ok2 {
				var s string
				if json.Unmarshal(dm, &s) == nil {
					cfg.PermissionMode = s
				}
			}
			// Permission rules
			if ar, ok2 := perms["allow"]; ok2 {
				decodeInto(ar, &cfg.PermissionRules.Allow)
			}
			if dr, ok2 := perms["deny"]; ok2 {
				decodeInto(dr, &cfg.PermissionRules.Deny)
			}
			if askr, ok2 := perms["ask"]; ok2 {
				decodeInto(askr, &cfg.PermissionRules.Ask)
			}
		}
	}
	// Legacy permissionMode fallback
	if cfg.PermissionMode == "" {
		if v, ok := raw["permissionMode"]; ok {
			var s string
			if json.Unmarshal(v, &s) == nil {
				cfg.PermissionMode = s
			}
		}
	}

	// Hooks
	if v, ok := raw["hooks"]; ok {
		var hooks map[string]json.RawMessage
		if json.Unmarshal(v, &hooks) == nil {
			if pre, ok2 := hooks["PreToolUse"]; ok2 {
				decodeInto(pre, &cfg.Hooks.PreToolUse)
			}
			if post, ok2 := hooks["PostToolUse"]; ok2 {
				decodeInto(post, &cfg.Hooks.PostToolUse)
			}
			if fail, ok2 := hooks["PostToolUseFailure"]; ok2 {
				decodeInto(fail, &cfg.Hooks.PostToolUseFailure)
			}
		}
	}

	// Plugins
	if v, ok := raw["plugins"]; ok {
		var plugins map[string]json.RawMessage
		if json.Unmarshal(v, &plugins) == nil {
			if ep, ok2 := plugins["enabled"]; ok2 {
				decodeInto(ep, &cfg.Plugins.EnabledPlugins)
			}
			if ed, ok2 := plugins["externalDirectories"]; ok2 {
				decodeInto(ed, &cfg.Plugins.ExternalDirectories)
			}
			if ir, ok2 := plugins["installRoot"]; ok2 {
				var s string
				if json.Unmarshal(ir, &s) == nil {
					cfg.Plugins.InstallRoot = s
				}
			}
			if rp, ok2 := plugins["registryPath"]; ok2 {
				var s string
				if json.Unmarshal(rp, &s) == nil {
					cfg.Plugins.RegistryPath = s
				}
			}
			if br, ok2 := plugins["bundledRoot"]; ok2 {
				var s string
				if json.Unmarshal(br, &s) == nil {
					cfg.Plugins.BundledRoot = s
				}
			}
			if mot, ok2 := plugins["maxOutputTokens"]; ok2 {
				var n int
				if json.Unmarshal(mot, &n) == nil {
					cfg.Plugins.MaxOutputTokens = n
				}
			}
		}
	}
	// Legacy enabledPlugins fallback
	if len(cfg.Plugins.EnabledPlugins) == 0 {
		if v, ok := raw["enabledPlugins"]; ok {
			decodeInto(v, &cfg.Plugins.EnabledPlugins)
		}
	}

	// Provider fallbacks
	if v, ok := raw["providerFallbacks"]; ok {
		var fb map[string]json.RawMessage
		if json.Unmarshal(v, &fb) == nil {
			if p, ok2 := fb["primary"]; ok2 {
				var s string
				if json.Unmarshal(p, &s) == nil {
					cfg.ProviderFallbacks.Primary = s
				}
			}
			if f, ok2 := fb["fallbacks"]; ok2 {
				decodeInto(f, &cfg.ProviderFallbacks.Fallbacks)
			}
		}
	}

	// Trusted roots
	if v, ok := raw["trustedRoots"]; ok {
		decodeInto(v, &cfg.TrustedRoots)
	}

	// Sandbox
	if v, ok := raw["sandbox"]; ok {
		var sandbox map[string]json.RawMessage
		if json.Unmarshal(v, &sandbox) == nil {
			if e, ok2 := sandbox["enabled"]; ok2 {
				var b bool
				if json.Unmarshal(e, &b) == nil {
					cfg.Sandbox.Enabled = &b
				}
			}
			if nr, ok2 := sandbox["namespaceRestrictions"]; ok2 {
				var b bool
				if json.Unmarshal(nr, &b) == nil {
					cfg.Sandbox.NamespaceRestrictions = &b
				}
			}
			if ni, ok2 := sandbox["networkIsolation"]; ok2 {
				var b bool
				if json.Unmarshal(ni, &b) == nil {
					cfg.Sandbox.NetworkIsolation = &b
				}
			}
			if fm, ok2 := sandbox["filesystemMode"]; ok2 {
				var s string
				if json.Unmarshal(fm, &s) == nil {
					cfg.Sandbox.FilesystemMode = s
				}
			}
			if am, ok2 := sandbox["allowedMounts"]; ok2 {
				decodeInto(am, &cfg.Sandbox.AllowedMounts)
			}
		}
	}

	return cfg
}
