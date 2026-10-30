// Package runtime — the host-configuration guard of host_state (#2069).
//
// With `host_state: auto` (the default for local docker sandboxes) the
// sandbox bind-mounts the operator's host state — the iterion home,
// ~/.claude, ~/.claude.json, ~/.codex, ~/.gitconfig, the warm caches —
// and addWorktreeGitMount binds the repository's whole `.git`. Every one
// of those binds was read-WRITE, so an agent inside the sandbox — one
// steered by a prompt injection, say — could write configuration the HOST
// later executes, outside the sandbox:
//
//   - an iterion plugin planted under <home>/plugins/ and enabled in
//     <home>/plugins.yaml runs its MCP servers and rewriters on the
//     launcher (pkg/plugin, pkg/backend/mcp/origin.go);
//   - Claude Code hooks in ~/.claude/settings*.json, MCP servers in
//     ~/.claude.json, plugin hooks under ~/.claude/plugins/;
//   - ~/.codex/config.toml's MCP servers for the host's codex;
//   - ~/.gitconfig's aliases / core.hooksPath / core.fsmonitor, and
//     .git/config + .git/hooks/ of the operator's own checkout.
//
// The guard below is the ticket's "read-only overlays + per-run copies"
// design. What the sandbox genuinely needs to WRITE stays writable: the
// run store and memory under the iterion home, Claude Code's sessions and
// credential refresh under ~/.claude, codex's auth.json under ~/.codex,
// git's objects/refs/HEAD under .git. What the host EXECUTES or LOADS AS
// INSTRUCTIONS is shadowed:
//
//   - files the in-sandbox tools write in place (Claude Code persists
//     state into ~/.claude.json and settings.local.json) bind a PER-RUN
//     COPY seeded from the host content — reads see the operator's
//     config, writes land in the copy and die with the sandbox;
//   - directories the sandbox never writes bind READ-ONLY — the ones the
//     host EXECUTES (hooks/ scripts, the local CLI install, plugins/) and
//     the ones it LOADS AS INSTRUCTIONS (skills/, commands/, agents/,
//     output-styles/, rules/): hostExecutedClaudeHomeEntries is the list,
//     and the enumeration rule lives with it. When the host has none, an
//     EMPTY read-only overlay pre-created on the host, so the agent
//     cannot plant the directory fresh through the read-write parent bind;
//   - the host's warm caches are not shadowed but REPLACED: the sandbox
//     gets its own, under the iterion home, because the host EXECUTES
//     ($PNPM_HOME's global bins, the Go toolchain in the module cache)
//     and COMPILES (the build cache's objects, trusted unhashed) what
//     they hold — sandboxCacheRoot in sandbox_mounts.go.
//
// Docker mounts nested binds by destination depth, parents first, so the
// overlay wins over the parent bind wherever it is appended (measured,
// docker 29: a copy bound at <dir>/config shadows a read-write bind of
// <dir>, and a read-only bind of <dir>/hooks refuses the plant). The
// per-run copies live in a scratch directory removed at sandbox teardown.
//
// Write semantics of a copy-bound file, stated because the two shapes do
// NOT agree — measured on docker 29.9.2 with the real binaries:
//
//   - a streaming write (O_TRUNC, write in place) lands in the copy and
//     dies with the run — measured on a copy-bound
//     ~/.claude/settings.json, whose rewrite from inside the container
//     landed in the copy with the host file byte-identical. The image's
//     claude 2.1.175 starts and runs against a copy-bound ~/.claude.json
//     (measured, inode and bytes of the host file unchanged after the
//     run), which is the state file it updates;
//   - an ATOMIC write — write a sibling, rename over the file — FAILS,
//     because a mountpoint cannot be renamed over. It does not land in
//     the copy: `git config --global user.email …` on a copy-bound
//     ~/.gitconfig dies with "could not write config file …: Device or
//     resource busy" and exit 4, and so does `--unset`. Reads work. A
//     bot that must write git config per run points GIT_CONFIG_GLOBAL at
//     a path of its own; the same EBUSY is what a workspace-local
//     `git config` (the golden-master extend identity swap) now gets —
//     the safe direction, refused loudly instead of written, and
//     ITERION_SANDBOX_HOST_STATE_GUARD=off restores the old reachability
//     for a bot that needs it.
//
// Audit: every shadowed path rides the sandbox_host_state_mounted event's
// `guarded` list, so `docker inspect` never shows a host bind the run's
// events cannot explain. The escape hatch philosophy §1 requires:
// ITERION_SANDBOX_HOST_STATE_GUARD=off restores the pre-guard all-writable
// posture for the operator whose bots deliberately manage host config.
package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/internal/isolationenv"
	"github.com/SocialGouv/iterion/internal/safepath"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/skilllib"
	"github.com/SocialGouv/iterion/pkg/store"
)

