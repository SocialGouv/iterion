package sandbox

import (
	"fmt"
	"path"
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

// EngineBinaryContainerPath is the in-container path where the engine's own
// iterion binary is expected. Two arrangements put it there: addClawBinaryMount
// (pkg/runtime) bind-mounts the host binary read-only at this target for
// claw-carrying workflows, and the production runner image bakes the binary at
// the same path otherwise. Consumers that execute inside a sandbox hand this
// path to bot scripts rather than the host resolution, which names a file the
// container cannot see.
const EngineBinaryContainerPath = "/usr/local/bin/iterion"

// EngineBinaryEnvVar is the environment variable through which the engine
// hands a tool node the path of ITS OWN iterion binary — the same resolution
// proc.LocateIterionBinary serves the MCP helpers, or the container path when
// the command runs sandboxed. A bot script that re-enters the CLI resolves it
// from here (PATH next), never from the workspace: a fallback there executes
// the tree under audit (#1799).
const EngineBinaryEnvVar = "ITERION_ENGINE_BIN"

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

	// ImageFallback is a second image ref the driver may pull when
	// Image cannot be resolved at the registry. It is set ONLY when the
	// engine picked Image itself (the version-pinned built-in default),
	// never when an operator named one: an explicit request must fail
	// loudly rather than silently run something else. Empty = no
	// fallback.
	ImageFallback string

	// Build, when non-nil, asks the driver to build an image from a
	// Dockerfile at run start. Mutually exclusive with Image. Phase 2.
	Build *Build

	// Mounts adds bind mounts on top of the implicit workspace mount.
	// devcontainer-compatible mount syntax
	// (`source=...,target=...,type=bind`). Phase 2.
	Mounts []string

	// Tmpfs lists `docker run --tmpfs` entries (e.g.
	// "/home/jo:uid=1000,gid=1000,mode=0755"). host_state lays a
	// uid-owned writable tmpfs at the forced HOME path so $HOME itself
	// (go/devbox/npm caches) is writable; the persistent state binds nest
	// on top. Docker driver only — host_state is rejected on k8s.
	Tmpfs []string

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

	// HostState selects whether the host's `~/.iterion` (run store)
	// and `~/.claude` (Claude Code OAuth + per-project sessions) are
	// auto-mounted into the sandbox so persistent memory survives
	// across runs. "" defaults to HostStateAuto when the sandbox is
	// active; HostStateNone disables the auto-mount entirely.
	HostState HostState

	// Network, when non-nil, controls egress filtering. Phase 3.
	Network *Network

	// SecretFiles are per-run plaintext payloads materialised as read-only
	// files inside the sandbox. Runtime fills this from protected workflow
	// secrets after resolving the effective spec; drivers must never log
	// Value. Docker writes Value to a host temp file and bind-mounts it;
	// Kubernetes creates a per-run Secret and projects each key.
	SecretFiles []SecretFileMount

	// BindSourceChecks names every bind source the plan verified because it
	// lives under a directory the sandbox itself writes — the project
	// scratch, the sandbox's own cache store and the read-only config
	// overlays under the read-write iterion home, the worktree `.git`
	// tree's shadows, and the run's own workspace when the engine laid it
	// under that home. A source under no such root — an operator checkout
	// the launch named, a driver-written temp file — is not a source a
	// sandbox could have chosen and is named by no entry.
	//
	// A bind's source is resolved by the host kernel at container CREATION,
	// but the engine verifies these sources when the PLAN is built — before
	// the driver pulls the image, which is minutes. A sandboxed run alive
	// through that window can swap a verified leaf for a symlink and choose
	// where the next creation's mount lands. So every entry here is
	// re-verified at the driver border, immediately before the container is
	// created, with the same question ([safepath.RealBindSource]); a source
	// that no longer verifies refuses the creation instead of mounting what
	// the swap chose. The border answer ([sandbox.AnswerBindSourceCheck],
	// shared with the witnesses that replay it over a plan's own entries)
	// parts ways on the source's shape there: a directory keeps the store
	// ask ([safepath.EnsureRealBindSourceDir]), a file — a copy seed, a
	// host-bound state file — is asked for identity alone, never creation
	// (creating on a file's path refused honest containers and would stand
	// a directory where the operator keeps a file), and a source gone since
	// the plan refuses whatever its shape — the engine-laid workspace
	// because nothing may stand in for the tree the engine actually laid,
	// every other source because the border recreates nothing the
	// plan-to-create window removed; the cold recreate is the plan's move,
	// and the retry replans.
	//
	// The ask judges the engine's own filesystem: with the daemon on another
	// host (DOCKER_HOST against a remote engine), the daemon resolves the
	// source on a tree these checks never saw — the same blindness the
	// plan-time ask already had, and a limit of the whole field.
	//
	// Drivers without host bind mounts never receive entries: the runtime
	// writes this field only where it also lays host binds. They may ignore
	// the field.
	BindSourceChecks []BindSourceCheck

	// ReadBackPaths names the container paths whose content the run must
	// recover after the container is gone (ADR-143). It is a DECLARATION,
	// never inferred from the mounts: the runtime names a path here when it
	// also arranges the storage that serves it — a host bind where the
	// driver has a filesystem, driver-owned storage (an emptyDir) where it
	// has none — and a spec whose declaration no mount serves is refused at
	// admission (Validate) rather than served silently half-way. The run
	// reads the content back at teardown through the driver's drain
	// (WorkspaceExporter.ExportRunFiles), keyed on RunInfo.ReadBackDirs.
	ReadBackPaths []string
}

