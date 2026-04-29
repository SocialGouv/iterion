package plugin

import (
	"encoding/json"
	"fmt"
)

// PluginKind represents the source type of a plugin.
// Uses string constants (not iota) so values are stable JSON strings.
type PluginKind string

const (
	KindBuiltin  PluginKind = "builtin"  // Hardcoded plugins
	KindBundled  PluginKind = "bundled"  // Pre-packaged with application
	KindExternal PluginKind = "external" // User-installed
)

func (k PluginKind) String() string {
	return string(k)
}

// Marketplace returns the marketplace string used in plugin IDs.
// Matches Rust's PluginKind::marketplace() → "builtin"/"bundled"/"external".
func (k PluginKind) Marketplace() string {
	return string(k)
}

func (k PluginKind) MarshalJSON() ([]byte, error) {
	return json.Marshal(string(k))
}

func (k *PluginKind) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	switch PluginKind(s) {
	case KindBuiltin, KindBundled, KindExternal:
		*k = PluginKind(s)
		return nil
	default:
		return fmt.Errorf("unknown PluginKind: %q", s)
	}
}

// PluginPermission is a manifest-level permission.
type PluginPermission string

const (
	PermissionRead    PluginPermission = "read"
	PermissionWrite   PluginPermission = "write"
	PermissionExecute PluginPermission = "execute"
)

// ValidPluginPermissions returns the set of valid permission strings.
func ValidPluginPermissions() map[PluginPermission]bool {
	return map[PluginPermission]bool{
		PermissionRead:    true,
		PermissionWrite:   true,
		PermissionExecute: true,
	}
}

// PluginToolPermission is the required permission level for a tool.
type PluginToolPermission string

const (
	ToolPermReadOnly         PluginToolPermission = "read-only"
	ToolPermWorkspaceWrite   PluginToolPermission = "workspace-write"
	ToolPermDangerFullAccess PluginToolPermission = "danger-full-access"
)

// ValidToolPermissions returns the set of valid tool permission strings.
func ValidToolPermissions() map[PluginToolPermission]bool {
	return map[PluginToolPermission]bool{
		ToolPermReadOnly:         true,
		ToolPermWorkspaceWrite:   true,
		ToolPermDangerFullAccess: true,
	}
}