// HostStateGuardEnvVar is the greppable escape hatch: off/none/0/false
// disables the guard (the ticket's "require explicit opt-in", inverted:
// the guard is the default and opting OUT is the explicit act).
const HostStateGuardEnvVar = isolationenv.HostStateGuard

// sandboxCacheRoot is where the sandbox's OWN warm caches live, under the
// operator's iterion home: <iterion home>/sandbox-caches/<the path they are
// served at under $HOME>. Bound at the container's $HOME path so
// GOCACHE/GOMODCACHE/PNPM_HOME resolve to them with no variable set, they
// survive from one sandboxed run to the next — and no tool of the HOST ever
// reads them, which is the whole point: the host executes what
// $PNPM_HOME/<bin> and $GOMODCACHE/golang.org/toolchain@…/bin/go hold, and
// relinks what the build cache holds without re-hashing it.
const sandboxCacheRoot = "sandbox-caches"

// SandboxCacheStoreDir is the sandbox's own cache store under an iterion home,
// and SandboxCacheRelPaths the caches it holds — exported because `iterion
// clean` must reclaim exactly what the mount plan creates. One definition: a
// sweeper keyed on its own copy of the layout is a sweeper that stops finding
// the store the first time the list grows.
func SandboxCacheStoreDir(iterionHome string) string {
	if iterionHome == "" {
		return ""
	}
	return filepath.Join(iterionHome, sandboxCacheRoot)
}

// SandboxCacheRelPaths are the caches the sandbox gets its own copy of, as
// paths under $HOME — the host EXECUTES ($PNPM_HOME's global bins, the Go
// toolchain in the module cache) and COMPILES (the build cache's objects,
// relinked without re-hashing) what they hold, which is why the sandbox must
// not write the host's.
func SandboxCacheRelPaths() []string {
	return []string{
		".cache/go-build", "go/pkg/mod", // Go build + module caches
		".npm",              // npm cache (_cacache)
		".local/share/pnpm", // pnpm home: store + global bins
		".cache/yarn",       // yarn cache
	}
}

// hostExecutedClaudeHomeEntries are the ~/.claude directories a read-only
// overlay must cover, because the HOST — the operator's own CLI, and
// iterion itself — either EXECUTES them or LOADS THEM AS INSTRUCTIONS on
// its next turn. Membership is decided by that verb, never by what a
// sandboxed CLI happens to need:
//
//   - executed as code: hooks/ holds the scripts a settings `command:`
//     names (shadowing settings.json shadows the POINTER, the script is
//     the POINTEE); local/ is the CLI install iterion's own probe resolves
//     and runs (pkg/internal/clilocate.ClaudeLocalFallback); plugins/
//     carries hooks and MCP servers the launcher starts;
//   - loaded as instructions, which steers the operator's next session
//     into running whatever the instruction says: skills/, commands/,
//     agents/, output-styles/, and rules/ — the latter pair being exactly
//     what iterion's own ambient-context policy withholds from a spawn
//     (pkg/backend/delegate/claude_code_ambient.go names ~/.claude/CLAUDE.md
//     and ~/.claude/rules/** as the operator's memory); workflows/ and
//     routines/ hold the harness's own scripted surfaces.
//
// ~/.claude/CLAUDE.md is the same class and a FILE, so it rides
// copyFileOver beside the settings files rather than this list.
//
// Since the home itself is EPHEMERAL (ephemeralDirOver), this list is the READ
// set, not the write protection: a path that is not here is simply ABSENT in
// the container, which is the shape the CLI meets on a fresh machine, and a
// plant lands in the per-run tmpfs. So a forgotten entry costs fidelity — the
// operator's skill or plugin missing from an in-sandbox session — where it used
// to cost the host's integrity. The state dirs the CLI writes (projects/,
// todos/, statsig/, shell-snapshots/, session-env/, backups/, logs/) are
// deliberately NOT here and need no enumeration: they are born with the run
// and die with it. The three paths that must round-trip to the host are named
// one by one at the call site, with the iterion process outside the container
// that reads them.
//
// cowork_plugins/, dev-mods/ and mcp-skill-archives/ are here on the same
// footing as plugins/: the operator's harness loads them as code, so an
// in-sandbox session must see them exactly as the host does — and never write
// them.
var hostExecutedClaudeHomeEntries = []string{
	"agents",
	"commands",
	"cowork_plugins",
	"dev-mods",
	"hooks",
	"local",
	"mcp-skill-archives",
	"output-styles",
	"plugins",
	"routines",
	"rules",
	"skills",
	"workflows",
}

