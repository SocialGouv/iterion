package runtime

import (
	"os"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// WorkspaceCheckpointEnabled reports whether a run's copy-based sandbox gets
// the mid-run workspace net, resolving the precedence chain the rest of the
// engine uses: workflow block → ITERION_WORKSPACE_CHECKPOINT → on.
//
// Default ON: the net exists because a pod that dies hard takes with it every
// commit the run had not yet exported, and most bots exist to produce those
// commits.
//
// The escape hatch is for the run that produces NONE. An auditor or a
// reviewer reads a repository and writes its findings to node outputs, which
// are durable in the store on their own; the checkpoint would preserve
// nothing that is not already safe. What it does instead is `git add -A` the
// pod's tree — bot scratch directories included, since they are untracked and
// nothing ignores them — and force-push it as `iterion/run-<id>-checkpoint`
// to the run's own remote, which is the repository the operator pointed the
// bot AT. Measured 2026-09-08: a review bot's `.review-pr/findings.md`, an
// internal artifact whose board posting had been deliberately disabled, was
// published that way to a public repository and stayed there 29 hours.
//
// So this switch is not a cost dial like repo_devbox's. It answers a question
// only the bot can answer — "do I write commits for the repo I was given?" —
// and a bot that answers no should not be pushing branches to it.
//
// Read from the compiled workflow, which travels to the pod inside the run
// message: a runner never re-decides this from its own environment when the
// bot has stated it.
func WorkspaceCheckpointEnabled(wf *ir.Workflow) bool {
	if wf != nil {
		if v, ok := parseOnOff(wf.WorkspaceCheckpoint); ok {
			return v
		}
	}
	if v, ok := parseOnOff(os.Getenv("ITERION_WORKSPACE_CHECKPOINT")); ok {
		return v
	}
	return true
}
