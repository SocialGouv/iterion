// Package runtime — sandbox mount helpers extracted from sandbox.go
// to keep [resolveAndStartSandbox] focused on the lifecycle skeleton.
package runtime

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/rewrite"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/memory"
	"github.com/SocialGouv/iterion/pkg/plugin"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/store"
)

// sandboxScratchContainerPath is where ${PROJECT_SCRATCH_DIR} resolves
// inside a sandbox. Fixed rather than the host path so a prompt or tool
// node sees one stable location; applyScratchMount backs it with the
// project's host scratch dir.
const sandboxScratchContainerPath = "/tmp/iterion-scratch"

// applyScratchMount binds the project's host scratch dir onto
// sandboxScratchContainerPath and returns the host path it bound (""
// when it did not bind).
//
// Why back it at all: a sub-bot child runs in its OWN container, so a
// purely container-local scratch means the file a child writes there is
// invisible to the parent reading the same path afterwards. The child
// reports success and the parent reads an empty directory, so the defect
// surfaces far from its cause (observed on app-concept: four topic
// syntheses written, none readable at fan-in). Backing it also lets a
// crashed run resume from its own working state, and it makes the
// sandboxed layout identical to the un-sandboxed one — which several
// bots depend on, treating scratch as CROSS-RUN memory (docs-refresh's
// promises ledger, sec-audit-deps' package cache, wiki-gen's cache).
// Scoping the bind per run would silently empty those caches under the
// default sandbox-on posture while leaving them intact without it.
//
// No chmod, deliberately: the mount is writable because
// applyHostUIDRemap has already aligned the container process with the
// host UID/GID, which is exactly what makes the sibling ~/.iterion bind
// work. Widening the mode instead would be a real weakening — this
// directory holds `verify.sh`, which a deterministic tool node later
// EXECUTES, and `packages.jsonl`, which triage reads back as trusted.
//
// Skipped, with an event, when the bind could not be made writable or
// useful: no host bind mounts on the driver (kubernetes), host_state
// off (the posture that keeps every ~/.iterion bind out of the
// container), or an image pinning a UID that is not the host's — the
// EACCES case the container-local path exists to serve.
func applyScratchMount(
	spec *sandbox.Spec,
	repoRoot, workDir string,
	supportsHostBindMounts bool,
	emitEvent func(store.EventType, map[string]any) error,
	logger *iterlog.Logger,
) string {
	if spec == nil {
		return ""
	}
	// Never degrade silently: falling back to the container-local path
	// reinstates the cross-container blindness this function exists to
	// fix, and an operator hitting it must find the reason in the run
	// record rather than infer it from an empty directory.
	skip := func(reason string) string {
		if emitEvent != nil {
			_ = emitEvent(store.EventSandboxHostStateMounted, map[string]any{
				"enabled": false,
				"source":  "scratch",
				"reason":  reason,
			})
		}
		if logger != nil {
			logger.Warn("runtime: sandbox scratch stays container-local (%s) — a sub-bot fan-in through ${PROJECT_SCRATCH_DIR} will not see its children's files", reason)
		}
		return ""
	}
	if !supportsHostBindMounts {
		return skip("driver has no host bind mounts")
	}
	if !spec.HostState.Active() {
		// host_state=none already emitted its own disabled event, and
		// suppressing every ~/.iterion bind is the point of that posture.
		return ""
	}
	if uid, ok := parseUserUID(spec.User); ok && uid != os.Getuid() {
		return skip(fmt.Sprintf("container pins UID %d, host is %d", uid, os.Getuid()))
	}
	base := repoRoot
	if base == "" {
		base = workDir
	}
	hostScratch := memory.WorkspaceScratchDir(base)
	if hostScratch == "" {
		return skip("no project scratch dir could be resolved")
	}
	if err := os.MkdirAll(hostScratch, 0o700); err != nil {
		return skip(fmt.Sprintf("mkdir %s: %v", hostScratch, err))
	}
	spec.Mounts = append(spec.Mounts, fmt.Sprintf(
		"source=%s,target=%s,type=bind", hostScratch, sandboxScratchContainerPath))
	// `docker inspect` must never show a mount events.jsonl cannot explain.
	if emitEvent != nil {
		_ = emitEvent(store.EventSandboxHostStateMounted, map[string]any{
			"enabled": true,
			"source":  "scratch",
			"mounts":  []string{fmt.Sprintf("%s -> %s", hostScratch, sandboxScratchContainerPath)},
		})
	}
	return hostScratch
}

