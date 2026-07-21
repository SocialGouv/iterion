// Package runtime — devbox provisioning for sandboxed runs.
//
// Two devbox sources can supply the binaries a run needs, and both are
// honoured together:
//
//   - the BOT's `devbox.json`, shipped next to `main.bot` in its bundle —
//     the tools the workflow itself needs (a deploy step's `crane`);
//   - the TARGET REPO's `devbox.json` at the workspace root — the
//     toolchain that repo pins to build itself.
//
// The sandbox images are based on `jetpackio/devbox`, so devbox and Nix
// are already in the container: this is plumbing, not packaging.
package runtime

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/internal/shellquote"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/store"
)

// bundleResourceDir returns the directory holding a bot's own resources —
// its devbox.json, skills, and anything else shipped beside the workflow.
//
// A packed .botz has a real bundle dir. A CATALOG bot does not: it is a plain
// .bot path, yet its resources sit next to main.bot exactly as a bundle's do.
// Keying only on the bundle meant every bundle-mounted resource silently never
// reached the bots that ship with iterion — which is most of them, and the ones
// the devbox support was written for.
func bundleResourceDir(b *bundle.Bundle, workflowPath string) string {
	if b != nil && b.Dir != "" {
		return b.Dir
	}
	if workflowPath == "" {
		return ""
	}
	dir := filepath.Dir(workflowPath)
	if dir == "." || dir == string(filepath.Separator) {
		// A bare "main.bot" with no directory would resolve to the process's
		// working directory, which is not the bot's own resource dir.
		return ""
	}
	return dir
}

const (
	// devboxConfigName is the devbox project manifest. Its presence is
	// the entire opt-in signal — no DSL field, no flag.
	devboxConfigName = "devbox.json"

	// devboxLockName pins resolved package versions beside the config.
	// Copied along with it so a locked project installs exactly what it
	// was authored against.
	devboxLockName = "devbox.lock"

	// devboxProfileBin is where `devbox install -c <dir>` materialises the
	// symlink farm holding every package <dir>'s config declares. Relative
	// to the project directory.
	devboxProfileBin = ".devbox/nix/profile/default/bin"

	// botDevboxDir is the writable in-container directory the bot's
	// devbox.json is copied to before installing. The bundle is
	// bind-mounted READ-ONLY and devbox writes `.devbox/` beside the
	// config it installs, so installing from the mount is impossible.
	botDevboxDir = "/tmp/iterion-devbox/bot"

	// fallbackContainerPATH is the base the devbox bin dirs prepend to
	// when neither the workflow's `sandbox.env:` block nor a
	// devcontainer's containerEnv declares a PATH. It is the FHS default
	// every iterion sandbox image (Debian-derived) ships. An image with a
	// non-standard PATH declares `sandbox.env.PATH:` — that value is kept
	// as the suffix, so the prepend never drops an entry the author asked
	// for.
	fallbackContainerPATH = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
)

// devboxProject is one resolved devbox source: where its config lives
// inside the container and how it got there.
type devboxProject struct {
	// label names the source in logs and events ("repo" | "bot").
	label string

	// hostConfig is the host path of the devbox.json that triggered it,
	// reported so an operator can see what was picked up.
	hostConfig string

	// dir is the in-container project directory `devbox install -c`
	// targets; <dir>/.devbox/nix/profile/default/bin lands on PATH.
	dir string

	// stageFrom, when non-empty, is a read-only in-container directory the
	// config is copied out of into dir before installing.
	stageFrom string
}

