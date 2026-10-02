package model

import (
	"path/filepath"

	clawrt "github.com/SocialGouv/claw-code-go/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/backend/ambient"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
)

// clawAmbientMaxBytes lifts claw-code-go's 48 KiB memory cap: the cap
// truncates from the END of the corpus, and the workspace's own files load
// last, so a repository with a large CLAUDE.md would lose exactly its own
// instructions to the operator's rules under `all`. 4 MiB is past any real
// instruction corpus; the model's own context limit is the honest bound.
const clawAmbientMaxBytes = 4 << 20

// clawAmbientSystemContext renders the project-instructions block the node's
// ambient-context policy (ADR-119) allows, with claw-code-go's own loader —
// walk-up and imports, `.claude/rules`, workspace imports confined to the
// boundary. Empty for `none`, whose contract is nothing but the prompt. The
// caller appends it as one more cacheable system block; the container runner
// takes the same path, since it rebuilds the Task and runs the same Execute.
func clawAmbientSystemContext(task delegate.Task) string {
	if task.WorkDir == "" || !task.AmbientContext.IncludesWorkspace() && !task.AmbientContext.IncludesOperator() {
		return ""
	}
	cfg := clawrt.MinimalPromptConfig()
	cfg.ProjectInstructions = true
	cfg.MemoryWalkUp = true
	cfg.MemoryImports = true
	cfg.MemoryMaxBytes = clawAmbientMaxBytes
	cfg.MemoryClaudeCodeLayout = true
	root := ambient.RepoRoot(task.WorkDir)
	switch task.AmbientContext {
	case ambient.Operator:
		// The main checkout of a nested worktree is the same repository: the
		// boundary moves up to it, so its files stay repository content.
		cfg.MemoryRoot = ambient.OperatorBoundary(root)
		cfg.MemorySkipWorkspace = true
	case ambient.All:
		// Native parity: the walk runs to the filesystem root, the user
		// scope included.
	default:
		cfg.MemoryRoot = root
		cfg.MemorySkipUser = true
		cfg.MemorySkipOuter = true
	}
	return clawrt.BuildSystemContext(filepath.Clean(task.WorkDir), cfg)
}