// hostLoadedIterionHomeEntries are the <iterion home> directories ITERION's
// own host-side process loads, read-only overlays for the same reason as the
// ~/.claude list — and a separate list because this home cannot be made
// ephemeral: the run store, the memory tree and the project scratch live
// under it and must survive the run.
//
// Membership is read off the code that loads them on the HOST:
//
//   - plugins/ — pkg/plugin/registry.go starts their MCP servers and
//     rewriters in the launcher's own process;
//   - skills/ — the machine-global skill library (pkg/skilllib/store.go),
//     which mirrorLibrarySkills copies into <workDir>/.claude/skills at
//     EVERY run (pkg/runtime/library_skills.go), where the agent's CLI loads
//     it as instructions. Overwriting the body of a skill the operator's
//     workflows name steers their next run;
//   - connectors/ — the local connector catalog (pkg/connection/local.go,
//     tier Home), whose package decides what a `tool … action:` call reaches
//     and which sealed credential goes with it.
//
// plugins.yaml rides copyFileOver beside them (a file, and one the sandbox's
// own iterion may legitimately write).
var hostLoadedIterionHomeEntries = []string{
	"connectors",
	"plugins",
	skilllib.SkillsDirName,
}

// hostConfigGuard builds the per-run copies and read-only overlays, and
// collects the audit list. Nil is the disabled guard: every method on a
// nil receiver is a no-op, so callers thread it without a nil check.
type hostConfigGuard struct {
	logger *iterlog.Logger
	// dir is the lazily created per-run scratch holding the copies —
	// os.MkdirTemp, removed by cleanup at sandbox teardown. Bind sources
	// must outlive the container, so nothing here is /tmp-cleaned by an
	// outer reaper mid-run.
	dir string
	seq int
	// guarded is the audit trail: "copy:<path>" for a per-run copy shadow,
	// "copy-seeded:<path>" when the host file did not exist and an empty
	// one was created so the shadow has a mountpoint, "ro:<path>" for a
	// read-only overlay (created empty on the host when absent),
	// "tmpfs:<path>" for a state dir the run gets ephemerally, "rw:<path>"
	// for the named exceptions that must round-trip to the host.
	guarded []string
	// tmpfs are the host paths the run gets as a per-run tmpfs instead of a
	// bind: applyHostStateMounts lays them where it lays the $HOME one.
	tmpfs []string
	// writes are the roots whose CONTENT the sandbox writes, so a bind
	// SOURCE under one of them is a path the sandbox could choose through a
	// symlink (safepath.RealBindSource). Registered by sandboxWrites.
	writes []string
	// verified is every (root, path) pair checkBindSource actually verified
	// at plan time — the record Spec.BindSourceChecks is flushed from, so
	// "the plan names every source it verified" holds by construction
	// instead of by two writers keeping their own lists.
	verified     []sandbox.BindSourceCheck
	seenVerified map[string]bool
}

// sandboxWrites registers a root whose content the sandbox writes. Every bind
// source the guard lays is checked against the longest registered root that
// contains it, so a store that gains a sandbox-writable parent gains the check
// by registration rather than by a new call at every site.
func (g *hostConfigGuard) sandboxWrites(roots ...string) {
	if g == nil {
		return
	}
	for _, root := range roots {
		if root != "" {
			g.writes = append(g.writes, filepath.Clean(root))
		}
	}
}

// checkBindSource is the ONE question every shadow asks before it binds: is
// this host path really the path it is named, or does a symlink the sandbox
// planted under a registered root decide what the mount reaches? A path under
// no registered root is not sandbox-written, and answers nil. Every pair the
// question verified is recorded, so the flush (verifiedBindSources) can name
// them for the driver border — a source the plan verified is exactly a source
// this function passed.
func (g *hostConfigGuard) checkBindSource(hostPath string) error {
	if g == nil || hostPath == "" {
		return nil
	}
	hostPath = filepath.Clean(hostPath)
	best := ""
	for _, root := range g.writes {
		if root == hostPath || strings.HasPrefix(hostPath, root+string(filepath.Separator)) {
			if len(root) > len(best) {
				best = root
			}
		}
	}
	if best == "" {
		return nil
	}
	if err := safepath.RealBindSource(best, hostPath); err != nil {
		return err
	}
	if g.seenVerified == nil {
		g.seenVerified = map[string]bool{}
	}
	if !g.seenVerified[hostPath] {
		g.seenVerified[hostPath] = true
		g.verified = append(g.verified, sandbox.BindSourceCheck{Root: best, Path: hostPath})
	}
	return nil
}

