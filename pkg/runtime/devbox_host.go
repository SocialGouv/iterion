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
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

// runExtraEnvSetter is the optional interface ClawExecutor implements so
// the engine can push run-level process-environment additions (the
// devbox profile bin dirs on PATH) into every host-spawned command of
// the run — tool nodes, delegate CLI spawns, and the claw bash builtin.
// Type-asserted at call time so test stubs don't have to implement it.
type runExtraEnvSetter interface {
	SetRunExtraEnv(env []string)
}

// runExtraEnvGetter is the READ half of the same seam: a producer that
// composes on top of an existing entry (e.g. `PATH` prepend) reads the
// current value here instead of `os.Getenv`, so an earlier writer's
// override is not silently discarded. The DEFAULT executor implements
// both halves; a test stub that only implements the setter degrades
// to the pre-M1 behaviour (os.Getenv fallback), which is exactly what
// the existing devbox tests expect. Grouped as its own interface (not
// merged with the setter) so old tests keep compiling.
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
// The PATH tail is composed via the runExtraEnvGetter seam so an
// EARLIER writer's PATH override (projectenv.Snapshot pushed by
// pkg/runview/service_launch.go's `executor.SetRunExtraEnv(s.runEnv)`)
// survives this call — engineBinDir + devboxBinDirs prepend to the
// existing value instead of clobbering it with `os.Getenv("PATH")`.
// Without this the operator's `.env` PATH additions would silently
// vanish on host runs (M1 of the #1384 adversarial round).
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
	// existing PATH last). Empty shim AND empty devbox bin dirs →
	// nothing to push and no SetRunExtraEnv call.
	//
	// tailPath: the existing PATH the fix must not discard. Reads the
	// executor's own runExtraEnv (`GetRunExtraEnvValue`) when available
	// so an earlier SetRunExtraEnv-pushed PATH override (e.g. the
	// project's `.env` PATH via `runview.Service.runEnv`) is preserved.
	// Falls back to `os.Getenv("PATH")` for test stubs that don't
	// implement the getter half of the seam.
	applyRunPath := func(setter runExtraEnvSetter, devboxBinDirs []string) string {
		binDirs := make([]string, 0, 1+len(devboxBinDirs))
		if engineBin != "" && shimDir == "" && shimCleanup == nil {
			// Lazy-stage: the shim exists ONLY when a caller is
			// about to push PATH. A run that never reaches
			// SetRunExtraEnv leaves nothing behind on disk.
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
		setter.SetRunExtraEnv([]string{"PATH=" + path})
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
		if setter, ok := e.executor.(runExtraEnvSetter); ok {
			cleanup := noop
			_ = applyRunPath(setter, nil)
			if shimCleanup != nil {
				cleanup = shimCleanup
			}
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
	setter, ok := e.executor.(runExtraEnvSetter)
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
	emitOutcome := func(projects []hostDevboxProject, binDirs []string, path string) {
		payload := map[string]any{
			"target": "host",
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
		stageRoot = filepath.Join(os.TempDir(), "iterion-devbox", runID)
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
		if instErr := runHostDevboxInstall(ctx, devboxBin, pr.dir, e.logger); instErr != nil {
			errs = append(errs, fmt.Sprintf("install failed for the %s %s: %v — the packages it declares are NOT on PATH for this run", pr.label, devboxConfigName, instErr))
			if e.logger != nil {
				e.logger.Warn("runtime: host devbox: %s", errs[len(errs)-1])
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

// stageEngineShim creates a per-run directory holding a SINGLE
// `iterion` symlink to engineBin, and returns its absolute path along
// with a cleanup that removes it. Used by provisionHostDevbox to
// prepend one binary — not one whole directory — to the run's PATH,
// so a bot's shell tools resolve `iterion` to THIS engine WITHOUT
// shadowing the devbox-provisioned toolchain that also lives in
// LocateIterionBinary's containing dir (node/go/python/git/devbox on
// /usr/local/bin, ~/.local/bin, or a brew prefix). See #1384's
// PR-review finding Rff1076.
//
// Symlink over copy so the shim never drifts against the running
// binary. If os.Symlink is refused by the filesystem (a rare non-POSIX
// mount), the caller degrades to no shim rather than blocking the
// run: a bot's shell then sees whatever `iterion` sits on the ambient
// PATH, exactly the pre-#1384 behaviour.
//
// The per-run directory is a subdir of os.TempDir() and carries the
// runID: a resume that re-enters provisionHostDevbox recreates it
// from scratch (removing the previous one first).
func stageEngineShim(runID, engineBin string) (string, func(), error) {
	root := filepath.Join(os.TempDir(), "iterion-engine-shim", runID)
	// Remove any leftover from a previous invocation so a stale
	// symlink from a prior engine binary path never wins.
	_ = os.RemoveAll(root)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", nil, err
	}
	shimLink := filepath.Join(root, "iterion")
	if err := os.Symlink(engineBin, shimLink); err != nil {
		_ = os.RemoveAll(root)
		return "", nil, fmt.Errorf("symlink %s -> %s: %w", shimLink, engineBin, err)
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