// addOptionalBindMount stats hostDir; if it exists and is readable it
// appends a bind-mount entry to spec.Mounts and returns the resolved
// container path. Missing hostDir is a silent skip — every aux mount
// (attachments, run-files, bundle) is optional. Returns "" when no
// mount was added so callers know to skip downstream wiring (env vars,
// etc.). label is used only in the warn log when stat fails for a
// non-ENOENT reason.
func addOptionalBindMount(
	spec *sandbox.Spec,
	hostDir, containerPath, defaultContainerPath, label string,
	readOnly bool,
	logger *iterlog.Logger,
) string {
	if hostDir == "" {
		return ""
	}
	_, statErr := os.Stat(hostDir)
	switch {
	case statErr == nil:
		if containerPath == "" {
			containerPath = defaultContainerPath
		}
		entry := fmt.Sprintf("source=%s,target=%s,type=bind", hostDir, containerPath)
		if readOnly {
			entry += ",readonly"
		}
		spec.Mounts = append(spec.Mounts, entry)
		return containerPath
	case errors.Is(statErr, fs.ErrNotExist):
		return ""
	default:
		if logger != nil {
			logger.Warn("runtime: sandbox %s host dir %s: %v — skipping mount", label, hostDir, statErr)
		}
		return ""
	}
}

// applyHostStateMounts auto-binds ~/.iterion and ~/.claude into the
// container at the same absolute path, so persistent memory survives
// across runs and Claude Code's cwd-derived project key resolves
// identically inside and outside the sandbox. Emits
// [store.EventSandboxHostStateMounted] with the precedence source so
// operators can audit why the mount fired (or didn't).
//
// Pre-conditions: spec is the resolved active spec; the caller has
// already decided that mounts/env mutations are safe.
// It RETURNS the host `~/.iterion` path it actually bind-mounted, at that same
// absolute path in the container, or "" when nothing was mounted — host_state
// off, or the auto-binder skipped it because the path is missing or overlaps
// the workspace bind. That is a fact about this run's mount plan, not a driver
// input, so it travels as a return value like addOptionalBindMount's, rather
// than as a field on the Spec every driver has to read past.
// seedGitIdentityEnv gives an in-sandbox `git commit` an author when the
// host's ~/.gitconfig is NOT mounted — i.e. under host_state: none. The mount
// was what carried the identity, so dropping it drops the identity too, and
// every commit-producing bot dies on "Author identity unknown" for a reason
// that has nothing to do with what it was asked to do.
//
// The identity is READ from the host's git config and passed as four env vars
// instead of mounting anything. `git config --get` resolves local-then-global
// exactly as git would inside the container, so the committer stays the
// operator — and the isolation host_state: none exists for is preserved: two
// values cross the boundary, not a home directory.
//
// Nothing is invented when the host has no identity either. git then fails
// with its own message, which names the real problem (a host that was never
// configured) instead of attributing commits to an identity nobody chose.
func seedGitIdentityEnv(spec *sandbox.Spec, workspacePath string, logger *iterlog.Logger) {
	if spec.Env == nil {
		spec.Env = map[string]string{}
	}
	// Signing is disabled for the same reason as under host_state: auto — the
	// gpg-agent socket does not cross the container boundary, so a
	// commit.gpgsign=true inherited from the repository's own config turns
	// every commit into "gpg: no secret key".
	if _, set := spec.Env["GIT_CONFIG_COUNT"]; !set {
		spec.Env["GIT_CONFIG_COUNT"] = "1"
		spec.Env["GIT_CONFIG_KEY_0"] = "commit.gpgsign"
		spec.Env["GIT_CONFIG_VALUE_0"] = "false"
	}

	name := gitConfigValue(workspacePath, "user.name")
	email := gitConfigValue(workspacePath, "user.email")
	if name == "" || email == "" {
		if logger != nil {
			logger.Warn("runtime: host_state=none and the host git config has no user.name/user.email (name=%q email=%q) — an in-sandbox `git commit` will fail with \"Author identity unknown\"", name, email)
		}
		return
	}
	// Only fill free slots: a workflow that sets its own committer means it.
	for key, value := range map[string]string{
		"GIT_AUTHOR_NAME":     name,
		"GIT_AUTHOR_EMAIL":    email,
		"GIT_COMMITTER_NAME":  name,
		"GIT_COMMITTER_EMAIL": email,
	} {
		if _, set := spec.Env[key]; !set {
			spec.Env[key] = value
		}
	}
}