// verifiedBindSources returns every source checkBindSource verified at plan
// time, for the flush into Spec.BindSourceChecks. Nil-safe.
func (g *hostConfigGuard) verifiedBindSources() []sandbox.BindSourceCheck {
	if g == nil {
		return nil
	}
	return g.verified
}

// newHostConfigGuard returns the run's guard, or nil when the operator
// disabled it. Only the driver with a shared host filesystem needs one —
// a driver whose binds are all dropped (kubernetes) has nothing to guard,
// and creating copies there would be pure waste.
func newHostConfigGuard(logger *iterlog.Logger) *hostConfigGuard {
	sweepStaleHostGuardScratch()
	off, planted := isolationHatchOff(HostStateGuardEnvVar)
	if off {
		return nil
	}
	g := &hostConfigGuard{logger: logger}
	// The hatch the repository under review wrote is read as unset, and the
	// run that was supposed to open it says so: a guard that stays on
	// without a word reads, in a run log, exactly like a hatch that works.
	if planted {
		g.warn("%s", plantedHatchWarning(HostStateGuardEnvVar))
	}
	return g
}

// cleanup removes the per-run copy scratch. Idempotent; safe on nil.
//
// resolveAndStartSandbox arms this in a defer the moment the guard exists
// and disarms it only once the live sandbox owns it, so EVERY return
// between the two — including the ones that are not errors — takes the
// copies with it. A SIGKILL of the engine runs no defer at all, which is
// what sweepStaleHostGuardScratch answers.
func (g *hostConfigGuard) cleanup() {
	if g == nil || g.dir == "" {
		return
	}
	_ = os.RemoveAll(g.dir)
	g.dir = ""
}

// hostGuardScratchPrefix is iterion's MARK on a per-run copy scratch: the
// one thing that tells a directory under the system temp dir apart from
// every other process's, and therefore the only thing
// sweepStaleHostGuardScratch is allowed to remove.
const hostGuardScratchPrefix = "iterion-hostguard-"

// hostGuardScratchStaleAfter is how long a marked scratch must have been
// untouched to count as abandoned. A live sandbox holds its copies bound
// for the whole run, so the window must exceed any run's duration — the
// same reasoning, and the same figure, as the delegate's ambient-overlay
// sweep (pkg/backend/delegate/ambient_home.go).
const hostGuardScratchStaleAfter = 24 * time.Hour

// sweepStaleHostGuardScratch removes the copy scratches of runs whose
// engine died without a teardown — a SIGKILL, a container OOM — once per
// process, at the first guard built. Each scratch holds copies of the
// operator's config: ~/.claude.json (account userID), ~/.gitconfig,
// ~/.codex/config.toml, owner-only, in a dir no other run mounts.
//
// Three facts are required of every path before it is removed, and a
// failure of any one of them keeps the directory rather than guessing: a
// DIRECT child of the system temp dir, a name carrying iterion's own
// prefix, and a modification time older than the staleness window. A live
// run's scratch is younger; a directory iterion did not create does not
// carry the prefix.
func sweepStaleHostGuardScratch() {
	hostGuardSweepOnce.Do(func() {
		tmp := os.TempDir()
		entries, err := os.ReadDir(tmp)
		if err != nil {
			return
		}
		for _, e := range entries {
			if !e.IsDir() || !strings.HasPrefix(e.Name(), hostGuardScratchPrefix) {
				continue
			}
			info, err := e.Info()
			if err != nil || time.Since(info.ModTime()) < hostGuardScratchStaleAfter {
				continue
			}
			_ = os.RemoveAll(filepath.Join(tmp, e.Name()))
		}
	})
}

var hostGuardSweepOnce sync.Once

