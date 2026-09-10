package runner

import (
	"context"
	"errors"
	"fmt"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/internal/appinfo"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ErrBotRequiresNewerEngine marks a run whose bundle declares an engine floor
// (`requires.iterion`) this build is below. Deterministic for this pod AND for
// every other pod of the same image, so the delivery is acked, not naked: a
// redelivery reaches the same arithmetic, burns the delivery budget on it, and
// parks the run on the DLQ with the diagnosis overwritten by DLQ_PARKED.
//
// The shape it closes: a bot pushed as a platform override (or baked into a
// newer image than the runners run) used builtins the evaluator did not have.
// The workflow COMPILED — a function call parses generically — then failed at
// its first evaluation, seven pods in ten minutes.
var ErrBotRequiresNewerEngine = errors.New("the bot requires a newer iterion engine than this runner")

// engineBuild names the build a bundle's requirement is held against. A var so
// a test can pin it: `go test` binaries carry appinfo.Version = "dev", which is
// deliberately unorderable, so the guard would otherwise be permanently
// inconclusive under test.
var engineBuild = appinfo.FullVersion

// guardEngineRequirement is the ONE point every bundle a run executes with
// passes through — stored (a `bot_bundle` ref rebuilt from the DB) or baked
// (the runner image's own catalog). It reads the bundle's declared engine
// floor and, when this build is below it, writes the terminal verdict on the
// run and returns ErrBotRequiresNewerEngine.
//
// Three outcomes, and the middle one is the point:
//   - met, or nothing declared → nil, silently;
//   - BELOW the floor → refused here, before any node executes, so nothing
//     half-done is left behind and the operator reads the arithmetic instead
//     of an expression error three nodes in;
//   - UNDECIDABLE (this build carries no orderable version — a `dev` build, a
//     fork's naming scheme) → a WARN and the run proceeds. Refusing would stop
//     every developer build; passing in silence would make the declaration a
//     decoration.
func (r *Runner) guardEngineRequirement(ctx context.Context, msg *queue.RunMessage, b *bundle.Bundle) error {
	if b == nil || b.Manifest == nil {
		return nil
	}
	build := engineBuild()
	verdict, reason := bundle.CheckManifestEngine(b.Manifest, build)
	switch verdict {
	case bundle.EngineOK:
		return nil
	case bundle.EngineUnknown:
		r.cfg.Logger.Warn("runner: run %s: bot %q: %s — the run proceeds unchecked", msg.RunID, msg.BotID, reason)
		return nil
	}
	err := fmt.Errorf("%w: bot %q: %s", ErrBotRequiresNewerEngine, msg.BotID, reason)
	r.failBotRequiresNewerEngine(ctx, msg, b.Manifest, build, err)
	return err
}

// failBotRequiresNewerEngine records the refusal on the run: a TERMINAL failed
// with the typed code, plus a run_failed event carrying the pair an operator
// needs to act — what the bot asked for, and what this pod is.
//
// Terminal rather than failed_resumable, unlike the IR-unloadable sibling: a
// resume re-queues onto the same fleet and re-reads the same manifest, so the
// only cures are a deploy or a bot edit, and after either the run is
// RE-LAUNCHED (the fixed bot's IR is not the one this run would resume with).
func (r *Runner) failBotRequiresNewerEngine(ctx context.Context, msg *queue.RunMessage, m *bundle.Manifest, build string, cause error) {
	if r.cfg.Store == nil {
		return
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), parkStoreOpTimeout)
	defer cancel()
	idCtx := store.WithIdentity(wctx, msg.TenantID, msg.OwnerID)
	required := ""
	if m.Requires != nil {
		required = m.Requires.Iterion
	}
	// UpdateRunOutcome keeps the cancelled-wins guard: a run the operator
	// cancelled meanwhile is not flipped back into a failure.
	if changed, err := r.cfg.Store.UpdateRunOutcome(idCtx, msg.RunID, store.RunStatusFailed, cause.Error(),
		store.RunOutcomeMeta{Code: store.FailureBotRequiresNewerEngine, Continuation: store.ContinuationFinal},
		store.RunnerVerdictFromStatuses()); err != nil {
		r.cfg.Logger.Warn("runner: run %s: could not record the engine requirement refusal: %v", msg.RunID, err)
	} else if !changed {
		r.cfg.Logger.Warn("runner: run %s: the engine-requirement verdict was declined (status drifted) — the document does not carry BOT_REQUIRES_NEWER_ENGINE", msg.RunID)
	}
	if _, err := r.cfg.Store.AppendEvent(idCtx, msg.RunID, store.Event{
		Type: store.EventRunFailed,
		Data: map[string]any{
			"code": string(store.FailureBotRequiresNewerEngine), "error": cause.Error(),
			"runner_version": build, "runner_commit": appinfo.Commit,
			"bot":      msg.BotID,
			"required": required,
			"hint":     "the bundle declares requires.iterion above this runner's build; bump the runner image (docs/cloud-deployment.md § pinning), or relax the bot's requirement — then RE-LAUNCH (a resume lands on the same fleet)",
		},
	}); err != nil {
		r.cfg.Logger.Warn("runner: run %s: could not emit run_failed for the engine requirement: %v", msg.RunID, err)
	}
}