// gitConfigValue reads one git config key as the host resolves it from dir
// (local, then global, then system). Returns "" when git fails for any reason
// — an unset key, a directory that is not a repository, or no git at all. The
// caller distinguishes those cases by what it does with an empty value, and
// says so; swallowing the difference here would be the silent part.
func gitConfigValue(dir, key string) string {
	cmd, cancel := gitCmd("-C", dir, "config", "--get", key)
	defer cancel()
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func applyHostStateMounts(
	spec *sandbox.Spec,
	wf *ir.Workflow,
	p SandboxParams,
	emitEvent func(store.EventType, map[string]any) error,
	logger *iterlog.Logger,
) (sharedStateDir string) {
	resolvedHostState, hsSource := pickHostState(workflowHostState(wf), p.HostStateOverride, p.HostStateDefault)
	spec.HostState = sandbox.HostState(resolvedHostState)
	if !spec.HostState.Active() {
		seedGitIdentityEnv(spec, p.WorkspacePath, logger)
		_ = emitEvent(store.EventSandboxHostStateMounted, map[string]any{
			"enabled": false,
			"source":  hsSource,
		})
		return ""
	}

	absWorkspace, absErr := filepath.Abs(p.WorkspacePath)
	if absErr != nil {
		absWorkspace = p.WorkspacePath
	}
	// Workspace at host's absolute path keeps cwd-derived state
	// (Claude Code project key, ${PROJECT_DIR}, absolute-path tool
	// args) resolvable identically in/out container. Preserve any
	// explicit DSL workspace_folder override.
	if spec.WorkspaceFolder == "" {
		spec.WorkspaceFolder = absWorkspace
	}

	homeDir := resolveHostHomeDir()
	iterionHomeDir := store.GlobalIterionDataDir()
	var claudeDir, claudeConfigPath, codexDir, gitConfigPath string
	if homeDir != "" {
		claudeDir = filepath.Join(homeDir, ".claude")
		// `~/.claude.json` is Claude Code's TOP-LEVEL config file — a
		// sibling of the ~/.claude directory, NOT inside it. Without it
		// the in-container CLI refuses to start when the mounted
		// ~/.claude/backups/ holds a config backup (written by the
		// operator's HOST sessions): it demands a manual restore, loops
		// the complaint on stderr, and in --print stream-json mode never
		// emits a single stdout byte nor exits — which the delegate sees
		// as a 90s cold-phase timeout on EVERY attempt (native:221edac8,
		// root-caused live on the 2026-07-07 sandboxed dogfood probes).
		claudeConfigPath = filepath.Join(homeDir, ".claude.json")
		// `~/.codex/auth.json` is where Codex CLI persists the
		// "Sign in with ChatGPT" OAuth token + account_id. claw's
		// OpenAI provider reads it when no OPENAI_API_KEY is set —
		// the only credential path for users on the ChatGPT-forfait
		// (no metered API keys). Without this mount, sandboxed
		// claw nodes that route to an openai/* model fail with
		// "provide either OPENAI_API_KEY or a (OAuthToken +
		// OpenAIChatGPTAccountID) pair sourced from Codex CLI
		// auth.json" — surfaced on the 2026-05-22 dogfood when
		// reviewer_gpt hit it on every feature_dev run.
		codexDir = filepath.Join(homeDir, ".codex")
		// `~/.gitconfig` carries the operator's git user.name +
		// user.email (and the rest of their global git config:
		// aliases, signing keys, etc.). Without this, in-container
		// `git commit` fails with "Author identity unknown" — the
		// classic blocker for any commit-producing bot
		// (feature_dev's commit_changes tool, docs-refresh's
		// post-fix commits, branch-improve-loop, …). Mount as a
		// file (collectHostStateMounts treats both files and dirs
		// the same; the bind is at the same host path so git
		// resolves `$HOME/.gitconfig` identically in/out of the
		// container).
		gitConfigPath = filepath.Join(homeDir, ".gitconfig")
	}

	mounts := collectHostStateMounts(absWorkspace, iterionHomeDir, claudeDir, claudeConfigPath, codexDir, gitConfigPath)
	mountPairs := make([]string, 0, len(mounts))
	for _, m := range mounts {
		entry := fmt.Sprintf("source=%s,target=%s,type=bind", m.HostPath, m.ContainerPath)
		if m.ReadOnly {
			entry += ",readonly"
		}
		spec.Mounts = append(spec.Mounts, entry)
		mountPairs = append(mountPairs, m.HostPath+":"+m.ContainerPath)
		// Record the iterion home only if it SURVIVED the filter —
		// collectHostStateMounts drops a candidate that overlaps the workspace
		// bind, and a consumer that assumed the mount from `host_state: auto`
		// alone would name a path the container cannot read.
		if m.HostPath == iterionHomeDir {
			sharedStateDir = m.ContainerPath
		}
	}

	// Warm the Go build + module caches across runs. Each fresh worktree
	// otherwise starts with an empty $HOME/.cache/go-build + $HOME/go/pkg/mod
	// (the host_state HOME tmpfs is per-run), so the first `go build` is a
	// full cold compile — AND it re-downloads the go1.26 toolchain, which
	// lives at $HOME/go/pkg/mod/golang.org/toolchain@…. Observed stalling
	// dispatched feature_dev/improve-loop runs for minutes in `act`. Bind
	// the host's caches read-write (Go's build + module caches are
	// concurrency-safe, so parallel runs sharing them is fine) at the same
	// absolute path; they nest on the tmpfs HOME like the .iterion/.claude
	// binds. Gated under host_state (off for host_state=none / cloud), and
	// best-effort — a missing cache dir is simply skipped (cold, as before).
	// Same rationale for the Node package caches: without them every
	// sandboxed run re-downloads its packages over the network (session
	// mining showed agents burning whole turns on cold `pnpm install`
	// failures — the one sandbox-provisioning complaint still live in
	// 2026-07). npm/pnpm/yarn caches are content-addressed and safe to
	// share; same host_state gate + best-effort skip when absent.
	if homeDir != "" {
		for _, rel := range []string{
			".cache/go-build", "go/pkg/mod", // Go build + module caches
			".npm",              // npm cache (_cacache)
			".local/share/pnpm", // pnpm home: store + global bins
			".cache/yarn",       // yarn cache
		} {
			p := filepath.Join(homeDir, rel)
			if fi, err := os.Stat(p); err == nil && fi.IsDir() {
				spec.Mounts = append(spec.Mounts, fmt.Sprintf("source=%s,target=%s,type=bind", p, p))
				mountPairs = append(mountPairs, p+":"+p)
			}
		}
	}

	// Force HOME inside the container to the host home path so
	// processes that resolve `~` (Claude Code, git, anything reading
	// $HOME) land in the mounted tree rather than a stock image's /root
	// or empty $HOME.
	if homeDir != "" {
		if spec.Env == nil {
			spec.Env = map[string]string{}
		}
		if _, alreadySet := spec.Env["HOME"]; !alreadySet {
			spec.Env["HOME"] = homeDir
		}
		// Docker creates the forced-HOME path as a root-owned mount parent.
		// Lay a uid-owned writable tmpfs at the home path so processes can
		// write under $HOME; the state binds nest on top and still persist
		// to the host. host_state is Linux + docker only (k8s rejects it),
		// and host-UID alignment is the host's own UID, so the tmpfs uid
		// matches the effective container user.
		//
		// A writable $HOME alone is NOT enough. Docker/runc creates a bind
		// mount's missing PARENT dirs as root:root — so the Go-cache binds
		// nested under HOME ($HOME/.cache/go-build, $HOME/go/pkg/mod) leave
		// $HOME/.cache and $HOME/go root-owned, shadowing the writable tmpfs
		// for their other children. devbox then can't `mkdir
		// $HOME/.cache/devbox` and dies before the build — the exact reason
		// devbox wasn't first-class in the sandbox. So we also lay a
		// user-owned tmpfs at the top-level-under-HOME component of every
		// nested bind (.cache, go), making the whole $HOME subtree writable
		// for devbox/npm/pip/go (incl. $HOME/go/bin) while the deeper binds
		// still overlay and persist. Direct $HOME/* binds (.claude,
		// .iterion, …) need nothing extra — their parent is $HOME itself.
		if goruntime.GOOS == "linux" {
			if uid := os.Getuid(); uid != 0 {
				gid := os.Getgid()
				// `exec` is REQUIRED. docker mounts --tmpfs noexec by
				// default, which blocks anything executable that lands in
				// $HOME — most importantly go's auto-downloaded toolchain
				// ($HOME/go/pkg/mod/golang.org/toolchain@.../bin/go), but
				// also nix/npm/pip binaries cached under HOME. Without it a
				// sandboxed `go build` dies with "cannot execute" and the
				// agent is forced to hand-relocate GOPATH/GOCACHE to /tmp.
				dirs := append([]string{homeDir}, homeNestedBindParents(homeDir, spec.Mounts)...)
				for _, dir := range dirs {
					spec.Tmpfs = append(spec.Tmpfs,
						fmt.Sprintf("%s:uid=%d,gid=%d,mode=0755,exec", dir, uid, gid))
				}
			}
		}
	}

	// Disable git commit signing inside the container. The mounted
	// ~/.gitconfig gives bots the operator's user.name/email so
	// `git commit` knows who is committing, but the host's
	// commit.gpgsign=true would also force signing — and the host's
	// gpg-agent socket can't cross the container boundary, so
	// signing fails with "gpg: can't create directory" /
	// "no secret key". Set git's CONFIG_KEY env override (git ≥
	// 2.31) to layer a `commit.gpgsign=false` on top of the mounted
	// gitconfig, scoped to this run. Operators who want signed bot
	// commits can amend post-finalize. spec.Env only gets these
	// when the slot is free so workflow authors can override.
	if spec.Env == nil {
		spec.Env = map[string]string{}
	}
	if _, set := spec.Env["GIT_CONFIG_COUNT"]; !set {
		spec.Env["GIT_CONFIG_COUNT"] = "1"
		spec.Env["GIT_CONFIG_KEY_0"] = "commit.gpgsign"
		spec.Env["GIT_CONFIG_VALUE_0"] = "false"
	}

	applyHostUIDRemap(spec, emitEvent, logger)

	_ = emitEvent(store.EventSandboxHostStateMounted, map[string]any{
		"enabled":          true,
		"source":           hsSource,
		"workspace_folder": spec.WorkspaceFolder,
		"mounts":           mountPairs,
	})
	return sharedStateDir
}

// homeNestedBindParents returns, for every bind mount in mounts whose
// target is strictly nested under homeDir (i.e. $HOME/<top>/<more…>),
// every strict ancestor dir between $HOME (exclusive) and the target
// (exclusive) — e.g. a $HOME/.local/share/pnpm bind yields $HOME/.local
// AND $HOME/.local/share. Docker/runc creates these intermediate parents
// as root:root, so they must each be re-laid as user-owned tmpfs for
// devbox/npm/pip/go to create siblings next to the bind (e.g.
// $HOME/.cache/devbox alongside a $HOME/.cache/go-build bind,
// $HOME/.local/share/fnm alongside a $HOME/.local/share/pnpm bind, or
// $HOME/go/bin alongside the $HOME/go/pkg/mod bind). Direct children
// ($HOME/<x>) are excluded — their parent is $HOME itself, already a
// user-owned tmpfs. Results are deduped and ordered shallowest-first so
// the tmpfs layers stack in mount order.
func homeNestedBindParents(homeDir string, mounts []string) []string {
	if homeDir == "" {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, m := range mounts {
		target := mountTarget(m)
		if target == "" {
			continue
		}
		rel, err := filepath.Rel(homeDir, target)
		if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
			continue // not under homeDir
		}
		parts := strings.Split(rel, string(filepath.Separator))
		if len(parts) < 2 {
			continue // direct child of $HOME — parent is $HOME itself
		}
		for depth := 1; depth < len(parts); depth++ {
			ancestor := filepath.Join(append([]string{homeDir}, parts[:depth]...)...)
			if !seen[ancestor] {
				seen[ancestor] = true
				out = append(out, ancestor)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		di := strings.Count(out[i], string(filepath.Separator))
		dj := strings.Count(out[j], string(filepath.Separator))
		if di != dj {
			return di < dj
		}
		return out[i] < out[j]
	})
	return out
}

// mountTarget extracts the target= path from a docker --mount spec entry
// of the form "source=…,target=…,type=bind[,readonly]". Returns "" when
// the entry carries no target= field.
func mountTarget(entry string) string {
	for _, field := range strings.Split(entry, ",") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(field), "target="); ok {
			return v
		}
	}
	return ""
}