// scratch returns the per-run copy dir, creating it on first use. 0700
// (os.MkdirTemp's mode) so the copies are readable by this user alone —
// they carry the operator's own config.
func (g *hostConfigGuard) scratch() (string, error) {
	if g.dir != "" {
		return g.dir, nil
	}
	dir, err := os.MkdirTemp("", hostGuardScratchPrefix)
	if err != nil {
		return "", err
	}
	g.dir = dir
	return dir, nil
}

// copyFileOver shadows a host FILE with a per-run copy bound read-write at
// the same absolute path: the sandbox reads the operator's content, and
// every write — a planted hook as much as Claude Code's own state update —
// lands in the copy and dies with the sandbox. When the host file does not
// exist, seed is created on the host first (user-owned, never the
// root-owned mountpoint docker would create through the parent bind) so
// the path is shadowed rather than plantable, and the copy starts from the
// seed. No-op on a nil guard; a filesystem failure is a WARNED skip, not a
// silent one — an unguarded path under a guard-enabled run must say so.
func (g *hostConfigGuard) copyFileOver(mounts []hostStateMount, hostPath, seed string) []hostStateMount {
	if g == nil || hostPath == "" {
		return mounts
	}
	if err := g.checkBindSource(hostPath); err != nil {
		g.warn("host guard: %v — the path is NOT shadowed and NOT bound; this run runs without it", err)
		return mounts
	}
	kind := "copy"
	content, err := os.ReadFile(hostPath)
	if err != nil {
		if !os.IsNotExist(err) {
			g.warn("host guard: cannot read %s (%v) — the path stays read-write in the sandbox", hostPath, err)
			return mounts
		}
		// Absent on the host: create the empty file so the overlay has a
		// user-owned mountpoint and the agent cannot plant the real one
		// through the read-write parent bind.
		if werr := os.WriteFile(hostPath, []byte(seed), 0o600); werr != nil {
			g.warn("host guard: cannot seed %s (%v) — the path stays plantable in the sandbox", hostPath, werr)
			return mounts
		}
		content = []byte(seed)
		kind = "copy-seeded"
	}
	dir, err := g.scratch()
	if err != nil {
		g.warn("host guard: no per-run scratch (%v) — %s stays read-write in the sandbox", err, hostPath)
		return mounts
	}
	g.seq++
	copy := filepath.Join(dir, fmt.Sprintf("%d-%s", g.seq, filepath.Base(hostPath)))
	if err := os.WriteFile(copy, content, 0o600); err != nil {
		g.warn("host guard: cannot stage a copy of %s (%v) — the path stays read-write in the sandbox", hostPath, err)
		return mounts
	}
	g.note(kind, hostPath)
	// Replace any read-write bind of the file itself (collectHostStateMounts
	// binds ~/.gitconfig / ~/.claude.json directly), then shadow — a nested
	// bind wins over the parent directory's wherever it lands in the list.
	out := mounts[:0]
	for _, m := range mounts {
		if m.HostPath == hostPath && m.ContainerPath == hostPath {
			continue
		}
		out = append(out, m)
	}
	return append(out, hostStateMount{HostPath: copy, ContainerPath: hostPath})
}

// copyFileOverExisting is copyFileOver minus the seeding: a path that does
// not exist on the host is left alone rather than created — for files whose
// ABSENCE is meaningful (a repository's .git/config always exists; an empty
// one created there would be the runtime writing the operator's .git).
func (g *hostConfigGuard) copyFileOverExisting(mounts []hostStateMount, hostPath string) []hostStateMount {
	if g == nil || hostPath == "" {
		return mounts
	}
	if _, err := os.Stat(hostPath); err != nil {
		return mounts
	}
	return g.copyFileOver(mounts, hostPath, "")
}

// note records the audit line: kind is "copy", "copy-seeded", "ro",
// "ro-created" or "private" — the vocabulary the
// sandbox_host_state_mounted event's `guarded` field carries. No-op on a
// nil guard, like every other method: the type's contract is that callers
// thread it without a nil check.
func (g *hostConfigGuard) note(kind, hostPath string) {
	if g == nil {
		return
	}
	g.guarded = append(g.guarded, kind+":"+hostPath)
}