// ReadBackDir is one entry of the drain manifest: a declared read-back
// container path, the host directory that receives its content at
// teardown, and the figure its set is metered against. The driver drains
// the path; the run file store owns the directory (its write target), and
// the two meet only here.
type ReadBackDir struct {
	// ContainerPath is the declared path inside the container (absolute,
	// clean — the same text Spec.ReadBackPaths carries).
	ContainerPath string
	// HostDir is the host directory the drained content lands in: the run
	// file store's own write target, so no consumer of the drained set
	// needs to know it (ADR-143 — no consumer reads the set by host path).
	HostDir string
	// CapBytes is the figure THIS dir's set is metered against at the
	// drain, resolved by [ReadBackCapFor] from the mount that serves the
	// path — the mount's own size, the [ReadBackCapEnv] override, or
	// [ReadBackCapBytes]. Zero reads as the default (Cap), so a manifest
	// entry built without it meters on the named figure.
	CapBytes int64
}

// Cap is the figure the drain meters this dir's set against: CapBytes, or
// [ReadBackCapBytes] where the entry named none.
func (d ReadBackDir) Cap() int64 {
	if d.CapBytes > 0 {
		return d.CapBytes
	}
	return ReadBackCapBytes
}

// BindSourceCheck is one bind source the driver must re-verify immediately
// before creating the container. Root is the sandbox-writable store the
// source must stay under (what [safepath.RealBindSource] compares against);
// Path is the source the mount string names.
type BindSourceCheck struct {
	Root string
	Path string

	// MustExist marks a source the ENGINE laid for this run and binds at
	// that path — the workspace worktree. Nothing at the border recreates
	// any verified source gone since the plan (the border recreates nothing
	// the window removed); for the workspace the refusal also guards the
	// meaning: an empty stand-in directory would start the run on a tree
	// the engine never laid.
	MustExist bool
}

type SecretFileMount struct {
	Name      string
	MountPath string
	Env       string
	Value     []byte
}

// HostState selects how the host's persistent state directories
// (`~/.iterion`, `~/.claude`) are exposed to the sandbox.
type HostState string

const (
	// HostStateUnset defers to the runtime default (currently auto when
	// the sandbox is active).
	HostStateUnset HostState = ""
	// HostStateAuto bind-mounts the host's `~/.iterion` and `~/.claude`
	// directories at the same absolute path inside the container, and
	// (on Linux, when the spec does not pin a User) maps the container
	// process to the host UID/GID so writes back to those mounts stay
	// owned by the host user.
	HostStateAuto HostState = "auto"
	// HostStateNone disables the auto-mount and UID remap entirely.
	HostStateNone HostState = "none"
)

// IsValid reports whether h is one of the legal host-state values.
func (h HostState) IsValid() bool {
	switch h {
	case HostStateUnset, HostStateAuto, HostStateNone:
		return true
	}
	return false
}