// applyHostUIDRemap enforces the "container UID == host UID" invariant
// on Linux hosts so writes to the bind-mounted ~/.iterion and ~/.claude
// trees stay host-owned. macOS / Windows Docker Desktop perform their
// own userns-remap implicitly, so this is a no-op there. Host UID 0
// (CI runners) is also a no-op — same UID either way.
func applyHostUIDRemap(
	spec *sandbox.Spec,
	emitEvent func(store.EventType, map[string]any) error,
	logger *iterlog.Logger,
) {
	if goruntime.GOOS != "linux" {
		return
	}
	hostUID := os.Getuid()
	hostGID := os.Getgid()
	if hostUID == 0 {
		return
	}
	if spec.User == "" {
		spec.User = strconv.Itoa(hostUID) + ":" + strconv.Itoa(hostGID)
		_ = emitEvent(store.EventSandboxUserRemap, map[string]any{
			"uid":    hostUID,
			"gid":    hostGID,
			"reason": "host_state=auto: align container UID with host so writes to ~/.iterion + ~/.claude remain host-owned",
		})
		return
	}
	specUID, ok := parseUserUID(spec.User)
	if ok && specUID != hostUID {
		_ = emitEvent(store.EventSandboxUIDMismatchWarning, map[string]any{
			"spec_user": spec.User,
			"host_uid":  hostUID,
		})
		if logger != nil {
			logger.Warn("runtime: sandbox host_state active but container user %q (UID %d) != host UID %d — writes to ~/.iterion + ~/.claude will be owned by UID %d and may be unreadable to subsequent host invocations",
				spec.User, specUID, hostUID, specUID)
		}
	} else if !ok && logger != nil {
		// Non-numeric user (e.g. "node") — we can't verify UID alignment
		// without inspecting the image. Surface so the operator at least
		// knows host_state can corrupt home-dir permissions if the
		// image's user doesn't resolve to the host UID at runtime.
		logger.Warn("runtime: sandbox host_state active but container user %q has no parseable UID — cannot verify host UID alignment; ~/.iterion + ~/.claude writes may end up with unexpected ownership",
			spec.User)
	}
}

