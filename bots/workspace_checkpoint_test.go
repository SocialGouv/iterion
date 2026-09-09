package bots

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/runtime"
)

// readOnlyBots are the catalog bots that READ the repository they are
// pointed at and write their conclusions to node outputs — never a commit
// for that repository. Adding one means adding it here, so the guard below
// covers it too.
var readOnlyBots = []string{
	"review-pr",
	"revi-converse",
	"sec-audit-source",
	"sec-audit-deps",
}

// committingBots are catalog bots whose whole contract is to leave commits
// in the repository they were given. They are the OTHER half of this guard:
// without them a resolver that answered "off" to everything would pass.
var committingBots = []string{
	"feature-dev",
	"branch-improve-loop",
	"whole-improve-loop",
}

// TestReadOnlyBotsDeclineTheWorkspaceCheckpoint pins `workspace_checkpoint:
// off` on every bot that does not commit to its target, through the REAL
// resolution chain — parse the shipped main.bot, compile it, and ask the
// engine's own resolver — rather than grepping for the line. A field renamed
// in the parser, dropped in the compiler or read from the wrong place would
// leave the text in the file and the guard green.
//
// What it protects: the mid-run workspace checkpoint force-pushes the pod's
// whole tree (`git add -A`, bot scratch dirs included — they are untracked
// and nothing ignores them) to `iterion/run-<id>-checkpoint` on the run's own
// remote, which is the repository the bot was pointed AT. Measured
// 2026-09-08: a review bot's `.review-pr/findings.md` — an internal artifact
// whose board posting had been deliberately disabled — reached a public
// repository that way and stayed 29 hours. For these bots the net also holds
// nothing: they make no commit, and their outputs are durable in the store.
func TestReadOnlyBotsDeclineTheWorkspaceCheckpoint(t *testing.T) {
	for _, bot := range readOnlyBots {
		t.Run(bot, func(t *testing.T) {
			wf := compileBot(t, bot)
			if wf == nil {
				t.Fatalf("%s did not compile", bot)
			}
			if runtime.WorkspaceCheckpointEnabled(wf) {
				t.Fatalf("%s reads its target and commits nothing to it, but resolves workspace_checkpoint ON: "+
					"every run would force-push a branch carrying its findings — its own scratch dir included — "+
					"to the repository it audits. Declare `workspace_checkpoint: off` on its workflow block.", bot)
			}
		})
	}
}

// TestCommittingBotsKeepTheWorkspaceCheckpoint is the second face: a bot that
// DOES commit must keep its net. A pod that dies hard takes with it every
// commit not yet exported, and that is the loss the checkpoint exists for —
// so this guard fails just as loudly on an over-broad `off` as its sibling
// does on a missing one.
func TestCommittingBotsKeepTheWorkspaceCheckpoint(t *testing.T) {
	for _, bot := range committingBots {
		t.Run(bot, func(t *testing.T) {
			wf := compileBot(t, bot)
			if wf == nil {
				t.Fatalf("%s did not compile", bot)
			}
			if !runtime.WorkspaceCheckpointEnabled(wf) {
				t.Fatalf("%s commits its work into the repository it is given, and a pod that dies hard "+
					"takes every unexported commit with it — it must NOT decline the workspace checkpoint", bot)
			}
		})
	}
}
