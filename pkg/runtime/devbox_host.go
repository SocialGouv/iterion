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

	// runHostDevboxInstall executes `devbox install -c projectDir` and
	// returns an error carrying the command output on failure.
	runHostDevboxInstall = func(ctx context.Context, devboxBin, projectDir string, logger *iterlog.Logger) error {
		ctx, cancel := context.WithTimeout(ctx, hostDevboxInstallTimeout)
		defer cancel()
		// #nosec G204 — devboxBin comes from exec.LookPath("devbox") and
		// projectDir from the run's own workspace/bundle resolution, not
		// from request input.
		cmd := exec.CommandContext(ctx, devboxBin, "install", "-c", projectDir)
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
// Best-effort, never silent: a missing devbox binary or a failed
// install warns host-side with what is consequently missing, lands in
// the sandbox_devbox_provisioned event's `errors` field, and lets the
// run proceed. Opt-in stays by file presence — no devbox.json anywhere
// means no event, no install, no PATH change.
//
// The returned cleanup removes the per-run staging dir; safe to call
// always.
func (e *Engine) provisionHostDevbox(ctx context.Context, runID string) func() {
	noop := func() {}

	repoCfg := devboxConfigIn(e.workDir, "workspace", e.logger)
	botCfg := devboxConfigIn(bundleResourceDir(e.bundle, e.filePath), "bundle", e.logger)
	if repoCfg == "" && botCfg == "" {
		return noop
	}

	// The executor's env seam is provisioning's ONLY consumer: without it
	// the installed profile bin dirs can never reach a command's PATH, so
	// installing would be pure waste. The production executor
	// (model.ClawExecutor) always implements it — an executor that does
	// not is a test stub, whose runs must stay hermetic (no real `devbox
	// install` out of an e2e scenario).
	setter, ok := e.executor.(runExtraEnvSetter)
	if !ok {
		if e.logger != nil {
			e.logger.Warn("runtime: host devbox: executor %T cannot receive run-level env — devbox provisioning skipped, the packages declared by the run's devbox.json are unavailable", e.executor)
		}
		return noop
	}

	var errs []string
	emitOutcome := func(projects []hostDevboxProject, binDirs []string, path string) {
		payload := map[string]any{
			"target": "host",
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
		emitOutcome(nil, nil, "")
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
	if len(projects) == 0 {
		emitOutcome(nil, nil, "")
		return cleanup
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
	// (warn + event errors).
	path := strings.Join(binDirs, ":")
	if base := os.Getenv("PATH"); base != "" {
		path += ":" + base
	}
	setter.SetRunExtraEnv([]string{"PATH=" + path})
	emitOutcome(projects, binDirs, path)
	return cleanup
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