// addClawBinaryMount bind-mounts a host iterion binary into the
// container at /usr/local/bin/iterion when the workflow uses claw
// nodes — the runner subprocess (`iterion __claw-runner`) must be on
// the container PATH. Production sandbox images bake iterion in, so
// the silent skip when no host binary can be located is intentional;
// the absence will then surface as a clear "exec: iterion: not found"
// at runner invocation time.
func addClawBinaryMount(spec *sandbox.Spec, wf *ir.Workflow, resolver effectiveBackendResolver) {
	if wf == nil || !containsClawNode(wf, resolver) {
		return
	}
	hostBin := locateHostIterionBinary()
	if hostBin == "" {
		return
	}
	spec.Mounts = append(spec.Mounts,
		fmt.Sprintf("source=%s,target=/usr/local/bin/iterion,type=bind,readonly", hostBin),
	)
}

// addRewriterMounts bind-mounts each enabled rewriter plugin's host binary (the
// optional command-output compressors, rtk by default) into the container at
// the container path the rewriter declares (sandbox_mount:, e.g.
// /usr/local/bin/rtk) whenever the binary is found on the host. When a node has
// compression enabled, the rewrite *decision* runs host-side (claude_code hook
// / tool node) but the rewritten command *executes* inside the container, and
// the sandboxed claw runner decides AND executes in-container — both need the
// rewriter binary on the container PATH. Mounting unconditionally-when-present
// keeps the host decision and the in-container execution from ever disagreeing;
// an unused read-only mount is negligible. Production images may bake the binary
// in instead (the host then has none → no-op). A host of a different arch than
// the container is the operator's responsibility (use an image with the binary
// baked in).
func addRewriterMounts(spec *sandbox.Spec) {
	reg, err := plugin.Load()
	if err != nil {
		return
	}
	for _, m := range rewrite.NewChain(specsOf(reg.EnabledRewriters())).SandboxMounts() {
		spec.Mounts = append(spec.Mounts,
			fmt.Sprintf("source=%s,target=%s,type=bind,readonly", m.HostPath, m.ContainerPath),
		)
	}
}

