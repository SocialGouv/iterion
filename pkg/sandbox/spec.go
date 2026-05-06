package sandbox

import (
	"fmt"
	"strings"
)

// Mode discriminates how a [Spec] should be resolved.
//
//   - ModeInherit (empty): use the parent scope's effective mode
//     (node inherits workflow, workflow inherits global, global
//     defaults to ModeNone).
//   - ModeNone: explicit opt-out — no sandbox even if a parent scope
//     activates one. Useful for a tool node that needs host access
//     while the rest of the workflow runs sandboxed.
//   - ModeAuto: read .devcontainer/devcontainer.json from the repo
//     root and use it as the spec source.
//   - ModeInline: use the sibling fields ([Spec.Image], [Spec.Build],
//     [Spec.Mounts], …) as-is.
type Mode string

const (
	ModeInherit Mode = ""
	ModeNone    Mode = "none"
	ModeAuto    Mode = "auto"
	ModeInline  Mode = "inline"
)

// IsValid reports whether m is one of the four legal modes. Callers
// should validate user-supplied values before propagating them through
// the IR.
func (m Mode) IsValid() bool {
	switch m {
	case ModeInherit, ModeNone, ModeAuto, ModeInline:
		return true
	}
	return false
}

// IsActive reports whether this mode requests a sandbox at runtime.
// ModeInherit and ModeNone return false.
func (m Mode) IsActive() bool {
	return m == ModeAuto || m == ModeInline
}

// Spec is the resolved, driver-agnostic sandbox specification.
//
// A Spec is produced by [Resolve] from the precedence chain
// (node > workflow > global) and then handed to a [Driver] for
// validation and start-up. Phase 0 wires only [Spec.Mode]; the richer
// fields ([Spec.Image], [Spec.Mounts], …) are populated by Phase 1+.
//
// Spec is the in-memory shape; the IR/AST counterpart lives in
// pkg/dsl/ir.SandboxSpec and is converted via [FromIR] at engine start
// time.
type Spec struct {
	// Mode is the activation mode.
	Mode Mode

	// Image is the container image reference (e.g.
	// "ghcr.io/myorg/img:tag"). Mutually exclusive with Build.
	// Phase 1.
	Image string

	// Build, when non-nil, asks the driver to build an image from a
	// Dockerfile at run start. Mutually exclusive with Image. Phase 2.
	Build *Build

	// Mounts adds bind mounts on top of the implicit workspace mount.
	// devcontainer-compatible mount syntax
	// (`source=...,target=...,type=bind`). Phase 2.
	Mounts []string

	// Env is the containerEnv map injected into the sandbox. Phase 1.
	Env map[string]string

	// User overrides the sandbox UID/username (devcontainer
	// `remoteUser`). Phase 1.
	User string

	// PostCreate is a shell snippet executed once after the sandbox
	// starts (devcontainer `postCreateCommand`). Phase 2.
	PostCreate string

	// WorkspaceFolder is the absolute path inside the sandbox where
	// the workspace is mounted. Defaults to "/workspace" when empty.
	// Phase 1.
	WorkspaceFolder string

	// Network, when non-nil, controls egress filtering. Phase 3.
	Network *Network
}

// Build describes a Dockerfile-based image build (Phase 2).
type Build struct {
	// Dockerfile is the relative path to the Dockerfile. Defaults to
	// "Dockerfile" when empty.
	Dockerfile string

	// Context is the build context directory (relative to the repo
	// root). Defaults to the directory containing the Dockerfile.
	Context string

	// Args are build-time substitutions (--build-arg).
	Args map[string]string
}

// Network controls a sandbox's egress policy (Phase 3).
//
// Rules are evaluated last-match-wins; an unmatched host falls back
// to the default of [Network.Mode] (allowlist denies, denylist
// allows). [Network.Preset], when non-empty, is applied as the
// implicit prefix of [Network.Rules].
type Network struct {
	// Mode is "allowlist" (default deny + allow listed),
	// "denylist" (default allow + deny listed), or "open" (no proxy).
	Mode NetworkMode

	// Preset names a built-in rule list ("iterion-default" covers
	// LLM endpoints + package registries + code hosts).
	Preset string

	// Rules are glob patterns ("**.github.com", "!evil.site",
	// "1.2.3.4/16"). Last match wins. Phase 3 documents the syntax.
	Rules []string

	// Inherit governs how a node-level [Network] composes with its
	// workflow-level parent. Empty means "merge". Only meaningful at
	// node scope; ignored at workflow scope.
	Inherit InheritMode
}

// NetworkMode is the egress default for unmatched hosts.
type NetworkMode string

const (
	NetworkModeUnset     NetworkMode = ""
	NetworkModeAllowlist NetworkMode = "allowlist"
	NetworkModeDenylist  NetworkMode = "denylist"
	NetworkModeOpen      NetworkMode = "open"
)

// IsValid reports whether m is one of the legal modes (or unset).
func (m NetworkMode) IsValid() bool {
	switch m {
	case NetworkModeUnset, NetworkModeAllowlist, NetworkModeDenylist, NetworkModeOpen:
		return true
	}
	return false
}

// InheritMode controls how node-level network rules compose with
// their workflow parent.
type InheritMode string

const (
	// InheritMerge appends node rules after workflow rules
	// (last-match-wins applies). Default.
	InheritMerge InheritMode = ""
	// InheritReplace discards parent rules and uses node rules only.
	InheritReplace InheritMode = "replace"
	// InheritAppend is identical to merge today; reserved for future
	// nuance (e.g. preserving parent mode while extending rules).
	InheritAppend InheritMode = "append"
)

// IsValid reports whether m is one of the legal inherit modes.
func (m InheritMode) IsValid() bool {
	switch m {
	case InheritMerge, InheritReplace, InheritAppend:
		return true
	}
	return false
}

// Validate returns nil if the spec is internally consistent. It does
// NOT consult driver capabilities — that is [Driver.Prepare]'s job.
func (s *Spec) Validate() error {
	if s == nil {
		return nil
	}
	if !s.Mode.IsValid() {
		return fmt.Errorf("sandbox: invalid mode %q (want \"\", none, auto, or inline)", s.Mode)
	}
	if s.Mode == ModeInline && s.Image == "" && s.Build == nil {
		return fmt.Errorf("sandbox: mode=inline requires either image or build to be set")
	}
	if s.Image != "" && s.Build != nil {
		return fmt.Errorf("sandbox: image and build are mutually exclusive")
	}
	if s.Network != nil {
		if !s.Network.Mode.IsValid() {
			return fmt.Errorf("sandbox.network: invalid mode %q (want allowlist, denylist, or open)", s.Network.Mode)
		}
		if !s.Network.Inherit.IsValid() {
			return fmt.Errorf("sandbox.network: invalid inherit %q (want merge, replace, or append)", s.Network.Inherit)
		}
	}
	if s.WorkspaceFolder != "" && !strings.HasPrefix(s.WorkspaceFolder, "/") {
		return fmt.Errorf("sandbox.workspaceFolder %q must be absolute", s.WorkspaceFolder)
	}
	return nil
}
