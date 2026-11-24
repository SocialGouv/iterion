package delegate

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"

	"github.com/SocialGouv/iterion/pkg/internal/proc"
)

// codexInstructionFiles are the operator-level instruction files codex reads
// from CODEX_HOME, whatever the working directory.
var codexInstructionFiles = []string{"AGENTS.md", "AGENTS.override.md"}

// codexOverlayDirName holds the per-spawn homes codexHomeWithoutInstructions
// builds inside the operator's own CODEX_HOME.
const codexOverlayDirName = ambientOverlayDirName

// codexAmbientConfig is the `-c` part of the node's ambient-context policy
// (ADR-119): without the workspace, codex stops reading the repository's
// AGENTS.md files (measured on codex-cli 0.156.1).
func codexAmbientConfig(task Task) map[string]string {
	if task.AmbientContext.IncludesWorkspace() {
		return nil
	}
	return map[string]string{"project_doc_max_bytes": "0"}
}

// codexConfig is the one `-c` map of a spawn. codexsdk.WithConfig replaces
// rather than merges, so every key the engine sets is composed here.
func codexConfig(task Task, webSearch string) map[string]string {
	cfg := map[string]string{"web_search": webSearch}
	maps.Copy(cfg, codexAmbientConfig(task))
	return cfg
}

// codexConfiguredMCPServers lists the enabled MCP servers codex's own
// configuration gives a spawn: the CLI's `mcp list --json` under the spawn's
// environment (its CODEX_HOME) and in its working directory (a trusted
// project's `.codex/config.toml`), so every layer codex reads is read by
// codex.
func codexConfiguredMCPServers(ctx context.Context, cli string, env map[string]string, workDir string) ([]string, error) {
	listCtx, cancel := context.WithTimeout(ctx, codexWebSearchVersionTimeout)
	defer cancel()
	cmd := exec.CommandContext(listCtx, cli, "mcp", "list", "--json")
	cmd.Dir = workDir
	cmd.Env = os.Environ()
	for _, k := range slices.Sorted(maps.Keys(env)) {
		cmd.Env = append(cmd.Env, k+"="+env[k])
	}
	proc.SetWaitDelay(cmd, codexVersionDrainBound)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("delegate: codex: list the MCP servers its configuration starts (`%s mcp list --json`): %w", cli, err)
	}
	var servers []struct {
		Name    string `json:"name"`
		Enabled bool   `json:"enabled"`
	}
	if err := json.Unmarshal(out, &servers); err != nil {
		return nil, fmt.Errorf("delegate: codex: read `mcp list --json`: %w", err)
	}
	var names []string
	for _, s := range servers {
		if s.Enabled {
			names = append(names, s.Name)
		}
	}
	slices.Sort(names)
	return names, nil
}

// codexSpawnEnv is the environment of a node's codex spawns: the credential
// environment, and — when the policy keeps the operator out — a CODEX_HOME
// that serves everything of the effective home except its instruction files.
// codex's project_doc_max_bytes only governs the repository's AGENTS.md; the
// home's own AGENTS.md loads regardless, so withholding it means moving the
// home. release must run once both passes are done.
func codexSpawnEnv(ctx context.Context, task Task) (env map[string]string, release func() error, err error) {
	env = codexCredEnvForCLI(ctx)
	if task.WorkDir != "" {
		// The SDK owns codex's exec.Cmd, so the gesture every host spawn of
		// an agent CLI crosses (proc.SetWorkDir) travels through its
		// environment option: the child's PWD names the workspace the SDK is
		// also handed as cwd, not the engine's checkout. The SDK appends
		// this map after os.Environ(), and os/exec keeps the last value of a
		// duplicate key.
		if env == nil {
			env = map[string]string{}
		}
		env[proc.WorkDirEnvKey] = task.WorkDir
	}
	noop := func() error { return nil }
	if task.AmbientContext.IncludesOperator() {
		return env, noop, nil
	}
	home := env["CODEX_HOME"]
	if home == "" {
		home = os.Getenv("CODEX_HOME")
	}
	if home == "" {
		userHome, herr := os.UserHomeDir()
		if herr != nil {
			return nil, nil, fmt.Errorf("delegate: codex: locate the operator's codex home: %w", herr)
		}
		home = filepath.Join(userHome, ".codex")
	}
	dir, release, err := codexHomeWithoutInstructions(home)
	if err != nil {
		return nil, nil, err
	}
	if dir == home {
		return env, release, nil
	}
	out := maps.Clone(env)
	if out == nil {
		out = map[string]string{}
	}
	out["CODEX_HOME"] = dir
	return out, release, nil
}

// codexHomeWithoutInstructions returns a directory that serves every entry of
// home through a symlink except codex's instruction files
// (homeWithoutInstructions). The sessions directory is created in home first:
// the formatting pass resumes the session the work pass wrote there.
func codexHomeWithoutInstructions(home string) (dir string, release func() error, err error) {
	return homeWithoutInstructions(BackendCodex, home, codexInstructionFiles, []string{"sessions"})
}
