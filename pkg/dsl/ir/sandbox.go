package ir

// SandboxSpec is the IR representation of a `sandbox:` block on a
// workflow or node. It is the bridge between the DSL surface (parsed
// in pkg/dsl/parser, stored on ast.WorkflowDecl/AgentDecl/...) and
// the runtime sandbox abstraction in pkg/sandbox.
//
// We mirror pkg/sandbox.Spec but keep the two types distinct so the
// IR remains free of runtime-level dependencies (drivers, factories,
// network proxies). pkg/runtime converts an IR SandboxSpec to a
// pkg/sandbox.Spec at engine start time via [ToSandbox].
//
// Phase 0 only wires the simple `sandbox: <ident>` form (none|auto)
// — Mode is the only meaningful field. The richer fields (Image,
// Build, Mounts, Network, ...) are populated by Phase 1+ when the
// block-form parser ships.
type SandboxSpec struct {
	// Mode is one of "" (inherit), "none" (explicit opt-out),
	// "auto" (read .devcontainer/devcontainer.json), or "inline"
	// (use the sibling fields). Phase 0 accepts "", "none", and
	// "auto"; "inline" lands in Phase 1.
	Mode string

	// Image is the container image reference. Phase 1.
	Image string

	// Build, when non-nil, asks the driver to build an image at run
	// start. Phase 2.
	Build *SandboxBuild

	// Mounts adds extra bind mounts (devcontainer mount syntax).
	// Phase 2.
	Mounts []string

	// Env is the containerEnv map. Phase 1.
	Env map[string]string

	// User is the devcontainer remoteUser override. Phase 1.
	User string

	// PostCreate is the devcontainer postCreateCommand. Phase 2.
	PostCreate string

	// WorkspaceFolder overrides the in-sandbox workspace path
	// (default `/workspace`). Phase 1.
	WorkspaceFolder string

	// Network controls egress filtering. Phase 3.
	Network *SandboxNetwork
}

// SandboxBuild describes a Dockerfile-based image build (Phase 2).
type SandboxBuild struct {
	Dockerfile string
	Context    string
	Args       map[string]string
}

// SandboxNetwork is the IR representation of a sandbox.network: block
// (Phase 3).
type SandboxNetwork struct {
	Mode    string   // "allowlist" | "denylist" | "open" | ""
	Preset  string   // "iterion-default" or named preset
	Rules   []string // glob patterns + "!exclusions"
	Inherit string   // "merge" (default) | "replace" | "append" — node scope only
}

// IsActive reports whether the spec requests an active sandbox mode
// (auto or inline). Convenience for diagnostics that only fire when
// a non-trivial mode is chosen.
func (s *SandboxSpec) IsActive() bool {
	if s == nil {
		return false
	}
	switch s.Mode {
	case "auto", "inline":
		return true
	}
	return false
}

// FromIdent builds a SandboxSpec from the simple `sandbox: <ident>`
// DSL form. Returns (nil, false) when the identifier is empty (the
// user did not declare a sandbox block at this scope) and (nil, true)
// for "" → no spec but also no error. An unrecognised identifier is
// reported via the boolean — callers turn it into a parse error or a
// validation diagnostic.
//
// Phase 0 accepts only "none" and "auto"; "inline" requires a block
// body which the Phase 0 parser does not yet emit.
func FromIdent(ident string) (*SandboxSpec, bool) {
	switch ident {
	case "":
		return nil, true
	case "none", "auto":
		return &SandboxSpec{Mode: ident}, true
	}
	return nil, false
}
