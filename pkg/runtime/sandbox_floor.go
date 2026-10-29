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
	"strings"

	"github.com/SocialGouv/iterion/internal/envtrust"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/store"
)

// SandboxFloorEnvVar is the greppable escape hatch philosophy §1 requires
// for a load-bearing limit: ITERION_SANDBOX_FLOOR=off (also none/0/false)
// disables the floor and honours every source's `sandbox: none` again —
// the pre-#1959 behaviour, for the operator who trusts their team tier.
const SandboxFloorEnvVar = "ITERION_SANDBOX_FLOOR"

// isolationHatchOff is the ONE reader of an isolation escape hatch —
// SandboxFloorEnvVar and [HostStateGuardEnvVar], the two variables that can
// switch an isolation control off. It reports whether the hatch is open, and
// whether a project `.env` is what set it.
//
// INHERITED, not live: which isolation an agent runs under is an authority
// decision, and iterion fills unset variables from the nearest `.env` walking
// up from the working directory (cmd/iterion applyDotEnv, no denylist) — a
// file that sits in the repository under review, which the sandboxed agent of
// a previous run had as its workspace. Read with os.Getenv, both hatches
// disarm themselves from inside the blast radius: the agent writes
// `ITERION_SANDBOX_FLOOR=off` into the repository's `.env` and the operator's
// next `iterion run` from that checkout honours the bot's own `sandbox: none`.
// envtrust.Inherited returns the empty string for a planted value, which
// every hatch reads as "the operator said nothing" — the closed answer.
//
// planted is returned rather than logged here: the warn belongs where a run
// logger exists and only where the hatch would have CHANGED the outcome, so
// the line names a consequence instead of a setting.
func isolationHatchOff(name string) (off, planted bool) {
	planted = envtrust.Planted(name)
	switch strings.ToLower(strings.TrimSpace(envtrust.Inherited(name))) {
	case "off", "none", "0", "false":
		off = true
	}
	return off, planted
}

// plantedHatchWarning is the one sentence both hatches say when a `.env`
// planted them: what was ignored, and that the operator's own environment is
// the only place the hatch is read from.
func plantedHatchWarning(name string) string {
	return name + " was set by a project .env, not by the operator — the hatch is IGNORED and the isolation control stays on; export it in the operator's own environment to open it"
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
// engine's start disagree on exactly the runs the floor exists for. One
// class of predictor cannot, and says so at its own site: a publisher
// screen reachable from a credential PREVIEW, which runs before any source
// is resolved and therefore holds no tier to judge
// (pkg/server/cloudpublisher). Such a screen is advisory, the runner's is
// authoritative, and a reader must find the asymmetry written where the
// reading is made — never infer agreement from this contract.
func ApplySandboxFloor(wf *ir.Workflow, botSourceTier, cliOverride string) (effective *ir.Workflow, neutralized bool) {
	if wf == nil || wf.Sandbox == nil || wf.Sandbox.Mode != string(sandbox.ModeNone) {
		return wf, false
	}
	floorOff, _ := isolationHatchOff(SandboxFloorEnvVar)
	if operatorTrustedBotSource(botSourceTier) || cliOverride != "" || floorOff {
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
	payload := map[string]any{
		"tier":            tier,
		"overridden_mode": string(sandbox.ModeNone),
		"reason":          "bot source the operator does not control declared a sandbox opt-out; the run resolves its sandbox from the operator's tiers instead",
		"escape_hatch":    SandboxFloorEnvVar + "=off",
		// What "falls to the operator's own tiers" is NOT a promise of
		// isolation, and an operator reads this event as one: the default
		// tier is `auto`, and `auto` on a host with no container runtime
		// degrades to an unsandboxed run with its own sandbox_skipped
		// event (#1425). The floor removes the bot's word on the question;
		// the run's isolation is settled by sandbox_started /
		// sandbox_skipped, which is where to read it.
		"isolation": "resolved from the operator's tiers, not guaranteed: a `auto` default still degrades to an unsandboxed run (sandbox_skipped) on a host with no container runtime — sandbox_started/sandbox_skipped is the authority",
	}
	// A `.env` in the repository under review cannot open the hatch
	// (isolationHatchOff reads the inherited environment), and the one run
	// where that mattered says so: the floor fired against a planted
	// opt-out.
	if _, planted := isolationHatchOff(SandboxFloorEnvVar); planted {
		if e.logger != nil {
			e.logger.Warn("runtime: %s (run %s)", plantedHatchWarning(SandboxFloorEnvVar), runID)
		}
		payload["hatch_ignored"] = "planted by a project .env, not exported by the operator"
	}
	_ = e.emit(ctx, runID, store.EventSandboxFloorApplied, "", payload)
}