// roDirOver shadows a host DIRECTORY the sandbox never needs to write with
// a read-only bind of the host's own tree — plugins and hooks the host
// executes stay visible (an in-sandbox Claude Code keeps the operator's
// plugins) but unwritable. When the host has no such directory, an empty
// one is created first: without the overlay the agent could plant the
// directory fresh through the read-write parent bind, and the host would
// find it populated on its next run.
func (g *hostConfigGuard) roDirOver(mounts []hostStateMount, hostPath string) []hostStateMount {
	if g == nil || hostPath == "" {
		return mounts
	}
	if err := g.checkBindSource(hostPath); err != nil {
		g.warn("host guard: %v — the path is NOT overlaid and NOT bound; this run runs without it", err)
		return mounts
	}
	info, err := os.Stat(hostPath)
	switch {
	case err == nil && info.IsDir():
		g.note("ro", hostPath)
	case err == nil || os.IsNotExist(err):
		// Absent (or a stray file, which MkdirAll refuses loudly): create
		// the empty dir so the overlay's mountpoint is user-owned — then
		// shadow it. Without the overlay the agent could plant the
		// directory fresh through the read-write parent bind.
		if mkerr := os.MkdirAll(hostPath, 0o755); mkerr != nil {
			g.warn("host guard: cannot create %s (%v) — the path stays plantable in the sandbox", hostPath, mkerr)
			return mounts
		}
		g.note("ro-created", hostPath)
	default:
		g.warn("host guard: cannot stat %s (%v) — the path stays read-write in the sandbox", hostPath, err)
		return mounts
	}
	return append(mounts, hostStateMount{HostPath: hostPath, ContainerPath: hostPath, ReadOnly: true})
}

// ephemeralDirOver replaces the read-write bind of a whole host state
// directory with a PER-RUN TMPFS at the same path: the in-sandbox CLI writes
// it as freely as it likes, every byte dies with the run, and nothing reaches
// the operator's home. The caller lays the tmpfs (applyHostStateMounts, beside
// the $HOME one) from [hostConfigGuard.ephemeralTmpfs].
//
// This is the posture inversion #2069 asked for, and the reason it is the
// right default is the enumeration problem: a read-only overlay list must name
// every path the HOST executes, and a forgotten one writes the host silently,
// while a tmpfs must name every path the SANDBOX needs to see, and a forgotten
// one is simply absent in the container — the shape the CLI meets on a fresh
// machine. The write set of a real authenticated run is not a list this file
// can hold (measured on the image's claude 2.1.175: an unauthenticated
// `--version` already writes backups/, and in a home one state away it does
// not); the READ set is, because it is the operator's own configuration.
//
// What must still round-trip to the host is named, one path at a time, by
// rwDirOver / rwFileOverExisting — with the reason at the call site.
func (g *hostConfigGuard) ephemeralDirOver(mounts []hostStateMount, hostPath string) []hostStateMount {
	if g == nil || hostPath == "" {
		return mounts
	}
	out := mounts[:0]
	for _, m := range mounts {
		if m.HostPath == hostPath && m.ContainerPath == hostPath {
			continue
		}
		out = append(out, m)
	}
	g.tmpfs = append(g.tmpfs, hostPath)
	g.note("tmpfs", hostPath)
	return out
}

// ephemeralTmpfs returns the host paths [hostConfigGuard.ephemeralDirOver]
// made per-run, for the caller that lays tmpfs mounts. Nil-safe.
func (g *hostConfigGuard) ephemeralTmpfs() []string {
	if g == nil {
		return nil
	}
	return append([]string(nil), g.tmpfs...)
}

// roDirOverExisting is roDirOver minus the creation: an absent directory is
// left alone rather than created empty on the host.
//
// It is the right form under an EPHEMERAL parent and the wrong one under a
// read-write bind. Under a tmpfs, a path the host does not have is a path the
// container does not have either, and a plant lands in the tmpfs — so creating
// it would only leave a directory in the operator's home forever (eight of
// them, measured, that no cleanup removes). Under a read-write bind the
// creation is what makes the overlay's mountpoint exist, and roDirOver stays.
func (g *hostConfigGuard) roDirOverExisting(mounts []hostStateMount, hostPath string) []hostStateMount {
	if g == nil || hostPath == "" {
		return mounts
	}
	if err := g.checkBindSource(hostPath); err != nil {
		g.warn("host guard: %v — the path is NOT overlaid and NOT bound; this run runs without it", err)
		return mounts
	}
	info, err := os.Stat(hostPath)
	if err != nil || !info.IsDir() {
		return mounts
	}
	g.note("ro", hostPath)
	return append(mounts, hostStateMount{HostPath: hostPath, ContainerPath: hostPath, ReadOnly: true})
}

