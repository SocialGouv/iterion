// Package runtime — devbox provisioning for runs WITHOUT an active sandbox.
//
// The same two devbox sources honoured by the sandbox path
// (sandbox_devbox.go) — the BOT's `devbox.json` shipped beside its
// workflow, and the TARGET REPO's `devbox.json` at the workspace root —
// are provisioned directly on the executing host here. This is the path
// every cloud run takes: the runner pod is the isolation boundary
// (ITERION_SANDBOX_OVERRIDE=none neutralizes any bot-declared sandbox
// block), so no container ever starts and the sandbox-side provisioning
// never fires. The iterion-runner-devbox image ships devbox + Nix for
// exactly this; on hosts without a devbox binary the gap is surfaced
// loudly instead of silently shipping a run whose declared tools are
// absent.
package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/internal/proc"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// hostDevboxInstallTimeout bounds one `devbox install` invocation. A cold
// Nix realise legitimately takes minutes; an install still running past
// this ceiling is treated as wedged (network stall, dead cache) and
// surfaced as a provisioning failure rather than hanging the run start
// with no events flowing.
const hostDevboxInstallTimeout = 15 * time.Minute

// Test seams. Production values shell out to the real devbox binary;
// tests substitute recorders so the engine-level wiring (the part that
// broke in cloud) is assertable without Nix.
var (
	// hostDevboxLookPath resolves the devbox binary on the executing
	// host's PATH.
	hostDevboxLookPath = func() (string, error) { return exec.LookPath("devbox") }

	// hostEngineBinPath resolves the ABSOLUTE PATH of THIS engine's
	// iterion binary. Used by provisionHostDevbox to stage a per-run
	// shim directory holding one `iterion` symlink, so a bot's shell
	// tools resolve `iterion` to this engine (#1384) WITHOUT the
	// resolver's containing directory shadowing the devbox-provisioned
	// node/go/python/git/devbox on that same dir (Rff1076). Empty
	// when no binary can be located; overridden in unit tests so the
	// host-dependent probe does not decide test outcomes.
	hostEngineBinPath = func() string {
		return proc.LocateIterionBinary()
	}

	// hostEngineVolatile reports whether THIS engine runs from a volatile
	// build path (`go run`, tests) — where no installed engine binary is
	// the normal, quiet case. Overridden in tests that pin the warn.
	hostEngineVolatile = func() bool {
		exe, err := os.Executable()
		return err != nil || proc.IsVolatileExecutable(exe)
	}

	// runHostDevboxInstall executes `devbox install -c projectDir` and
	// returns an error carrying the command output on failure.
	runHostDevboxInstall = func(ctx context.Context, devboxBin, projectDir string, logger *iterlog.Logger) error {
		ctx, cancel := context.WithTimeout(ctx, hostDevboxInstallTimeout)
		defer cancel()
		// #nosec G204 — devboxBin comes from exec.LookPath("devbox") and
		// projectDir from the run's own workspace/bundle resolution, not
		// from request input.
		cmd := exec.CommandContext(ctx, devboxBin, "install", "-c", projectDir)
		// `devbox install` drives nix, which forks builders and substituters
		// of its own. Cancelling the run — or hitting the 15-minute ceiling —
		// has to stop them: they hold CombinedOutput's pipes, and a nix build
		// left running writes the store long after the run that asked for it
		// is gone.
		proc.TerminateGroupOnCancel(cmd)
		// devbox prompts before writing a lockfile on some paths; a
		// run-start hook has nobody to answer.
		cmd.Env = append(os.Environ(), "DEVBOX_NO_PROMPT=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("devbox install -c %s: %w (output: %s)", projectDir, err, tailString(string(out), 2000))
		}
		return nil
	}
)

