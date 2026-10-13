package delegate

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
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

// codexSpawnEnv is the environment of a node's codex spawns: the credential
// environment, and — when the policy keeps the operator out — a CODEX_HOME
// that serves everything of the effective home except its instruction files.
// codex's project_doc_max_bytes only governs the repository's AGENTS.md; the
// home's own AGENTS.md loads regardless, so withholding it means moving the
// home. release must run once both passes are done.
func codexSpawnEnv(ctx context.Context, task Task) (env map[string]string, release func() error, err error) {
	env = codexCredEnvForCLI(ctx)
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