// roFileOverExisting binds a host FILE read-only at its own path, only when
// the host has it — for an instruction file the in-sandbox CLI must READ as
// the operator wrote it and never write. A copy would let the sandbox rewrite
// it for its own session, which for an instruction file is not a service:
// nothing in the container needs to edit the operator's memory, and the write
// that matters (the one a plant would make) must fail.
func (g *hostConfigGuard) roFileOverExisting(mounts []hostStateMount, hostPath string) []hostStateMount {
	if g == nil || hostPath == "" {
		return mounts
	}
	if err := g.checkBindSource(hostPath); err != nil {
		g.warn("host guard: %v — the path is NOT overlaid and NOT bound; this run runs without it", err)
		return mounts
	}
	if info, err := os.Stat(hostPath); err != nil || info.IsDir() {
		return mounts
	}
	g.note("ro", hostPath)
	return append(mounts, hostStateMount{HostPath: hostPath, ContainerPath: hostPath, ReadOnly: true})
}

// rwDirOver binds a host state DIRECTORY read-write through an ephemeral
// parent: the named exception, for state that must round-trip to the host
// because an iterion process OUTSIDE the container reads or writes the same
// tree. create makes the directory when the host has none — only pass true for
// a path whose absence would silently degrade a feature, and say which at the
// call site.
func (g *hostConfigGuard) rwDirOver(mounts []hostStateMount, hostPath string, create bool) []hostStateMount {
	if g == nil || hostPath == "" {
		return mounts
	}
	if err := g.checkBindSource(hostPath); err != nil {
		g.warn("host guard: %v — the path is NOT bound read-write; the run gets the ephemeral one instead", err)
		return mounts
	}
	kind := "rw"
	info, err := os.Stat(hostPath)
	switch {
	case err == nil && info.IsDir():
	case create && os.IsNotExist(err):
		if mkerr := os.MkdirAll(hostPath, 0o700); mkerr != nil {
			g.warn("host guard: cannot create %s (%v) — the run gets the ephemeral path instead", hostPath, mkerr)
			return mounts
		}
		kind = "rw-created"
	default:
		return mounts
	}
	g.note(kind, hostPath)
	return append(mounts, hostStateMount{HostPath: hostPath, ContainerPath: hostPath})
}

// rwFileOverExisting binds a host state FILE read-write through an ephemeral
// parent, only when the host has it. Never created: a state file the operator
// does not have is one the CLI must be free to decide about, and an empty one
// iterion put there can read as a corrupt credential or a disabled feature.
func (g *hostConfigGuard) rwFileOverExisting(mounts []hostStateMount, hostPath string) []hostStateMount {
	if g == nil || hostPath == "" {
		return mounts
	}
	if err := g.checkBindSource(hostPath); err != nil {
		g.warn("host guard: %v — the path is NOT bound read-write; the run gets the ephemeral one instead", err)
		return mounts
	}
	if info, err := os.Stat(hostPath); err != nil || info.IsDir() {
		return mounts
	}
	g.note("rw", hostPath)
	return append(mounts, hostStateMount{HostPath: hostPath, ContainerPath: hostPath})
}

// audit returns the shadowed-path list for the sandbox_host_state_mounted
// event, in application order. Nil-safe.
func (g *hostConfigGuard) audit() []string {
	if g == nil || len(g.guarded) == 0 {
		return nil
	}
	return append([]string(nil), g.guarded...)
}

func (g *hostConfigGuard) warn(format string, args ...any) {
	if g != nil && g.logger != nil {
		g.logger.Warn("runtime: "+format, args...)
	}
}