// tailString returns at most the last n bytes of s — install output can
// run long and only the tail carries the failure.
func tailString(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

// engineExtraEnvSetter is the optional interface ClawExecutor implements
// so the engine can push its own run-level process-environment additions
// (the run's PATH: the engine shim, then the devbox profile bin dirs) into
// every host-spawned command of the run — tool nodes, delegate CLI spawns,
// and the claw bash builtin. The engine's layer sits over the launch
// surface's (SetRunExtraEnv) and is replaced, not stacked, when the same
// executor is provisioned again. Type-asserted at call time so test stubs
// don't have to implement it.
type engineExtraEnvSetter interface {
	SetEngineExtraEnv(env []string)
}

// runExtraEnvGetter is the READ half of the same seam: the launch
// surface's value for a key — never the engine's own layer — so a producer
// that composes on top of an existing entry (the PATH prepend) reads the
// same base on every call: an earlier writer's override (the project's
// `.env` PATH pushed by runview or the dispatcher) survives instead of
// being clobbered by `os.Getenv`, and a second provisioning of one
// executor composes from that base again rather than from its own
// previous composition. The production executor implements both halves;
// a test stub that only implements the setter degrades to the os.Getenv
// tail. Grouped as its own interface so such stubs keep compiling.
type runExtraEnvGetter interface {
	GetRunExtraEnvValue(key string) string
}

// hostDevboxProject is one resolved devbox source installed on the host.
type hostDevboxProject struct {
	// label names the source in logs and events ("repo" | "bot"), same
	// vocabulary as the sandbox path's devboxProject.
	label string

	// config is the devbox.json that triggered the project.
	config string

	// dir is the directory `devbox install -c` targets;
	// <dir>/.devbox/nix/profile/default/bin lands on PATH.
	dir string

	// staged marks a project whose config was copied out of a
	// read-only source (the bot's bundle dir) into dir first.
	staged bool
}

// provisionHostDevbox provisions every devbox source this run carries
// directly on the executing host. Called by startSandbox whenever no
// sandbox is active — which is EVERY cloud run (the runner pod is the
// isolation boundary) and every local run without a sandbox declaration.
//
// Same contract as the sandbox-side applyDevboxProvisioning:
//
//  1. INSTALL — `devbox install -c` per source, repo first then bot.
//     The repo installs in place (the workspace is writable and its
//     config may reference sibling paths); the bot's config is staged
//     out of its bundle dir — typically read-only in a runner image
//     (/opt/iterion/bots is root-owned, the pod runs unprivileged) —
//     into a per-run temp dir, because devbox writes `.devbox/` beside
//     the config it installs.
//  2. LOAD — the profile bin dirs are prepended to the run's PATH via
//     the executor's SetRunExtraEnv, which threads them into every
//     host-spawned command (tool nodes, delegate CLIs, claw bash).
//
// The engine's OWN iterion binary is prepended to PATH through a
// per-run SHIM DIRECTORY that holds a single `iterion` symlink to
// proc.LocateIterionBinary — NOT the resolver's whole containing
// directory. LocateIterionBinary resolves to `/usr/local/bin`,
// `~/.local/bin` or a brew/homebrew prefix — dirs that also hold
// `node`, `go`, `python`, `git`, `devbox`, so prepending the whole
// directory would shadow every one of the devbox-provisioned tools
// with the operator's ambient copy and reintroduce the exact class
// of silent engine/bot toolchain disagreement #1384 closed for
// `iterion`, one level up. The shim scopes the prepend to the one
// symbol we intend to resolve. Whichever binary was ambient on PATH
// still resolves at the tail — the shim just gives THIS engine
// priority over it. Sandboxed runs bind-mount /usr/local/bin/iterion
// inside the image (via addClawBinaryMount for claw-carrying
// workflows; the runner image bakes it otherwise), so this fires on
// the HOST path only. See #1384 and its PR-review finding Rff1076.
//
// The PATH tail is the launch surface's PATH, read through the
// runExtraEnvGetter seam (projectenv.Snapshot pushed by
// pkg/runview/service_launch.go's `executor.SetRunExtraEnv(s.runEnv)`
// and its dispatcher twins), the process PATH when none was pushed:
// shim + devboxBinDirs prepend to that base instead of clobbering it
// with `os.Getenv("PATH")` — the operator's `.env` PATH additions would
// otherwise silently vanish on host runs — and never to the executor's
// current composed PATH, so provisioning one executor twice yields the
// same shape twice rather than a PATH growing a removed shim per run.
//
// Best-effort, never silent: a missing devbox binary or a failed
// install warns host-side with what is consequently missing, lands in
// the sandbox_devbox_provisioned event's `errors` field, and lets the
// run proceed. Opt-in stays by file presence — no devbox.json anywhere
// still fires the engineBinDir shim prepend but installs nothing; a
// run whose engine cannot be located AND has no devbox declares
// nothing to push and pushes nothing.
//
// The returned cleanup removes the per-run staging dir AND the shim
// dir; safe to call always.
func (e *Engine) provisionHostDevbox(ctx context.Context, runID string) func() {
	noop := func() {}

	// engineBin: THIS engine's iterion binary path, resolved by the
	// same helper the delegated MCP servers use. Empty when no binary
	// can be located (e.g. an anonymous `go run` under go-build's
	// volatile paths that Locate() correctly skips) — the shim prepend
	// then degrades to devbox-only, exactly as before. Read through
	// the hostEngineBinPath test seam so unit tests can pin it.
	engineBin := hostEngineBinPath()
	if engineBin == "" && !hostEngineVolatile() {
		// Quiet only where no install is expected (`go run`, tests);
		// anywhere else a bot's shell tools are about to fall back to
		// the ambient `iterion` without a word — say why.
		if exe, exeErr := os.Executable(); exeErr == nil && e.logger != nil {
			e.logger.Warn("runtime: host devbox: no iterion binary could be located (ITERION_BIN, a sibling of %s, the standard install paths) — a bot's shell tools will resolve `iterion` to whatever sits on the ambient PATH first", exe)
		}
	}

	// shimDirState is populated on first use of applyRunPath: the
	// staged shim directory holding one `iterion` symlink to
	// engineBin, and its cleanup. Deferred to first use so a run
	// whose executor cannot accept env (test stubs) never writes to
	// disk. Failure to stage falls back to no shim — a shim we
	// cannot create is louder logged than the workflow it would help.
	var shimDir string
	var shimCleanup func()

	// applyRunPath commits the PATH override for this run. Called at
	// each provisioning return path so every branch goes through one
	// PATH assembly (engineBin's shim first, devbox bin dirs after,
	// the base PATH last). Empty shim AND empty devbox bin dirs →
	// nothing to push and no SetEngineExtraEnv call.
	//
	// The base PATH is the launch surface's override when the executor
	// exposes one (`GetRunExtraEnvValue`: a project's `.env` PATH pushed
	// through SetRunExtraEnv by runview or the dispatcher), the process
	// PATH otherwise (test stubs without the getter half) — never the
	// executor's current COMPOSED PATH. What is pushed here lands in the
	// executor's engine layer, which the getter does not read back, so a
	// second provisioning of the same executor composes the same shape
	// again — one shim, the devbox dirs, the base — instead of prepending
	// to a PATH that already carries a previous run's removed shim.
	applyRunPath := func(setter engineExtraEnvSetter, devboxBinDirs []string) string {
		binDirs := make([]string, 0, 1+len(devboxBinDirs))
		if engineBin != "" && shimDir == "" && shimCleanup == nil {
			// Lazy-stage: the shim exists ONLY when a caller is
			// about to push PATH. A run that never reaches
			// SetEngineExtraEnv leaves nothing behind on disk.
			d, cleanup, err := stageEngineShim(runID, engineBin)
			if err != nil {
				if e.logger != nil {
					e.logger.Warn("runtime: host devbox: could not stage the engine iterion shim (%v) — a bot's shell tools may resolve `iterion` to whatever sits on the ambient PATH first", err)
				}
			} else {
				shimDir, shimCleanup = d, cleanup
			}
		}
		if shimDir != "" {
			binDirs = append(binDirs, shimDir)
		}
		binDirs = append(binDirs, devboxBinDirs...)
		if len(binDirs) == 0 {
			return ""
		}
		tailPath := os.Getenv("PATH")
		if getter, ok := setter.(runExtraEnvGetter); ok {
			if existing := getter.GetRunExtraEnvValue("PATH"); existing != "" {
				tailPath = existing
			}
		}
		path := strings.Join(binDirs, ":")
		if tailPath != "" {
			path += ":" + tailPath
		}
		setter.SetEngineExtraEnv([]string{"PATH=" + path})
		return path
	}

	repoCfg := devboxConfigIn(e.workDir, "workspace", e.logger)
	botCfg := devboxConfigIn(e.resourceDirForRun(), "bundle", e.logger)
	// A declined repo source is REPORTED, not dropped: silence here is
	// indistinguishable from a repo that declared nothing, and the
	// operator staring at a missing binary deserves the reason.
	skippedRepo := ""
	if repoCfg != "" && !e.repoDevboxEnabled() {
		if e.logger != nil {
			e.logger.Info("runtime: host devbox: repo_devbox is off — %s is NOT installed for this run; the packages the target repo pins are unavailable (the bot's own devbox.json still is)", repoCfg)
		}
		skippedRepo, repoCfg = repoCfg, ""
	}
	if repoCfg == "" && botCfg == "" {
		// No devbox source. Push the engine shim through PATH if the
		// executor supports the seam — a bot's shell tools still
		// resolve `iterion` to THIS engine even without a devbox
		// declaration (#1384). Absent-setter is silently accepted here
		// because there is nothing devbox-shaped to complain about;
		// the "packages declared…" warning below fires ONLY when a
		// declaration actually exists and cannot be delivered.
		if setter, ok := e.executor.(engineExtraEnvSetter); ok {
			cleanup := noop
			path := applyRunPath(setter, nil)
			if shimCleanup != nil {
				cleanup = shimCleanup
			}
			if skippedRepo != "" {
				payload := map[string]any{
					"target":          "host",
					"skipped_sources": []string{"repo"},
					"skipped_configs": []string{skippedRepo},
					"reason":          "repo_devbox off",
				}
				if path != "" {
					payload["path"] = path
				}
				if err := e.emit(ctx, runID, store.EventSandboxDevboxProvisioned, "", payload); err != nil && e.logger != nil {
					e.logger.Warn("runtime: emit %s event for run %s: %v", store.EventSandboxDevboxProvisioned, runID, err)
				}
			}
			return cleanup
		}
		// Non-setter executor with no devbox declaration: the
		// declined-repo event still fires (pre-#1384 behaviour), and
		// no warning is emitted because nothing was declared to be
		// undeliverable.
		if skippedRepo != "" {
			if err := e.emit(ctx, runID, store.EventSandboxDevboxProvisioned, "", map[string]any{
				"target":          "host",
				"skipped_sources": []string{"repo"},
				"skipped_configs": []string{skippedRepo},
				"reason":          "repo_devbox off",
			}); err != nil && e.logger != nil {
				e.logger.Warn("runtime: emit %s event for run %s: %v", store.EventSandboxDevboxProvisioned, runID, err)
			}
		}
		return noop
	}

	// The executor's env seam is the ONLY consumer of both the engine
	// binary shim AND the devbox profile bin dirs — without it neither
	// can reach a command's PATH. The production executor
	// (model.ClawExecutor) always implements it; an executor that does
	// not is a test stub, whose runs must stay hermetic (no real
	// `devbox install` out of an e2e scenario). Checked HERE (after
	// the no-devbox short circuit) so a declined-repo event still
	// fires for a non-setter executor and the "packages declared…"
	// warning only fires when there IS a devbox declaration to
	// deliver — the ordering the pre-#1384 code chose (the reviewer
	// pointed out that moving it above the short-circuit swallowed
	// the declined-repo event and mis-warned on no-devbox runs).
	setter, ok := e.executor.(engineExtraEnvSetter)
	if !ok {
		if e.logger != nil {
			e.logger.Warn("runtime: host devbox: executor %T cannot receive run-level env — devbox provisioning skipped, the packages declared by the run's devbox.json are unavailable", e.executor)
		}
		// The declined-repo event still fires: the operator's signal
		// about repo_devbox=off must not depend on whether the bot
		// also declared a devbox (which reaches this branch when
		// botCfg is non-empty; the no-devbox branch above handles
		// the botCfg=="" case). Same class as the question the
		// #1487 verdict flagged — the reader's follow-up round found
		// this pre-existing sibling.
		if skippedRepo != "" {
			if err := e.emit(ctx, runID, store.EventSandboxDevboxProvisioned, "", map[string]any{
				"target":          "host",
				"skipped_sources": []string{"repo"},
				"skipped_configs": []string{skippedRepo},
				"reason":          "repo_devbox off",
			}); err != nil && e.logger != nil {
				e.logger.Warn("runtime: emit %s event for run %s: %v", store.EventSandboxDevboxProvisioned, runID, err)
			}
		}
		return noop
	}

	var errs []string
	// lockKept records what keepRepoDevboxLock had to undo after the repo's
	// install: the operator reads it in the event, next to the errors.
	var lockKept []string
	emitOutcome := func(projects []hostDevboxProject, binDirs []string, path string) {
		payload := map[string]any{
			"target": "host",
		}
		if len(lockKept) > 0 {
			payload["lock_kept"] = lockKept
		}
		if skippedRepo != "" {
			payload["skipped_sources"] = []string{"repo"}
			payload["skipped_configs"] = []string{skippedRepo}
			payload["reason"] = "repo_devbox off"
		}
		if len(projects) > 0 {
			labels := make([]string, 0, len(projects))
			configs := make([]string, 0, len(projects))
			for _, pr := range projects {
				labels = append(labels, pr.label)
				configs = append(configs, pr.config)
			}
			payload["sources"] = labels
			payload["configs"] = configs
		}
		if len(binDirs) > 0 {
			payload["bin_dirs"] = binDirs
		}
		// Shim-only runs (devbox missing, or every source lost at
		// stage time) change the PATH too: the operator reading the
		// event sees what commands will resolve against.
		if path != "" {
			payload["path"] = path
		}
		if len(errs) > 0 {
			payload["errors"] = errs
		}
		if err := e.emit(ctx, runID, store.EventSandboxDevboxProvisioned, "", payload); err != nil && e.logger != nil {
			e.logger.Warn("runtime: emit %s event for run %s: %v", store.EventSandboxDevboxProvisioned, runID, err)
		}
	}

	devboxBin, lookErr := hostDevboxLookPath()
	if lookErr != nil {
		missing := make([]string, 0, 2)
		for _, cfg := range []string{repoCfg, botCfg} {
			if cfg != "" {
				missing = append(missing, cfg)
			}
		}
		errs = append(errs, fmt.Sprintf("devbox is not on PATH on this host — %s ignored, the packages it declares are unavailable", strings.Join(missing, " + ")))
		if e.logger != nil {
			e.logger.Warn("runtime: host devbox: %s (install devbox or run in an image that ships it, e.g. iterion-runner-devbox)", errs[len(errs)-1])
		}
		// The engine binary shim still fires so `iterion` inside the
		// bot means THIS engine even when devbox is missing (#1384).
		path := applyRunPath(setter, nil)
		emitOutcome(nil, nil, path)
		// A shim WAS created here — return its cleanup so the
		// per-run temp dir does not leak on missing-devbox runs.
		if shimCleanup != nil {
			return shimCleanup
		}
		return noop
	}

	// Repo first, then bot: a repo pinning its own toolchain stays
	// authoritative for building itself, the bot's packages fill in the
	// rest — same PATH-precedence order as the sandbox path.
	var projects []hostDevboxProject
	if repoCfg != "" {
		projects = append(projects, hostDevboxProject{label: "repo", config: repoCfg, dir: e.workDir})
	}
	stageRoot := ""
	if botCfg != "" {
		// The bot's config is copied into a private per-run directory
		// (runScopedTempDir: 0700, unpredictable, directly under the
		// temp dir): `devbox install` runs there and its profile bin
		// dir goes on the run's PATH, so it must be a directory nobody
		// else on the host could have pre-created or can substitute.
		root, mkErr := runScopedTempDir("iterion-devbox", runID)
		if mkErr != nil {
			errs = append(errs, fmt.Sprintf("stage the bot devbox.json (%s): %v — the packages it declares are NOT available to this run", botCfg, mkErr))
			if e.logger != nil {
				e.logger.Warn("runtime: host devbox: %s", errs[len(errs)-1])
			}
		} else {
			stageRoot = root
			botDir := filepath.Join(stageRoot, "bot")
			if stageErr := stageHostDevboxConfig(filepath.Dir(botCfg), botDir); stageErr != nil {
				errs = append(errs, fmt.Sprintf("stage the bot devbox.json (%s): %v — the packages it declares are NOT available to this run", botCfg, stageErr))
				if e.logger != nil {
					e.logger.Warn("runtime: host devbox: %s", errs[len(errs)-1])
				}
			} else {
				projects = append(projects, hostDevboxProject{label: "bot", config: botCfg, dir: botDir, staged: true})
			}
		}
	}
	cleanup := noop
	if stageRoot != "" {
		cleanup = func() { _ = os.RemoveAll(stageRoot) }
	}
	// composeCleanup chains the devbox staging cleanup with the shim
	// cleanup (populated lazily on first applyRunPath call). Both may
	// be no-ops individually.
	composeCleanup := func() func() {
		devboxCleanup := cleanup
		return func() {
			devboxCleanup()
			if shimCleanup != nil {
				shimCleanup()
			}
		}
	}
	if len(projects) == 0 {
		// Same engine binary prepend, so a run that lost every
		// devbox source at stage-time still gets THIS engine's
		// iterion in front of PATH (#1384).
		path := applyRunPath(setter, nil)
		emitOutcome(nil, nil, path)
		return composeCleanup()
	}

	binDirs := make([]string, 0, len(projects))
	labels := make([]string, 0, len(projects))
	configs := make([]string, 0, len(projects))
	for _, pr := range projects {
		binDirs = append(binDirs, filepath.Join(pr.dir, filepath.FromSlash(devboxProfileBin)))
		labels = append(labels, pr.label)
		configs = append(configs, pr.config)
	}

	if e.logger != nil {
		e.logger.Info("runtime: host devbox: provisioning %s from %s — `devbox install` now, %s prepended to the run's PATH (a cold Nix realise can take minutes)",
			strings.Join(labels, "+"), strings.Join(configs, ", "), strings.Join(binDirs, ", "))
	}
	for _, pr := range projects {
		// The repo installs IN PLACE, in the run's worktree: what `devbox
		// install` writes there, the run's own gates read as the pass's work.
		var lockBefore []byte
		hadLock := false
		keepLock := pr.label == "repo"
		if keepLock {
			data, err := os.ReadFile(filepath.Join(pr.dir, devboxLockName))
			switch {
			case err == nil:
				lockBefore, hadLock = data, true
			case errors.Is(err, fs.ErrNotExist):
				// The repository tracks no lock: one the install creates is removed below.
			default:
				// Unreadable is not absent: a read error here must not turn into
				// "created by devbox" after the install and remove a tracked file.
				keepLock = false
				errs = append(errs, fmt.Sprintf("read the repo %s before install: %v — whatever devbox writes to it stays in the worktree", devboxLockName, err))
				if e.logger != nil {
					e.logger.Warn("runtime: host devbox: %s", errs[len(errs)-1])
				}
			}
		}
		if instErr := runHostDevboxInstall(ctx, devboxBin, pr.dir, e.logger); instErr != nil {
			errs = append(errs, fmt.Sprintf("install failed for the %s %s: %v — the packages it declares are NOT on PATH for this run", pr.label, devboxConfigName, instErr))
			if e.logger != nil {
				e.logger.Warn("runtime: host devbox: %s", errs[len(errs)-1])
			}
		}
		if keepLock {
			if note, err := keepRepoDevboxLock(pr.dir, lockBefore, hadLock); err != nil {
				errs = append(errs, fmt.Sprintf("keep the repo %s: %v — the worktree carries whatever the failed write left there", devboxLockName, err))
				if e.logger != nil {
					e.logger.Warn("runtime: host devbox: %s", errs[len(errs)-1])
				}
			} else if note != "" {
				lockKept = append(lockKept, note)
				if e.logger != nil {
					e.logger.Warn("runtime: host devbox: %s", note)
				}
			}
		}
	}

	// PATH is exposed even for a partially failed install: the profile
	// dirs of the successful projects must load, and a failed project's
	// dir simply resolves nothing. The failure itself is already loud
	// (warn + event errors). The engine binary SHIM is prepended in
	// FRONT of every devbox dir so `iterion` inside the bot means THIS
	// engine WITHOUT shadowing the devbox toolchain (#1384 + Rff1076);
	// the existing PATH override (projectenv) survives as the tail via
	// the runExtraEnvGetter seam.
	path := applyRunPath(setter, binDirs)
	emitOutcome(projects, binDirs, path)
	return composeCleanup()
}

// runScopedTempDir creates a fresh private directory for one run's
// provisioning artefacts: os.MkdirTemp makes it 0700 under an
// unpredictable name DIRECTLY in os.TempDir() — a sticky temp dir (as
// /tmp is) keeps every other local user from renaming or unlinking the
// entry — and never adopts a path that already exists (a collision
// retries under another name). What these directories hold leads the PATH of every
// host-spawned command of the run (the engine shim) or is installed from
// and put on it (the staged bot devbox project), so a fixed parent such
// as `$TMPDIR/<prefix>/<runID>` is not an option: it belongs to whichever
// local user created it first — a second user on the host then fails
// MkdirAll with EACCES and silently loses the provisioning, and the owner
// can rename the per-run entry away and substitute one carrying a binary
// of their own, which the run then executes as its user. The runID rides
// in the name as a debugging hint for a directory a crash left behind,
// reduced to a safe file-name alphabet so an explicit --run-id can carry
// neither a path separator (MkdirTemp refuses one) nor an unbounded
// length.
func runScopedTempDir(prefix, runID string) (string, error) {
	dir, err := os.MkdirTemp("", prefix+"-"+tempNameHint(runID)+"-")
	if err != nil {
		return "", err
	}
	// A relative TMPDIR comes back relative, and this directory goes
	// FIRST on the run's PATH: tool nodes run with cmd.Dir set to the
	// run's workDir, so a cwd-relative entry would resolve nowhere and
	// silently drop the shim (and the devbox profile dirs).
	abs, err := filepath.Abs(dir)
	if err != nil {
		// Reachable only if the cwd dies between MkdirTemp and Abs
		// (an absolute TMPDIR never triggers an Abs failure); the
		// directory we just created must not leak behind it.
		_ = os.RemoveAll(dir)
		return "", err
	}
	return abs, nil
}

// tempNameHint reduces runID to a single path component of at most 64
// bytes over [A-Za-z0-9._-]; every other byte becomes '_'.
func tempNameHint(runID string) string {
	var b strings.Builder
	for i := 0; i < len(runID) && b.Len() < 64; i++ {
		c := runID[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
			b.WriteByte(c)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// stageEngineShim creates a private per-run directory holding a SINGLE
// `iterion` symlink to engineBin, and returns its absolute path along
// with a cleanup that removes it. Used by provisionHostDevbox to
// prepend one binary — not one whole directory — to the run's PATH,
// so a bot's shell tools resolve `iterion` to THIS engine WITHOUT
// shadowing the devbox-provisioned toolchain that also lives in
// LocateIterionBinary's containing dir (node/go/python/git/devbox on
// /usr/local/bin, ~/.local/bin, or a brew prefix). See #1384's
// PR-review finding Rff1076.
//
// The directory comes from runScopedTempDir — 0700, unpredictable,
// directly under os.TempDir(), never a pre-existing path — because it
// LEADS the PATH of every host-spawned command of the run: at a
// predictable location under a shared parent, another local user could
// substitute the `iterion` the run executes.
//
// Symlink over copy so the shim never drifts against the running
// binary. If os.Symlink is refused by the filesystem (a rare non-POSIX
// mount), the directory is removed and the caller degrades to no shim
// rather than blocking the run, with a warning: a bot's shell then sees
// whatever `iterion` sits on the ambient PATH.
func stageEngineShim(runID, engineBin string) (string, func(), error) {
	root, err := runScopedTempDir("iterion-engine-shim", runID)
	if err != nil {
		return "", nil, err
	}
	shimLink := filepath.Join(root, "iterion")
	if err := os.Symlink(engineBin, shimLink); err != nil {
		_ = os.RemoveAll(root)
		return "", nil, fmt.Errorf("symlink %s -> %s: %w", shimLink, engineBin, err)
	}
	// Belt: whatever the cause — a stale binary path removed between the
	// locate and this link, a relative engine binary — a shim whose
	// `iterion` resolves to nothing would silently send every bot shell
	// to the ambient PATH's iterion, the exact disagreement #1384
	// reports, with no warning (the staging itself "succeeded"). Stat
	// the link and fail loud instead.
	if _, err := os.Stat(shimLink); err != nil {
		_ = os.RemoveAll(root)
		return "", nil, fmt.Errorf("shim %s does not resolve (target %s): %w", shimLink, engineBin, err)
	}
	return root, func() { _ = os.RemoveAll(root) }, nil
}

// stageHostDevboxConfig copies srcDir's devbox.json (and devbox.lock
// when present) into dstDir, creating it. The bundle dir is typically
// read-only in a runner image and devbox writes `.devbox/` beside the
// config it installs, so installing from the source dir is impossible.
func stageHostDevboxConfig(srcDir, dstDir string) error {
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return err
	}
	if err := copyRegularFile(filepath.Join(srcDir, devboxConfigName), filepath.Join(dstDir, devboxConfigName)); err != nil {
		return err
	}
	// The lock is optional — a bundle that ships none installs unlocked.
	lockSrc := filepath.Join(srcDir, devboxLockName)
	if _, err := os.Stat(lockSrc); err == nil {
		return copyRegularFile(lockSrc, filepath.Join(dstDir, devboxLockName))
	}
	return nil
}

// keepRepoDevboxLock puts the repository's devbox.lock back to what the run
// found it when what `devbox install` wrote in place in the worktree is
// plugin metadata drift — and nothing else.
//
// devbox rewrites the lock's `plugin_version` whenever the host's plugin
// registry is newer than the pin — one line on a laptop with a fresh devbox,
// on a runner image rebuilt with one. The tracked tree then differs before
// the first node runs, and every gate that reads the tree as the pass's own
// work (a campaign's scope gate, a clean-tree precheck, a whole-tree commit)
// refuses honest work for a file the bot never touched (#1459). Metadata is
// all that moved, so the pinned content comes back. A lock the install
// changed BEYOND that — a repository whose lock was behind its devbox.json,
// packages resolved for the first time or moved to another nixpkgs commit —
// is kept: the worktree then carries the resolution the run's PATH was
// built from, and a later `devbox run` reuses it instead of resolving anew,
// possibly elsewhere; the gates see the change, rightly, the repository is
// out of sync with itself. A lock the install CREATED is removed only where
// git would show it — an untracked file in a repository that does not
// ignore it; an ignored lock, or one outside any repository, costs the gates
// nothing and spares every later devbox invocation a resolution. A lock the
// install left untouched yields no note and no write; one it removed, or
// left unparseable (an install cut short mid-write is no resolution anything
// can reuse), comes back; one it created AND left unparseable is removed
// wherever it is — devbox would refuse to run with it. The note says what
// was done.
func keepRepoDevboxLock(dir string, before []byte, hadLock bool) (string, error) {
	path := filepath.Join(dir, devboxLockName)
	after, err := os.ReadFile(path)
	switch {
	case hadLock && err == nil && bytes.Equal(after, before):
		return "", nil
	case hadLock && err != nil:
		// Removed: put the pinned content back.
		if werr := os.WriteFile(path, before, 0o644); werr != nil {
			return "", werr
		}
		return fmt.Sprintf("devbox install removed %s; restored to the repository's pinned content so the worktree stays what the run found it", path), nil
	case hadLock && !lockParses(after):
		// Left unparseable — an install that crashed or was cut short
		// mid-write: no resolution anything can reuse. The pinned content
		// comes back; the install's own failure is reported on its own.
		if werr := os.WriteFile(path, before, 0o644); werr != nil {
			return "", werr
		}
		return fmt.Sprintf("devbox install left %s unparseable (%d bytes); restored to the repository's pinned content — the install itself is reported on its own", path, len(after)), nil
	case hadLock && !lockDiffersOnlyInPluginMetadata(before, after):
		// A real re-lock: kept, said — and said why the pin lost.
		reason := "the repository's lock was behind its devbox.json"
		if !lockParses(before) {
			reason = "the repository's lock did not parse"
		}
		return fmt.Sprintf("devbox install changed %s beyond plugin metadata (%s); kept — the worktree carries the resolution the run's PATH was built from", path, reason), nil
	case hadLock:
		// Rewritten metadata: restore the pinned content.
		if werr := os.WriteFile(path, before, 0o644); werr != nil {
			return "", werr
		}
		return fmt.Sprintf("devbox install rewrote %s (%d → %d bytes: plugin metadata drift on this host); restored to the repository's pinned content so the worktree stays what the run found it", path, len(before), len(after)), nil
	case err == nil:
		// Created in a repository that tracks none. One the install left
		// unparseable goes first: devbox refuses to run with it in place
		// (measured: `unexpected end of JSON input`), so keeping it would
		// break every later devbox invocation in the worktree rather than
		// spare one a resolution.
		if !lockParses(after) {
			if rerr := os.Remove(path); rerr != nil {
				return "", rerr
			}
			return fmt.Sprintf("devbox install created %s and left it unparseable; removed — no later devbox run could have read it", path), nil
		}
		// Removed only where git would show it.
		visible, known := gitWouldShow(dir, devboxLockName)
		if !known {
			return fmt.Sprintf("devbox install created %s; kept — no git repository reads the worktree, so no gate sees it and a later devbox run reuses it", path), nil
		}
		if !visible {
			return fmt.Sprintf("devbox install created %s; kept — the repository ignores it, so no gate sees it and a later devbox run reuses it", path), nil
		}
		if rerr := os.Remove(path); rerr != nil {
			return "", rerr
		}
		return fmt.Sprintf("devbox install created %s, a file the repository does not track; removed so the worktree stays clean", path), nil
	default:
		return "", nil
	}
}

// lockParses reports whether b is a devbox.lock document devbox can read
// again: a JSON object. A write cut short, an empty file or a bare scalar
// is not one — `json.Valid` alone would accept `null` or `[]`.
func lockParses(b []byte) bool {
	var doc map[string]any
	return json.Unmarshal(b, &doc) == nil && doc != nil
}

// lockDiffersOnlyInPluginMetadata reports whether two devbox.lock documents
// read the same once every package entry's `plugin_version` — the field
// devbox rewrites when the host's plugin registry is newer than the pin —
// is dropped. Content that does not parse is a real difference.
func lockDiffersOnlyInPluginMetadata(a, b []byte) bool {
	strip := func(raw []byte) (map[string]any, bool) {
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			return nil, false
		}
		if pkgs, ok := doc["packages"].(map[string]any); ok {
			for _, v := range pkgs {
				if entry, ok := v.(map[string]any); ok {
					delete(entry, "plugin_version")
				}
			}
		}
		return doc, true
	}
	da, oka := strip(a)
	db, okb := strip(b)
	return oka && okb && reflect.DeepEqual(da, db)
}

// gitWouldShow reports whether git, asked in dir, would list rel as a change
// — an untracked file no ignore rule covers. known is false when dir is not
// inside a git work tree or git cannot answer.
func gitWouldShow(dir, rel string) (visible, known bool) {
	inside, cancel := gitCmd("-C", dir, "rev-parse", "--is-inside-work-tree")
	defer cancel()
	if err := inside.Run(); err != nil {
		return false, false
	}
	check, cancelCheck := gitCmd("-C", dir, "check-ignore", "-q", rel)
	defer cancelCheck()
	err := check.Run()
	if err == nil {
		return false, true // exit 0: an ignore rule covers it
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return true, true // exit 1: not ignored
	}
	return false, false
}

// copyRegularFile copies src to dst (0644).
func copyRegularFile(src, dst string) error {
	// #nosec G304 — both paths are derived from the run's own
	// workspace/bundle resolution, not from request input.
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o644)
}
