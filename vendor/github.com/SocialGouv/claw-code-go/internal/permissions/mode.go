package permissions

import "fmt"

// PermissionMode represents the security level of a session or tool requirement.
// Modes are ordered: ReadOnly < WorkspaceWrite < DangerFullAccess < Prompt < Allow.
//
// Explicit int assignments (not iota) prevent accidental ordering bugs if
// constants are reordered. This matches Rust's PermissionMode enum ordering.
type PermissionMode int

const (
	// ModeReadOnly allows read-only operations only.
	ModeReadOnly PermissionMode = 0
	// ModeWorkspaceWrite allows writing within the workspace directory.
	ModeWorkspaceWrite PermissionMode = 1
	// ModeDangerFullAccess allows arbitrary command execution and system access.
	ModeDangerFullAccess PermissionMode = 2
	// ModePrompt requires interactive approval for operations above the mode level.
	ModePrompt PermissionMode = 3
	// ModeAllow permits all operations without prompting.
	ModeAllow PermissionMode = 4
)

// CLI-facing aliases preserving backward compatibility with the original Go
// permission modes. These resolve to the Rust-equivalent security levels.
const (
	ModeDefault           = ModePrompt         // "default" → consult ruleset, ask when needed
	ModeAcceptEdits       = ModeWorkspaceWrite // "accept-edits" → auto-allow workspace writes
	ModeBypassPermissions = ModeAllow          // "bypass" → allow everything
	ModePlan              = ModeReadOnly       // "plan" → deny all execution
)

// ParsePermissionMode converts a CLI string to a PermissionMode.
// Accepts both legacy Go names and Rust-style canonical names.
func ParsePermissionMode(s string) (PermissionMode, error) {
	switch s {
	case "default", "prompt", "":
		return ModePrompt, nil
	case "accept-edits", "workspace-write":
		return ModeWorkspaceWrite, nil
	case "bypass", "allow":
		return ModeAllow, nil
	case "plan", "read-only":
		return ModeReadOnly, nil
	case "danger-full-access":
		return ModeDangerFullAccess, nil
	default:
		return ModePrompt, fmt.Errorf("unknown permission mode %q (want: default, accept-edits, bypass, plan, read-only, workspace-write, danger-full-access, prompt, allow)", s)
	}
}

// String returns the canonical (Rust-compatible) string representation.
func (m PermissionMode) String() string {
	switch m {
	case ModeReadOnly:
		return "read-only"
	case ModeWorkspaceWrite:
		return "workspace-write"
	case ModeDangerFullAccess:
		return "danger-full-access"
	case ModePrompt:
		return "prompt"
	case ModeAllow:
		return "allow"
	default:
		return "unknown"
	}
}

// AsStr returns the Rust-compatible string (same as String, provided for
// naming parity with Rust's PermissionMode::as_str).
func (m PermissionMode) AsStr() string {
	return m.String()
}
