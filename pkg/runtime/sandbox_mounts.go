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
	"github.com/SocialGouv/iterion/pkg/treenoise"
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
// container), an image pinning a UID that is not the host's — the
// EACCES case the container-local path exists to serve — or an iterion
// home that is not the one the operator chose (operatorsIterionHome).
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
	// The scratch dir lives under the iterion home, so it is the operator's
	// only when that home is: under a home a project `.env` chose, the
	// repository decides the bind source — symlinks included.
	if _, notMounted := operatorsIterionHome(); notMounted != "" {
		return skip("the iterion home is not the one the operator chose")
	}
	base := repoRoot
	if base == "" {
		base = workDir
	}
	hostScratch := memory.WorkspaceScratchDir(base)
	if hostScratch == "" {
		return skip("no project scratch dir could be resolved")
	}
	// The scratch lives under the read-write iterion home, so a previous run
	// could have replaced a component of this path with a symlink — and this
	// bind is the one whose own comment names the threat ("the repository
	// decides the bind source — symlinks included"). The same one check every
	// such source passes, and a failure is this function's own loud skip.
	iterionHome, _ := operatorsIterionHome()
	if err := ensureRealBindSourceDir(iterionHome, hostScratch, 0o700); err != nil {
		return skip(err.Error())
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
// guard (#2069) shadows the host configuration the HOST later executes —
// plugins.yaml + plugins/, ~/.claude/settings*.json + plugins/,
// ~/.claude.json, ~/.codex/config.toml, ~/.gitconfig — with per-run copies
// or read-only overlays, and its audit list rides the event's `guarded`
// field. Nil disables the shadowing (the ITERION_SANDBOX_HOST_STATE_GUARD=off
// posture; tests construct their own).
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
	guard *hostConfigGuard,
	emitEvent func(store.EventType, map[string]any) error,
	logger *iterlog.Logger,
) (sharedStateDir string) {
	resolvedHostState, hsSource := pickHostState(workflowHostState(wf), p.HostStateOverride, p.HostStateDefault)
	spec.HostState = sandbox.HostState(resolvedHostState)
	if !spec.HostState.Active() {
		seedGitIdentityEnv(spec, p.WorkspacePath, logger)
		// The worktree .git bind is not host_state-gated, so its #2069
		// shadows apply here too — and the one audit event carries them.
		guard.guardWorktreeGit(spec, p.WorktreeGitDir)
		disabled := map[string]any{
			"enabled": false,
			"source":  hsSource,
		}
		if guarded := guard.audit(); len(guarded) > 0 {
			disabled["guarded"] = guarded
		}
		_ = emitEvent(store.EventSandboxHostStateMounted, disabled)
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

	homeDir, homePlanted := resolveHostHomeDir()
	if homePlanted {
		logger.Warn("runtime: %s was set by a project .env, not by the operator — the host state under it (~/.claude, ~/.codex, ~/.gitconfig, caches) is not mounted into the sandbox", store.HomeEnvName())
	}
	iterionHomeDir, iterionHomeNotMounted := operatorsIterionHome()
	if iterionHomeNotMounted != "" {
		logger.Warn("runtime: the iterion home %s is not the one the operator chose — a project .env set it, or it is the shared <tmp> fallback — so it is not mounted into the sandbox", iterionHomeNotMounted)
	}
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

	// #2069: the binds above give the sandbox the host state it must WRITE
	// (the run store, Claude Code sessions and credential refresh, codex's
	// auth.json). What follows shadows the host CONFIGURATION the host later
	// executes — per-run copies for files in-sandbox tools legitimately
	// write, read-only overlays for what they never do. Only the SURVIVING
	// binds are guarded: a candidate the filter dropped (missing on the
	// host, or covered by the workspace bind) exposes nothing.
	if guard != nil {
		// The iterion home stays READ-WRITE — the run store, the memory tree
		// and the project scratch live under it and must outlive the run — so
		// it is a root whose content the sandbox writes, and every bind source
		// below it is checked against a symlink the sandbox planted there
		// (sandbox_bind_source.go). Registering the root is what makes the
		// check reach all of them; the ~/.claude and ~/.codex trees need no
		// registration because nothing under them is written any more.
		guard.sandboxWrites(iterionHomeDir)
		mounted := map[string]bool{}
		for _, m := range mounts {
			mounted[m.HostPath] = true
		}
		if mounted[gitConfigPath] {
			mounts = guard.copyFileOver(mounts, gitConfigPath, "")
		}
		if mounted[claudeConfigPath] {
			mounts = guard.copyFileOver(mounts, claudeConfigPath, "{}\n")
		}
		if mounted[claudeDir] {
			// READ-ONLY BY DEFAULT, through an ephemeral home: the read-write
			// bind of ~/.claude is replaced by a per-run tmpfs, and what the
			// operator's own CLI executes or loads is bound back on top,
			// read-only. The in-sandbox CLI writes its home as freely as ever
			// — in the tmpfs — and a path nobody thought of fails nowhere: it
			// is simply absent, which is the shape the CLI meets on a fresh
			// machine, instead of writing the operator's home in silence. The
			// enumeration that remains is of the READ set, which is the
			// operator's own configuration and therefore knowable.
			mounts = guard.ephemeralDirOver(mounts, claudeDir)
			for _, entry := range hostExecutedClaudeHomeEntries {
				mounts = guard.roDirOverExisting(mounts, filepath.Join(claudeDir, entry))
			}
			// The settings files and the memory file are read AND written in
			// place by the CLI (a permission "always allow" lands in
			// settings.local.json), so they are per-run copies rather than
			// read-only binds: the write succeeds and dies with the run. Not
			// seeded — under the tmpfs an absent file is absent in the
			// container too, so there is nothing to make unplantable, and a
			// file iterion created in the operator's home would stay there.
			mounts = guard.copyFileOverExisting(mounts, filepath.Join(claudeDir, "settings.json"))
			mounts = guard.copyFileOverExisting(mounts, filepath.Join(claudeDir, "settings.local.json"))
			mounts = guard.copyFileOverExisting(mounts, filepath.Join(claudeDir, "CLAUDE.md"))
			// The two exceptions, each with the HOST-side iterion process that
			// needs the same tree:
			//   - projects/ holds the CLI's session transcripts, and
			//     `session: persist` packs them from the host path after the
			//     run (delegate.SessionFilesRoot -> $HOME/.claude, read by
			//     sessionpack.CollectBySessionID in
			//     pkg/backend/model/executor_persist.go). Ephemeral, every
			//     persisted session would silently restart from scratch;
			//   - .credentials.json is the forfait OAuth token the CLI reads
			//     and refreshes. Ephemeral, a sandboxed claude_code node on a
			//     forfait has no credential at all.
			mounts = guard.rwDirOver(mounts, filepath.Join(claudeDir, "projects"), true)
			mounts = guard.rwFileOverExisting(mounts, filepath.Join(claudeDir, ".credentials.json"))
		}
		if mounted[codexDir] {
			// Same posture for CODEX_HOME, and it closes the same defect one
			// level down: config.toml's `notify = [...]` and
			// `mcp_servers.<x>.command` NAME a program, and shadowing the
			// pointer while leaving ~/.codex/notify.sh writable shadowed
			// nothing. Under the ephemeral home the named program lands in
			// the tmpfs whatever its path.
			mounts = guard.ephemeralDirOver(mounts, codexDir)
			mounts = guard.copyFileOverExisting(mounts, filepath.Join(codexDir, "config.toml"))
			// The instruction files codex reads from CODEX_HOME whatever the
			// working directory (delegate.codexInstructionFiles is the same
			// list, read by the ambient-context policy). Read-only, and never
			// created: an AGENTS.override.md iterion seeds into a home that
			// had none is a first-wins shadow of the operator's own
			// AGENTS.md, forever (docs/adr/0119, docs/backends.md).
			mounts = guard.roDirOverExisting(mounts, filepath.Join(codexDir, "prompts"))
			for _, name := range []string{"AGENTS.md", "AGENTS.override.md"} {
				mounts = guard.roFileOverExisting(mounts, filepath.Join(codexDir, name))
			}
			// The exceptions, as above: auth.json is the "Sign in with
			// ChatGPT" token claw's OpenAI provider reads, and sessions/ is
			// what sessionpack packs for a codex `session: persist`.
			mounts = guard.rwFileOverExisting(mounts, filepath.Join(codexDir, "auth.json"))
			mounts = guard.rwDirOver(mounts, filepath.Join(codexDir, "sessions"), true)
		}
		if mounted[iterionHomeDir] {
			mounts = guard.copyFileOver(mounts, filepath.Join(iterionHomeDir, "plugins.yaml"), "")
			for _, entry := range hostLoadedIterionHomeEntries {
				mounts = guard.roDirOver(mounts, filepath.Join(iterionHomeDir, entry))
			}
			// The sandbox's own cache store, read-only AT ITS OWN PATH. The
			// run still writes the caches through the $HOME paths they are
			// bound at (mount options are per mountpoint), but it can no
			// longer rewrite the STORE — which is where a planted symlink
			// would choose the next run's bind sources.
			mounts = guard.roDirOver(mounts, SandboxCacheStoreDir(iterionHomeDir))
		}
	}

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
	// dispatched feature_dev/improve-loop runs for minutes in `act`. Same
	// rationale for the Node package caches: without them every sandboxed
	// run re-downloads its packages over the network (session mining showed
	// agents burning whole turns on cold `pnpm install` failures — the one
	// sandbox-provisioning complaint still live in 2026-07).
	//
	// The warmth is kept; the SOURCE is not the host's own cache (#2069).
	// Each of these is a store whose content the HOST runs: $PNPM_HOME holds
	// the global bins `pnpm setup` puts on the operator's PATH, the module
	// cache holds the toolchain binary GOTOOLCHAIN=auto executes, and the
	// build cache holds objects `go build` relinks without re-hashing
	// (verification is GODEBUG=gocacheverify=1, off by default). Bound at
	// the host's own path, read-write, a sandboxed agent writes what the
	// host runs next. So the sandbox gets its OWN caches under the iterion
	// home, bound at the same CONTAINER path (GOCACHE/GOMODCACHE/PNPM_HOME
	// resolve from $HOME, which is the host home path in-container):
	// persistent from one sandboxed run to the next, iterion-owned, and
	// never read by the host's own tools. Gated under host_state (off for
	// host_state=none / cloud) and on the operator's iterion home being
	// mounted — without it there is nowhere iterion owns to keep them, and
	// the run is cold, which is the pre-existing "cache absent" state.
	// With the guard off (the hatch, or a driver with no host binds) the
	// operator asked for the pre-guard posture, and that includes the host's
	// own caches at their own path — best-effort, skipped when absent.
	// A container pinning a UID that is not the host's gets NO cache: the
	// store is created 0700 and owned by the host user, so a foreign UID
	// cannot even read it and `go build` would die on permission denied with
	// no iterion event to explain it. That is the same condition — and the
	// same answer — as applyScratchMount's, which skips its bind rather than
	// handing over a directory the container cannot use.
	foreignUID := -1
	if uid, ok := parseUserUID(spec.User); ok && uid != os.Getuid() {
		foreignUID = uid
	}
	if homeDir != "" {
		for _, rel := range SandboxCacheRelPaths() {
			target := filepath.Join(homeDir, filepath.FromSlash(rel))
			source := target
			switch {
			case guard == nil:
				if fi, err := os.Stat(source); err != nil || !fi.IsDir() {
					continue
				}
			case iterionHomeDir == "":
				// No home iterion owns (a `.env` chose it, or it is the
				// shared <tmp> fallback — already warned above): the run is
				// cold rather than warmed from a store the host executes.
				continue
			case foreignUID >= 0:
				logger.Warn("runtime: the container pins UID %d and the host is %d, so the sandbox's own cache store (%s, owner-only) would be unreadable inside it — this run builds cold instead", foreignUID, os.Getuid(), SandboxCacheStoreDir(iterionHomeDir))
				continue
			default:
				source = filepath.Join(SandboxCacheStoreDir(iterionHomeDir), filepath.FromSlash(rel))
				// The store lives under the read-write iterion home, so the
				// RUN BEFORE THIS ONE could have replaced this path with a
				// symlink to any host path — the one check every such bind
				// source passes (sandbox_bind_source.go).
				if err := ensureRealBindSourceDir(SandboxCacheStoreDir(iterionHomeDir), source, 0o700); err != nil {
					logger.Warn("runtime: the sandbox's own cache %s is not the path it is named (%v) — this run builds cold, and nothing is mounted from it", source, err)
					continue
				}
				guard.note("private", target)
			}
			spec.Mounts = append(spec.Mounts, fmt.Sprintf("source=%s,target=%s,type=bind", source, target))
			mountPairs = append(mountPairs, source+":"+target)
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
		//
		// The decision itself is homeTmpfsEntries, so the posture can be
		// driven for a platform this process is not running on.
		uid, gid := -1, -1
		if goruntime.GOOS == "linux" {
			if u := os.Getuid(); u != 0 {
				uid, gid = u, os.Getgid()
			}
		}
		spec.Tmpfs = append(spec.Tmpfs, homeTmpfsEntries(
			homeDir, guard.ephemeralTmpfs(), homeNestedBindParents(homeDir, spec.Mounts), uid, gid)...)
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

	// The worktree .git shadows (#2069) are applied HERE — after the
	// HOME-tmpfs layering above, which a mount under $HOME must never feed:
	// homeNestedBindParents would lay a tmpfs at the workspace bind's own
	// destination for the overlay's ancestors, and a tmpfs shadows a bind
	// at the same destination in either argument order. Docker mounts
	// nested binds parents-first by destination depth, so these overlays
	// win over the tree bind addWorktreeGitMount appends later.
	guard.guardWorktreeGit(spec, p.WorktreeGitDir)

	mounted := map[string]any{
		"enabled":          true,
		"source":           hsSource,
		"workspace_folder": spec.WorkspaceFolder,
		"mounts":           mountPairs,
	}
	// #2069: the host-config guard's audit — every host path shadowed by a
	// per-run copy or a read-only overlay, so `docker inspect` never shows
	// a host bind this event cannot explain.
	if guarded := guard.audit(); len(guarded) > 0 {
		mounted["guarded"] = guarded
	}
	if iterionHomeNotMounted != "" {
		mounted["iterion_home_not_mounted"] = iterionHomeNotMounted
	}
	if homePlanted {
		mounted["home_not_mounted"] = os.Getenv(store.HomeEnvName())
	}
	_ = emitEvent(store.EventSandboxHostStateMounted, mounted)
	return sharedStateDir
}

// operatorsIterionHome returns the iterion home host_state may bind-mount
// read-write into the sandbox: GlobalIterionDataDir, only when it is the home
// the operator chose (store.InheritedIterionDataDir). A home a project `.env`
// set, or the shared <tmp> fallback, would let a repository pick a host
// directory the sandboxed agent can write; it comes back as notMounted
// instead, for the audit event.
func operatorsIterionHome() (dir, notMounted string) {
	dir = store.GlobalIterionDataDir()
	if chosen := store.InheritedIterionDataDir(); chosen == "" || filepath.Clean(chosen) != filepath.Clean(dir) {
		return "", dir
	}
	return dir, ""
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
//
// An ancestor that is ITSELF a bind target is skipped: docker does not
// root-create what a mount provides, and a tmpfs there would SHADOW the
// bind — the #2069 config overlays ($HOME/.claude/settings.json over the
// $HOME/.claude bind) are exactly this shape, and laying the tmpfs lost
// the operator's whole ~/.claude for the run.
// homeTmpfsEntries is the `--tmpfs` plan for a guarded run's $HOME, as a
// decision separate from the platform it is taken on: uid >= 0 means the
// host-UID remap aligns an owner (Linux, non-root engine), uid < 0 that it
// does not and the image's own user owns what is laid.
//
// Two stories ride one mount list, and only one of them is conditional:
//
//   - the WRITABLE-$HOME layering (homeDir itself and nestedParents) exists
//     to re-lay as user-owned what docker/runc root-creates, so it has
//     nothing to say where no uid is being aligned;
//   - the guard's EPHEMERAL state dirs are laid either way, and that is the
//     whole point of them. ephemeralDirOver dropped the read-write bind of
//     ~/.claude and ~/.codex, so this tmpfs is where the in-sandbox CLI
//     writes its home. Skip it and docker root-creates the mount parent of
//     the read-only overlays nested under them, which an image whose user is
//     not root cannot write — the operator's home would be safe and the CLI
//     would be broken.
//
// Deduped because nestedParents names the same ancestor for every nested bind
// under it, and because the guard's dirs and those ancestors overlap by
// construction (~/.claude is the parent of the projects/ bind): two --tmpfs at
// one destination is a mount docker cannot order.
func homeTmpfsEntries(homeDir string, ephemeral, nestedParents []string, uid, gid int) []string {
	if homeDir == "" {
		return nil
	}
	opts := "mode=0755,exec"
	if uid >= 0 {
		opts = fmt.Sprintf("uid=%d,gid=%d,mode=0755,exec", uid, gid)
	}
	dirs := make([]string, 0, 2+len(ephemeral)+len(nestedParents))
	if uid >= 0 {
		dirs = append(dirs, homeDir)
	}
	dirs = append(dirs, ephemeral...)
	if uid >= 0 {
		dirs = append(dirs, nestedParents...)
	}
	out := make([]string, 0, len(dirs))
	seen := map[string]bool{}
	for _, dir := range dirs {
		if dir == "" || seen[dir] {
			continue
		}
		seen[dir] = true
		out = append(out, dir+":"+opts)
	}
	return out
}

func homeNestedBindParents(homeDir string, mounts []string) []string {
	if homeDir == "" {
		return nil
	}
	targets := map[string]bool{}
	for _, m := range mounts {
		if t := mountTarget(m); t != "" {
			targets[t] = true
		}
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
			if targets[ancestor] {
				continue // a bind already provides it — never shadow it
			}
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
		field = strings.TrimSpace(field)
		// docker spells the target three ways; a bind dropped under one of
		// them is dropped all the same.
		for _, key := range []string{"target=", "dst=", "destination="} {
			if v, ok := strings.CutPrefix(field, key); ok {
				return v
			}
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
		fmt.Sprintf("source=%s,target=%s,type=bind,readonly", hostBin, sandbox.EngineBinaryContainerPath),
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
// attachments path handed to nodes, returned empty when its bind is gone.
// What went is returned as a report for the run's stream
// (store.EventSandboxHostBindsDropped); the caller decides what an empty
// attachments path means for a run that holds attachments. A driver with a
// host filesystem keeps everything and reports nothing.
func finalizeMountsForDriver(spec *sandbox.Spec, caps sandbox.Capabilities, attachmentsDir, botRunFilesDir string, logger *iterlog.Logger) (string, droppedBinds) {
	if caps.SupportsHostBindMounts {
		return attachmentsDir, droppedBinds{}
	}
	return "", dropUnsupportedHostBinds(spec, botRunFilesDir, logger)
}

// droppedBinds is what dropUnsupportedHostBinds took away from a spec: the
// container targets of the host binds it removed and the promises it
// withdrew with them. It is the record the run's stream carries, so a reader
// of the run — not only of the runner's log — sees what the container never
// had.
type droppedBinds struct {
	Targets    []string          // cleaned container targets of the dropped host binds, sorted
	Withdrawn  []string          // variables withdrawn because their bind went
	HandedBack map[string]string // variable → the bot's own value handed back in the runtime's place
	Carried    []string          // targets a seed serves in the dropped bind's place (the attachments on a copying driver)
}

func (d droppedBinds) empty() bool {
	return len(d.Targets) == 0 && len(d.Withdrawn) == 0 && len(d.HandedBack) == 0 && len(d.Carried) == 0
}

// carry moves target from the dropped list to the carried one: the bind
// went, but a seed lands the same directory at the same path, so the
// container does have it.
func (d droppedBinds) carry(target string) droppedBinds {
	clean := path.Clean(target)
	kept := make([]string, 0, len(d.Targets))
	for _, t := range d.Targets {
		if t == clean {
			d.Carried = append(d.Carried, t)
			continue
		}
		kept = append(kept, t)
	}
	d.Targets = kept
	return d
}

// payload is the event data of store.EventSandboxHostBindsDropped.
func (d droppedBinds) payload(driver string) map[string]any {
	out := map[string]any{
		"driver": driver,
		"mounts": append([]string{}, d.Targets...),
	}
	if len(d.Withdrawn) > 0 {
		out["withdrawn"] = append([]string{}, d.Withdrawn...)
	}
	if len(d.HandedBack) > 0 {
		handed := make(map[string]string, len(d.HandedBack))
		for k, v := range d.HandedBack {
			handed[k] = v
		}
		out["handed_back"] = handed
	}
	if len(d.Carried) > 0 {
		out["carried"] = append([]string{}, d.Carried...)
	}
	return out
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
// The returned report lists what went: every dropped bind's target, and the
// variable withdrawn or handed back.
func dropUnsupportedHostBinds(spec *sandbox.Spec, botRunFilesDir string, logger *iterlog.Logger) droppedBinds {
	var report droppedBinds
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
				report.Targets = append(report.Targets, t)
			}
		}
	}
	sort.Strings(report.Targets)
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
		return report
	}
	report.Withdrawn = append(report.Withdrawn, runFilesEnvVar)
	if botRunFilesDir != "" && !underDropped(botRunFilesDir) {
		spec.Env[runFilesEnvVar] = botRunFilesDir
		report.HandedBack = map[string]string{runFilesEnvVar: botRunFilesDir}
		if logger != nil {
			logger.Warn("runtime: sandbox: %s handed back to the bot's own value %s — the runtime's bind serving %s was dropped with the other host binds", runFilesEnvVar, botRunFilesDir, dir)
		}
		return report
	}
	delete(spec.Env, runFilesEnvVar)
	if logger != nil {
		logger.Warn("runtime: sandbox: %s withdrawn — the bind serving %s was dropped with the other host binds; tools writing run files fall back to a temp dir", runFilesEnvVar, dir)
	}
	return report
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
// `fatal: not a git repository`. A tighter per-run isolation (whole `.git`
// read-only + per-run subtree read-write overlay) remains possible —
// docker bind layering allows it — but still wants a test matrix proving
// the override semantics on every supported driver. #2069 closed the
// host-EXECUTION half of it below (config + hooks shadowed); the residual
// is ref/object corruption, an availability concern, not host code
// execution.
//
// Read-write because git writes HEAD, refs, packed-refs, index, etc.
// into the gitdir during normal `git commit` / `git checkout` flows,
// and the worktree's gitdir is part of the broader `.git` tree.
//
// That read-write tree is also the #2069 surface on the REPOSITORY side:
// `.git/config` (core.hooksPath, core.fsmonitor, aliases, credential
// helpers) and `.git/hooks/` are configuration the operator's own git
// executes on its next host-side command in this checkout. The guard
// shadows both — from applyHostStateMounts (hostConfigGuard.
// guardWorktreeGit), NOT here: the overlays must not feed the HOME-tmpfs
// layering, which a mount added before applyHostStateMounts would (a
// tmpfs at a bind's destination shadows the bind, whatever the argument
// order — measured on docker 29). Docker mounts nested binds by
// destination depth, parents first, so the overlays win over this tree
// bind even though it is appended after them.
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
	dotGit := worktreeDotGit(gitDir)
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

// seedTreeNoiseEnv gives in-sandbox tool scripts the canonical tree-noise
// pathspecs (pkg/treenoise) so a scope gate filters the tree with the
// engine's list, not its own literal — the sandbox half of the host's
// executor env (#1464). Like seedDefaultLocale it steps aside when the
// operator or the workflow set the variable themselves.
func seedTreeNoiseEnv(spec *sandbox.Spec) {
	if spec.Env == nil {
		spec.Env = map[string]string{}
	}
	if value, set := spec.Env[treenoise.TreeNoiseEnvVar]; set && value != "" {
		return
	}
	// The operator's own exported environment is a claim too: one rule
	// everywhere — their list is honored in the container, never silently
	// replaced by the canonical one (verdict 3).
	if value, exported := os.LookupEnv(treenoise.TreeNoiseEnvVar); exported && value != "" {
		spec.Env[treenoise.TreeNoiseEnvVar] = value
		return
	}
	spec.Env[treenoise.TreeNoiseEnvVar] = treenoise.EnvValue()
}
