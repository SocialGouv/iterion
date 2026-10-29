// Package runtime — the operator isolation floor (#1959).
//
// A workflow-level `sandbox: none` is honoured whatever the bot's source
// is, so without a floor the DEPLOYMENT's isolation posture is decided by
// the bot text. Cloud launches resolve their bot through one authority
// (pkg/server/bot_resolver.go) that already knows which tier served the
// launch — team (a row the launching TEAM wrote in the studio editor),
// platform (the deployment operator's override), baked (the catalog the
// operator shipped in the image). A tier the operator does not control
// must not get to switch the sandbox off: its opt-out is neutralized,
// loudly, and mode resolution falls through to the operator's own tiers
// (the CLI-strength override, then the global default).
//
// The ONE decision lives in [ApplySandboxFloor]; the engine applies it at
// run start (applySandboxFloor, before admission so the admission probe
// and the sandbox start judge the same workflow), and the cloud runner
// applies it to its own pre-engine sandbox predictions (file-secret
// materialization, the usage-cap ladder) so every side of the boundary
// agrees.
package runtime

import (
	"context"
	"os"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/store"
)

// SandboxFloorEnvVar is the greppable escape hatch philosophy §1 requires
// for a load-bearing limit: ITERION_SANDBOX_FLOOR=off (also none/0/false)
// disables the floor and honours every source's `sandbox: none` again —
// the pre-#1959 behaviour, for the operator who trusts their team tier.
const SandboxFloorEnvVar = "ITERION_SANDBOX_FLOOR"

// sandboxFloorDisabled reports whether the operator switched the floor
// off. Read per call so a process-level change between attempts (a runner
// restart is not required to reason about a single run) reads one answer.
func sandboxFloorDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(SandboxFloorEnvVar))) {
	case "off", "none", "0", "false":
		return true
	}
	return false
}

// operatorTrustedBotSource reports whether the given bot-resolution tier
// (store.BotSourceTier*) speaks for the deployment operator. The platform
// override and the baked catalog do — the operator wrote one and shipped
// the other. The empty tier is the operator's own launch (a local `iterion
// run` of a file they chose, or a run document that predates the stamp):
// nothing about it is attacker-controlled, so it is trusted. Everything
// else — the team tier today, any tier a future resolver adds — is NOT:
// the floor fails closed on a vocabulary it does not recognise.
func operatorTrustedBotSource(tier string) bool {
	switch tier {
	case "", store.BotSourceTierPlatform, store.BotSourceTierBaked:
		return true
	}
	return false
}

// ApplySandboxFloor enforces the operator isolation floor on a workflow's
// declared sandbox mode: a `sandbox: none` declared by a bot source the
// operator does not control is neutralized — the returned workflow is a
// copy whose sandbox block no longer carries a mode, so pickMode's chain
// falls through to the operator's own tiers (CLI override, global
// default). Returns the input unchanged with neutralized=false when the
// floor does not speak:
//
//   - the workflow declares no opt-out (nothing to neutralize);
//   - the source IS operator-trusted (operatorTrustedBotSource) — the
//     catalog's deliberate opt-outs, bots/copilot being the case that
//     must keep working;
//   - a CLI-strength override is set — the operator's explicit choice is
//     never silently replaced (it already beats the workflow block);
//   - the operator disabled the floor (SandboxFloorEnvVar).
//
// Callers that predict a run's sandbox posture WITHOUT an engine (the
// cloud runner's file-secret materialization and usage-cap ladder) must
// apply this to the workflow they probe with, or the prediction and the
// engine's start disagree on exactly the runs the floor exists for.
func ApplySandboxFloor(wf *ir.Workflow, botSourceTier, cliOverride string) (effective *ir.Workflow, neutralized bool) {
	if wf == nil || wf.Sandbox == nil || wf.Sandbox.Mode != string(sandbox.ModeNone) {
		return wf, false
	}
	if operatorTrustedBotSource(botSourceTier) || cliOverride != "" || sandboxFloorDisabled() {
		return wf, false
	}
	out := *wf
	block := *wf.Sandbox
	block.Mode = ""
	out.Sandbox = &block
	return &out, true
}

// applySandboxFloor is the engine's application of the floor, run once per
// attempt right after the run document resolves and BEFORE admission: the
// admission's sandbox probe (RunWillBeSandboxed) and startSandbox must
// judge the SAME workflow, so the neutralized copy replaces e.workflow for
// the whole attempt. The tier is the wire-carried launch fact
// (WithBotSourceTier) when present, else the run document's stamp — the
// two carriers v28 ships together; a local launch has neither and reads as
// the operator's own.
//
// Neutralization is loud by design: a warn in the run log and a
// sandbox_floor_applied event naming the tier, what was overridden and the
// escape hatch — never a silent upgrade.
func (e *Engine) applySandboxFloor(ctx context.Context, runID string, run *store.Run) {
	tier := e.botSourceTier
	if tier == "" && run != nil {
		tier = run.BotSourceTier
	}
	floored, neutralized := ApplySandboxFloor(e.workflow, tier, e.sandboxOverride)
	if !neutralized {
		return
	}
	e.workflow = floored
	if e.logger != nil {
		e.logger.Warn("runtime: run %s: sandbox floor (#1959) neutralized the workflow's `sandbox: none` — bot source tier %q is not operator-controlled, so isolation falls to the operator's own tiers (%s=off restores the opt-out)", runID, tier, SandboxFloorEnvVar)
	}
	_ = e.emit(ctx, runID, store.EventSandboxFloorApplied, "", map[string]any{
		"tier":            tier,
		"overridden_mode": string(sandbox.ModeNone),
		"reason":          "bot source the operator does not control declared a sandbox opt-out; the run resolves its sandbox from the operator's tiers instead",
		"escape_hatch":    SandboxFloorEnvVar + "=off",
	})
}