// applyDevboxProvisioning wires every devbox source this run carries into
// the sandbox spec. A bot needing `crane` and a repo pinning its own
// toolchain compose — neither silently wins.
//
// Two halves, because the crux is the NON-INTERACTIVE PATH: tool nodes
// (and the agents' Bash tool) run through `sh -c`, which never sources a
// shell profile, so installing without exposing the binaries would be an
// invisible no-op.
//
//  1. INSTALL — a `devbox install` prologue prepended to Spec.PostCreate.
//  2. LOAD — each project's profile bin dir prepended to Spec.Env["PATH"],
//     which the driver applies at container creation, so every exec
//     inherits it without sourcing anything.
//
// Best-effort by construction, never silent: a failed or impossible
// install prints what is consequently missing on the container's stderr,
// warns host-side, and lets the run proceed. Latency is the reason it is
// opt-in by file presence — a cold `devbox install` resolves and realises
// Nix store paths and can take minutes; a run whose bot and repo declare
// no devbox.json adds nothing to PostCreate and pays nothing.
//
// Pre-conditions: spec is the resolved active spec, the bundle mount has
// already been added (bundleContainerPath is its resolved target, "" when
// absent), and applyHostStateMounts has run so Spec.WorkspaceFolder is
// settled. Callers gate on [sandbox.Capabilities.SupportsPostCreate].
func applyDevboxProvisioning(
	spec *sandbox.Spec,
	p SandboxParams,
	bundleContainerPath string,
	emitEvent func(store.EventType, map[string]any) error,
	logger *iterlog.Logger,
) {
	if spec == nil {
		return
	}
	projects := resolveDevboxProjects(spec, p, bundleContainerPath, logger)
	if len(projects) == 0 {
		return
	}

	binDirs := make([]string, 0, len(projects))
	labels := make([]string, 0, len(projects))
	configs := make([]string, 0, len(projects))
	for _, pr := range projects {
		binDirs = append(binDirs, path.Join(pr.dir, devboxProfileBin))
		labels = append(labels, pr.label)
		configs = append(configs, pr.hostConfig)
	}

	spec.PostCreate = joinShellSnippets(devboxInstallSnippet(projects), spec.PostCreate)

	if spec.Env == nil {
		spec.Env = map[string]string{}
	}
	base := spec.Env["PATH"]
	if base == "" {
		base = fallbackContainerPATH
	}
	spec.Env["PATH"] = strings.Join(binDirs, ":") + ":" + base

	if logger != nil {
		logger.Info("runtime: sandbox devbox: provisioning %s from %s — `devbox install` at container start, %s prepended to PATH (a cold Nix realise can take minutes)",
			strings.Join(labels, "+"), strings.Join(configs, ", "), strings.Join(binDirs, ", "))
	}
	_ = emitEvent(store.EventSandboxDevboxProvisioned, map[string]any{
		"sources":  labels,
		"configs":  configs,
		"bin_dirs": binDirs,
		"path":     spec.Env["PATH"],
	})
}

// resolveDevboxProjects lists the devbox sources this run carries, in
// PATH-precedence order: the target repo first, then the bot. A repo that
// pins its own toolchain stays authoritative for building itself; the
// bot's packages fill in what the repo does not provide.
func resolveDevboxProjects(
	spec *sandbox.Spec,
	p SandboxParams,
	bundleContainerPath string,
	logger *iterlog.Logger,
) []devboxProject {
	var out []devboxProject

	if cfg := devboxConfigIn(p.WorkspacePath, "workspace", logger); cfg != "" {
		// Installs in place: the workspace bind is read-write, and a
		// repo's devbox.json may reference sibling paths (`path:./flake`)
		// that only resolve next to it. devbox drops a self-ignoring
		// `.devbox/.gitignore`, so the generated profile stays invisible
		// to the repo's git — it cannot ride a `git add -A` onto a branch.
		out = append(out, devboxProject{
			label:      "repo",
			hostConfig: cfg,
			dir:        containerWorkspaceFolder(spec, p.WorkspacePath),
		})
	}
	if bundleContainerPath != "" {
		if cfg := devboxConfigIn(p.BundleHostDir, "bundle", logger); cfg != "" {
			out = append(out, devboxProject{
				label:      "bot",
				hostConfig: cfg,
				dir:        botDevboxDir,
				stageFrom:  bundleContainerPath,
			})
		}
	}

	kept := out[:0]
	for _, pr := range out {
		// A directory carrying a PATH separator or a control character
		// would poison Spec.Env["PATH"] (and the docker --env parser).
		// Drop it loudly rather than corrupt every command in the run.
		if strings.ContainsAny(pr.dir, ":\n\r\x00") {
			if logger != nil {
				logger.Warn("runtime: sandbox devbox: skipping the %s devbox.json (%s) — its container directory %q cannot go on PATH, so the tools it declares are NOT available to this run", pr.label, pr.hostConfig, pr.dir)
			}
			continue
		}
		kept = append(kept, pr)
	}
	return kept
}

