package runtime

// Permissions controls which tools are allowed during a session.
type Permissions struct {
	AllowBash      bool
	AllowFileWrite bool
	AllowFileRead  bool
	AllowGlob      bool
	AllowGrep      bool
}

// DefaultPermissions returns a Permissions struct with all tools allowed.
// Phase 1: allow everything.
func DefaultPermissions() *Permissions {
	return &Permissions{
		AllowBash:      true,
		AllowFileWrite: true,
		AllowFileRead:  true,
		AllowGlob:      true,
		AllowGrep:      true,
	}
}

// CheckPermission returns true if the given tool is permitted.
func CheckPermission(perm *Permissions, tool string) bool {
	if perm == nil {
		return false
	}
	switch tool {
	case "bash":
		return perm.AllowBash
	case "write_file":
		return perm.AllowFileWrite
	case "read_file":
		return perm.AllowFileRead
	case "glob":
		return perm.AllowGlob
	case "grep":
		return perm.AllowGrep
	default:
		// Allow unknown tools (file_edit, web_fetch, web_search, ask_user,
		// todo_write, MCP tools, etc.); PermManager handles the real gating.
		return true
	}
}