// guardWorktreeGit shadows the host-executed files of the worktree's
// mounted .git tree (#2069): config as a per-run copy — an in-sandbox
// `git config --local` is the lock-and-rename shape, so it does not write
// the copy, it fails EBUSY (the warn below says so), while refs and
// objects, which a commit needs, stay writable — and hooks/ as a
// read-only overlay, created empty on the host when absent so the agent
// cannot plant the directory fresh. applyHostStateMounts calls it (with
// SandboxParams.WorktreeGitDir) so the shadows ride the one
// sandbox_host_state_mounted audit event — and deliberately AFTER its
// HOME-tmpfs layering: a mount visible to homeNestedBindParents whose
// ancestors reach $HOME would lay a tmpfs at the workspace bind's own
// destination, and a tmpfs shadows a bind at the same destination in
// either argument order (measured, docker 29).
func (g *hostConfigGuard) guardWorktreeGit(spec *sandbox.Spec, gitDir string) {
	if g == nil {
		return
	}
	dotGit := worktreeDotGit(gitDir)
	if dotGit == "" {
		return
	}
	// The whole .git tree is bound read-write (addWorktreeGitMount), so it is
	// a root whose content the sandbox writes: the config copy's source and
	// the hooks overlay's are checked against a symlink planted there, like
	// every other bind source under such a root.
	g.sandboxWrites(dotGit)
	// The same precondition the bind itself (addWorktreeGitMount) applies:
	// no existing .git tree, no shadows either.
	info, err := os.Stat(dotGit)
	if err != nil || !info.IsDir() {
		return
	}
	// config is shadowed only when present (a real repository always has
	// one; an absent one is the repo's problem, not a plantable path the
	// guard creates), hooks/ overlaid read-only even when absent.
	guarded := g.copyFileOverExisting(nil, filepath.Join(dotGit, "config"))
	if len(guarded) > 0 {
		// Not silent about the one write that now fails: git's config.lock
		// rename cannot cross the copy's mountpoint, so an in-sandbox
		// `git config <key> <value>` dies with EBUSY. A run log that hides
		// that sends the operator debugging the wrong layer.
		g.warn("host guard: %s/.git/config is shadowed by a per-run copy (#2069) — in-sandbox `git config` writes fail (EBUSY); %s=off restores the writable config", dotGit, HostStateGuardEnvVar)
	}
	guarded = g.roDirOver(guarded, filepath.Join(dotGit, "hooks"))
	for _, m := range guarded {
		entry := fmt.Sprintf("source=%s,target=%s,type=bind", m.HostPath, m.ContainerPath)
		if m.ReadOnly {
			entry += ",readonly"
		}
		spec.Mounts = append(spec.Mounts, entry)
	}
}

// worktreeDotGit derives the shared `.git` directory from a per-run
// worktree gitdir (<repoRoot>/.git/worktrees/<run-id>) — the walk
// addWorktreeGitMount performs, factored so the guard and the bind can
// never disagree on which tree they shadow/mount.
func worktreeDotGit(gitDir string) string {
	if gitDir == "" {
		return ""
	}
	return filepath.Dir(filepath.Dir(gitDir))
}

// guardWorkspaceBind names the run's WORKSPACE bind for the driver border
// (#2069). The workspace is the one bind source the DRIVER derives from run
// metadata (RunInfo.WorkspacePath) instead of from this plan — read-write,
// and for a run on an engine worktree it lives under the iterion home the
// home bind hands every living sandboxed run read-write: the exact shape
// the scratch and the caches have, at the path a takeover values most.
//
// The question asked here is NOT the store's. The workspace is the living
// checkout — it is SUPPOSED to be writable, and its contents are the run's
// own business. What the guard can promise is the tree's identity: the
// resolved source is the directory the engine laid for THIS run under the
// store, no component of it a symlink, no resolution reaching outside the
// store root that laid it. A swap planted through the read-write home bind
// of a run alive between this plan and the creation fails the ask — at
// plan time here, and again at the driver border (MustExist), which is
// what makes the promise hold whatever happens between the two asks.
//
// A workspace the engine did not lay — an operator checkout the launch
// named, anywhere outside the store — is a source no sandbox chose: no
// entry, no ask. Empty store root (a test process without an operator
// home) asks nothing either: there is no store to have been swapped.
func (g *hostConfigGuard) guardWorkspaceBind(spec *sandbox.Spec, workspacePath string) error {
	if g == nil || workspacePath == "" {
		return nil
	}
	abs, err := filepath.Abs(workspacePath)
	if err != nil {
		return fmt.Errorf("resolve the workspace path %s: %w", workspacePath, err)
	}
	root := store.GlobalIterionDataDir()
	if root == "" || abs == root || !strings.HasPrefix(abs, root+string(filepath.Separator)) {
		return nil
	}
	// The plan-time half of the double question, with the same failure
	// meaning the border's half has: this is not the tree the engine laid.
	// Unlike the scratch and the caches there is no cold skip to fall back
	// on — a workspace that fails the ask refuses the RUN, which is why the
	// error carries out instead of degrading into a warn.
	if err := safepath.RealBindSource(root, abs); err != nil {
		return fmt.Errorf("the workspace bind source is not the directory the engine laid under %s (%v) — a symlink under the store decides what the sandbox is handed; refusing the run", root, err)
	}
	spec.BindSourceChecks = append(spec.BindSourceChecks, sandbox.BindSourceCheck{
		Root:      root,
		Path:      abs,
		MustExist: true,
	})
	return nil
}