// devboxConfigIn returns the host path of dir's devbox.json, or "" when
// dir is unset or carries none. A stat failure other than "not found" is
// surfaced: we could not tell whether tools were meant to come with this
// sandbox, and an operator staring at a missing binary deserves the clue.
func devboxConfigIn(dir, label string, logger *iterlog.Logger) string {
	if dir == "" {
		return ""
	}
	cfg := filepath.Join(dir, devboxConfigName)
	_, err := os.Stat(cfg)
	switch {
	case err == nil:
		return cfg
	case errors.Is(err, fs.ErrNotExist):
		return ""
	default:
		if logger != nil {
			logger.Warn("runtime: sandbox devbox: cannot stat the %s %s at %s: %v — treating it as absent", label, devboxConfigName, cfg, err)
		}
		return ""
	}
}

// containerWorkspaceFolder is the in-container path the host workspace is
// bind-mounted at. Mirrors the docker driver's rule: an explicit
// Spec.WorkspaceFolder wins, an empty one means "the same absolute path as
// on the host", which keeps ${PROJECT_DIR} and Claude Code's cwd-derived
// project key resolving identically in and out of the container.
func containerWorkspaceFolder(spec *sandbox.Spec, hostWorkspace string) string {
	if spec != nil && spec.WorkspaceFolder != "" {
		return spec.WorkspaceFolder
	}
	abs, err := filepath.Abs(hostWorkspace)
	if err != nil {
		return hostWorkspace
	}
	return abs
}

// devboxInstallSnippet renders the `devbox install` prologue.
//
// Every failure path names what is consequently missing and then returns
// success: a sandbox that boots without one tool is recoverable, one that
// refuses to boot is not — but silence is not on the menu either, since a
// missing binary would otherwise read as an agent bug.
//
// POSIX sh only (no `[[ ]]`, no brace expansion, no `<<<`): PostCreate
// runs through `sh -c`, which is dash on the Debian-based sandbox images.
func devboxInstallSnippet(projects []devboxProject) string {
	labels := make([]string, 0, len(projects))
	for _, pr := range projects {
		labels = append(labels, pr.label)
	}

	var b strings.Builder
	b.WriteString("# iterion: devbox provisioning\n")
	b.WriteString("if command -v devbox >/dev/null 2>&1; then\n")
	// devbox prompts before writing a lockfile on some paths; a
	// post-create hook has nobody to answer.
	b.WriteString("  DEVBOX_NO_PROMPT=1; export DEVBOX_NO_PROMPT\n")
	for _, pr := range projects {
		dir := shellquote.Quote(pr.dir)
		fail := shellquote.Quote(fmt.Sprintf(
			"iterion: devbox install failed for the %s %s (%s) — the packages it declares are NOT on PATH for this run",
			pr.label, devboxConfigName, pr.dir))
		if pr.stageFrom == "" {
			fmt.Fprintf(&b, "  devbox install -c %s || echo %s >&2\n", dir, fail)
			continue
		}
		// Staged copy out of the read-only bundle mount. The lock is
		// optional — a bundle that ships none installs unlocked.
		src := shellquote.Quote(pr.stageFrom)
		fmt.Fprintf(&b,
			"  { mkdir -p %s && cp %s/%s %s/%s && { cp %s/%s %s/%s 2>/dev/null || true; } && devbox install -c %s; } || echo %s >&2\n",
			dir,
			src, devboxConfigName, dir, devboxConfigName,
			src, devboxLockName, dir, devboxLockName,
			dir, fail)
	}
	b.WriteString("else\n")
	fmt.Fprintf(&b, "  echo %s >&2\n", shellquote.Quote(fmt.Sprintf(
		"iterion: devbox is not on PATH in this sandbox image — the %s %s was ignored and the packages it declares are unavailable",
		strings.Join(labels, "+"), devboxConfigName)))
	b.WriteString("fi")
	return b.String()
}

// joinShellSnippets concatenates PostCreate fragments with a newline so
// each keeps its own shell semantics — a bot snippet opening with `set -e`
// governs only what follows it, and cannot abort the fragments before it.
// Blank fragments drop out, so a spec with no PostCreate of its own gets
// the prologue alone.
func joinShellSnippets(parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, s := range parts {
		if strings.TrimSpace(s) == "" {
			continue
		}
		kept = append(kept, strings.TrimRight(s, "\n"))
	}
	return strings.Join(kept, "\n")
}