// specsOf flattens rewriter contributions to their specs (chain input).
func specsOf(contribs []plugin.RewriterContribution) []plugin.RewriterSpec {
	out := make([]plugin.RewriterSpec, 0, len(contribs))
	for _, c := range contribs {
		out = append(out, c.Spec)
	}
	return out
}

// dropHostBindMounts removes type=bind entries from a sandbox spec's mount list,
// returning the kept (non-bind) mounts. resolveAndStartSandbox calls it when the
// selected driver has no host filesystem (Capabilities.SupportsHostBindMounts is
// false — i.e. kubernetes). A bot's docker-authored bind mount (e.g. a ~/.claude
// OAuth mount in its sandbox block) is meaningless there and would otherwise
// hard-fail manifest building (the kubernetes driver rejects type=bind), so we
// warn and drop it; the sandboxed agent gets its creds/config from env instead.
// Non-bind mounts (pvc/configmap/secret) pass through untouched.
func dropHostBindMounts(mounts []string, logger *iterlog.Logger) []string {
	if len(mounts) == 0 {
		return mounts
	}
	kept := make([]string, 0, len(mounts))
	for _, m := range mounts {
		if mountIsHostBind(m) {
			if logger != nil {
				logger.Warn("runtime: sandbox: dropping host bind mount %q — this driver has no host filesystem (type=bind unsupported); the sandboxed agent must get its creds/config from env or a pvc/configmap/secret mount", m)
			}
			continue
		}
		kept = append(kept, m)
	}
	return kept
}

