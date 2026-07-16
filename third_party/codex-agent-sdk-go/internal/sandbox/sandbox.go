// Package sandbox provides sandbox configuration types for the Codex CLI.
package sandbox

// NetworkConfig configures network access for the sandbox.
type NetworkConfig struct {
	AllowUnixSockets    []string `json:"allowUnixSockets,omitempty"`
	AllowAllUnixSockets *bool    `json:"allowAllUnixSockets,omitempty"`
	AllowLocalBinding   *bool    `json:"allowLocalBinding,omitempty"`
	HTTPProxyPort       *int     `json:"httpProxyPort,omitempty"`
	SOCKSProxyPort      *int     `json:"socksProxyPort,omitempty"`
}

// IgnoreViolations configures which violations to ignore.
type IgnoreViolations struct {
	File    []string `json:"file,omitempty"`
	Network []string `json:"network,omitempty"`
}

// Settings configures CLI sandbox behavior.
type Settings struct {
	Enabled                   *bool             `json:"enabled,omitempty"`
	AutoAllowBashIfSandboxed  *bool             `json:"autoAllowBashIfSandboxed,omitempty"`
	ExcludedCommands          []string          `json:"excludedCommands,omitempty"`
	AllowUnsandboxedCommands  *bool             `json:"allowUnsandboxedCommands,omitempty"`
	Network                   *NetworkConfig    `json:"network,omitempty"`
	IgnoreViolations          *IgnoreViolations `json:"ignoreViolations,omitempty"`
	EnableWeakerNestedSandbox *bool             `json:"enableWeakerNestedSandbox,omitempty"`
}