// Active reports whether the host-state auto-mount should fire.
// Treats the unset value as "auto" so callers can resolve precedence
// elsewhere (CLI > workflow > env > default) and then trust the spec.
func (h HostState) Active() bool {
	return h == HostStateAuto || h == HostStateUnset
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
			return fmt.Errorf("sandbox.network: invalid inherit %q (want replace or append; omit it to merge, the default)", s.Network.Inherit)
		}
	}
	if s.WorkspaceFolder != "" && !strings.HasPrefix(s.WorkspaceFolder, "/") {
		return fmt.Errorf("sandbox.workspaceFolder %q must be absolute", s.WorkspaceFolder)
	}
	if !s.HostState.IsValid() {
		return fmt.Errorf("sandbox: invalid host_state %q (want \"\", auto, or none)", s.HostState)
	}
	for _, sf := range s.SecretFiles {
		if sf.MountPath == "" || !strings.HasPrefix(sf.MountPath, "/") {
			return fmt.Errorf("sandbox.secretFiles[%s]: mount_path %q must be absolute", sf.Name, sf.MountPath)
		}
		if strings.ContainsAny(sf.MountPath, "\n\r\x00") {
			return fmt.Errorf("sandbox.secretFiles[%s]: mount_path contains a control character", sf.Name)
		}
		if path.Clean(sf.MountPath) != sf.MountPath || sf.MountPath == "/" {
			return fmt.Errorf("sandbox.secretFiles[%s]: mount_path %q must be a clean absolute file path", sf.Name, sf.MountPath)
		}
		if sf.Env != "" && !validEnvName(sf.Env) {
			return fmt.Errorf("sandbox.secretFiles[%s]: env %q is not a valid environment variable name", sf.Name, sf.Env)
		}
	}
	for _, p := range s.ReadBackPaths {
		if !strings.HasPrefix(p, "/") || path.Clean(p) != p || p == "/" {
			return fmt.Errorf("sandbox: read-back path %q must be a clean absolute container path", p)
		}
		// ADR-143: named ⇒ served. The declaration is checked against the
		// mounts as they stand, whatever their type — a host bind on a
		// driver that has a filesystem, driver-owned storage where it has
		// none — so a read-back path no mount serves refuses here instead
		// of becoming the promise-about-a-directory-nobody-has defect the
		// bind-dropping rule (#815) exists for.
		if MountServing(s.Mounts, p) == "" {
			return fmt.Errorf("sandbox: read-back path %q is served by no mount — declare it only alongside the storage that serves it (ADR-143)", p)
		}
	}
	return nil
}

// MountServing returns the mount whose target is containerPath or a
// parent of it, or "" when none does — the NEAREST such mount when they
// nest (the deepest target, the first of equals), because a mount shadows
// its parent's volume under its own target: the files at containerPath
// live in its storage, and its `size=` is the figure that bounds them
// (ReadBackCapFor). The one reading of "does a mount serve this path":
// the admission half of the read-back contract (the declaration is asked
// of the mounts as they stand, not of the driver's intentions), the
// runtime's decision to add or skip its own storage at a declared path
// (it asks whether the nearest kept mount sits AT the path — a parent
// holds the path in its tree, never as a directory it promises), the
// bind-dropping rule's judgment of what a dropped bind took with it, and
// the drain's figure all ask it here — a serving predicate kept in two
// places answers differently the day one copy stops matching the other.
func MountServing(mounts []string, containerPath string) string {
	want := path.Clean(containerPath)
	serving, depth := "", -1
	for _, m := range mounts {
		tgt := MountTargetOf(m)
		if tgt == "" {
			continue
		}
		if c := path.Clean(tgt); (want == c || strings.HasPrefix(want, c+"/")) && len(c) > depth {
			serving, depth = m, len(c)
		}
	}
	return serving
}

// MountTargetOf extracts a mount string's `target=` field ("dst" and
// "destination" are the same key's aliases), "" when it names none. The
// one parser of the mount-target grammar: admission and every runtime
// mount reader parse through it, so an alias added here is visible to
// all of them — a second parser reads the language the day it was
// written and never hears about the next alias.
func MountTargetOf(m string) string {
	for _, part := range strings.Split(m, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "target", "dst", "destination":
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func validEnvName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r == '_':
			continue
		case i > 0 && r >= '0' && r <= '9':
			continue
		default:
			return false
		}
	}
	return true
}