// runFilesEnvVar tells in-sandbox tool scripts where the run-files bind
// landed (SandboxParams.RunFilesHostDir). It is set only when that bind was
// added, and withdrawn with it: the variable is a promise that the directory
// exists, never a hint to try.
const runFilesEnvVar = "ITERION_ARTIFACT_FILES_DIR"

// finalizeMountsForDriver drops every host bind mount the selected driver
// cannot honour, and with them the promises the runtime made on their
// behalf: the run-files variable (dropUnsupportedHostBinds) and the
// attachments path handed to nodes, returned empty when its bind is gone —
// nodes are then given the host path, which at least fails loudly instead of
// resolving to an empty mount point. A driver with a host filesystem keeps
// everything.
func finalizeMountsForDriver(spec *sandbox.Spec, caps sandbox.Capabilities, attachmentsDir, botRunFilesDir string, logger *iterlog.Logger) string {
	if caps.SupportsHostBindMounts {
		return attachmentsDir
	}
	dropUnsupportedHostBinds(spec, botRunFilesDir, logger)
	return ""
}

// dropUnsupportedHostBinds strips every host bind mount from spec — the
// selected driver has no host filesystem to honour them — and withdraws the
// run-files variable when the directory it names went with them. Measured on
// the pod backend: the variable named a directory the pod never had, a gate
// wrapper redirecting its report into it died on "Directory nonexistent"
// before the oracle ran, and four lots read an oracle verdict out of an
// environment failure. The rule is keyed on the mounts, not on who set the
// variable: a target a dropped bind served is gone unless a mount the driver
// honours (pvc, configmap, secret) still serves it, and a value naming a path
// no bind ever served is a promise the runtime never made. botRunFilesDir is
// the value the runtime overwrote when it added its bind: handed back when
// the runtime's promise is withdrawn, unless it names a dropped target too.
func dropUnsupportedHostBinds(spec *sandbox.Spec, botRunFilesDir string, logger *iterlog.Logger) {
	// Targets compare cleaned: a trailing slash or a spaced field is the same
	// directory, and a miss here recreates the incident this rule closes.
	target := func(m string) string {
		if t := mountTarget(m); t != "" {
			return path.Clean(t)
		}
		return ""
	}
	gone := map[string]bool{}
	for _, m := range spec.Mounts {
		if mountIsHostBind(m) {
			if t := target(m); t != "" {
				gone[t] = true
			}
		}
	}
	spec.Mounts = dropHostBindMounts(spec.Mounts, logger)
	for _, m := range spec.Mounts {
		delete(gone, target(m))
	}
	// A value goes when it names a dropped target OR a path under one: a
	// directory under a bind that is gone is gone with it.
	underDropped := func(dir string) bool {
		value := path.Clean(dir)
		for t := range gone {
			if value == t || strings.HasPrefix(value, t+"/") {
				return true
			}
		}
		return false
	}
	dir, set := spec.Env[runFilesEnvVar]
	if !set || !underDropped(dir) {
		return
	}
	if botRunFilesDir != "" && !underDropped(botRunFilesDir) {
		spec.Env[runFilesEnvVar] = botRunFilesDir
		if logger != nil {
			logger.Warn("runtime: sandbox: %s handed back to the bot's own value %s — the runtime's bind serving %s was dropped with the other host binds", runFilesEnvVar, botRunFilesDir, dir)
		}
		return
	}
	delete(spec.Env, runFilesEnvVar)
	if logger != nil {
		logger.Warn("runtime: sandbox: %s withdrawn — the bind serving %s was dropped with the other host binds; tools writing run files fall back to a temp dir", runFilesEnvVar, dir)
	}
}

// mountIsHostBind reports whether a docker-style mount string is a host bind
// mount: an explicit type=bind, or — docker's default — a source= with no
// explicit type. pvc/configmap/secret types are never host binds.
func mountIsHostBind(mount string) bool {
	var typ string
	hasSource := false
	for _, part := range strings.Split(mount, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch strings.TrimSpace(kv[0]) {
		case "type":
			typ = strings.TrimSpace(kv[1])
		case "source", "src":
			hasSource = true
		}
	}
	if typ != "" {
		return typ == "bind"
	}
	return hasSource
}

// addWorktreeGitMount bind-mounts the source repo's `.git` directory
// into the container at the SAME host path when a worktree is active,
// so the worktree's `.git` pointer file — a one-line
// `gitdir: <repoRoot>/.git/worktrees/<run-id>` — resolves correctly
// inside the sandbox and every nested reference it walks (objects,
// refs, HEAD, packed-refs, config) is also reachable.
//
// gitDir is the per-run worktree gitdir (`<repoRoot>/.git/worktrees/<run-id>`)
// computed by [resolveWorktreeGitDir]. We derive the parent `.git`
// directory and mount that whole tree because git resolves the
// worktree's pointer file by:
//  1. reading `<repoRoot>/.git/worktrees/<run-id>/commondir` → `..`
//  2. resolving the relative path → `<repoRoot>/.git/` (shared objects
//     + refs + config).
//
// Mounting only the per-run subtree leaves git unable to find shared
// objects, refs/, HEAD, packed-refs — and every command fails with
// `fatal: not a git repository`. The TODO below tracks a tighter
// per-run isolation design (whole `.git` read-only + per-run subtree
// read-write overlay), which docker bind layering technically allows
// but is risky to ship without a test matrix proving the override
// semantics on every supported driver. For now: simple + correct.
//
// Read-write because git writes HEAD, refs, packed-refs, index, etc.
// into the gitdir during normal `git commit` / `git checkout` flows,
// and the worktree's gitdir is part of the broader `.git` tree.
//
// Silently skips when:
//   - gitDir is empty (non-worktree run, or cloud runner that never
//     populated the worktreeContext);
//   - the path doesn't exist on disk (e.g. an inconsistent on-resume
//     state where the worktree was manually removed but the run
//     record still claims worktree=true).
//
// The kubernetes driver hard-errors on `type=bind` at manifest render
// time (see pkg/sandbox/kubernetes/mounts.go) — that surfaces as a
// clear "type=bind not supported in cloud" diagnostic at run start,
// which is the right behaviour for cloud runners that lack a host
// filesystem to bind. Cloud runs don't need this mount: the kubernetes
// driver tar-copies the CLONE ROOT (real `.git`, incl. the credential
// store) into the pod at the workspace path, re-anchors the copied
// clone's git plumbing in-pod, and the runner writes rotated git
// credentials through via [sandbox.WorkspaceFileRefresher] — see
// pkg/sandbox/kubernetes/driver.go (populateWorkspace /
// fixupWorkspaceGit) and ADR-082 Phase 3.
func addWorktreeGitMount(spec *sandbox.Spec, gitDir string, logger *iterlog.Logger) {
	if gitDir == "" {
		return
	}
	// Walk up two levels: <repoRoot>/.git/worktrees/<run-id> → <repoRoot>/.git
	dotGit := filepath.Dir(filepath.Dir(gitDir))
	info, statErr := os.Stat(dotGit)
	if statErr != nil {
		if !errors.Is(statErr, fs.ErrNotExist) && logger != nil {
			logger.Warn("runtime: sandbox repo .git %s: %v — skipping mount", dotGit, statErr)
		}
		return
	}
	if !info.IsDir() {
		return
	}
	spec.Mounts = append(spec.Mounts,
		fmt.Sprintf("source=%s,target=%s,type=bind", dotGit, dotGit),
	)
}

// seedDefaultLocale gives the sandbox a UTF-8 capable locale when the image
// declares none — which the shipped images do not.
//
// An unset locale is not neutral: it IS the C/POSIX locale, and the JVM then
// derives `sun.jnu.encoding` from it and decodes filenames as ASCII. A build
// whose resources carry an accented name fails on "Problems opening file input
// stream", naming a file that is plainly there. The same class of surprise
// reaches anything that reads bytes as text — the symptom never resembles the
// cause, and the cause is an environment variable nobody set.
//
// `C.UTF-8` is chosen for what it does NOT do: it keeps C's collation and
// number/date formatting, so sort order stays byte-order and no output changes
// shape. Only the encoding moves. Picking `en_US.UTF-8` (or any language)
// would silently change how programs sort and format — an environment
// property leaking into results.
//
// LANG only, never LC_ALL: LC_ALL overrides every category and would take
// precedence over a repository or a workflow that sets its own. This is a
// default to fall back on, not a decision to impose — and it steps aside
// entirely as soon as either variable is already set.
func seedDefaultLocale(spec *sandbox.Spec) {
	if spec.Env == nil {
		spec.Env = map[string]string{}
	}
	if _, set := spec.Env["LANG"]; set {
		return
	}
	if _, set := spec.Env["LC_ALL"]; set {
		return
	}
	spec.Env["LANG"] = "C.UTF-8"
}
