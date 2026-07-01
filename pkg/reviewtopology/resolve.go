// Package reviewtopology resolves the mono/dual review topology for
// bi-model review-loop bots (whole-improve-loop, branch-improve-loop,
// feature-dev, docs-refresh, secured-renovacy) from detected provider
// credentials, and injects the resolved review_mode / mono_family vars
// into a run's inputs when the target workflow opts in by declaring a
// review_mode var.
//
// Background: those bots alternate two model families ("claude" ↔ "gpt")
// each review pass. That DUAL topology is robust but requires two provider
// families and pays for both. MONO runs a single family (~half the calls),
// trading cross-model verification for frugality. The topology cannot be
// resolved inside the DSL (a bot can't probe host credentials), so it is
// resolved here — out of band, at launch — and injected as vars the bot's
// router edges and stop condition read via {{vars.review_mode}} /
// {{vars.mono_family}}.
package reviewtopology

import (
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/detect"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// Var names injected into a run's inputs. A bot opts into topology
// resolution by declaring VarReviewMode in its vars: block.
const (
	VarReviewMode = "review_mode"
	VarMonoFamily = "mono_family"
)

// Topology modes. ModeAuto is the pre-resolution default a bot ships with;
// Resolve never returns it (it collapses auto → mono|dual).
const (
	ModeAuto = "auto"
	ModeMono = "mono"
	ModeDual = "dual"
)

// Review families. Decision (operator): {anthropic, zai} are the SAME
// family "claude" — there is no true cross-family verification between
// them — and {openai} is "gpt". Cloud providers (foundry/bedrock/vertex)
// are out of scope for v1 and do not participate in topology resolution.
const (
	FamilyClaude = "claude"
	FamilyGPT    = "gpt"
)

// familyOf maps a detect provider name to its review family, or "" for
// providers that don't participate in the mono/dual topology.
func familyOf(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "anthropic", "zai":
		return FamilyClaude
	case "openai":
		return FamilyGPT
	default:
		return ""
	}
}

// monoPreference is the order a single family is picked in when resolving
// mono: prefer claude, then gpt. (Parameterisable later if needed.)
var monoPreference = []string{FamilyClaude, FamilyGPT}

// availableFamilies returns the set of distinct review families backed by
// an available provider credential in the detection report.
func availableFamilies(rep detect.Report) map[string]bool {
	fams := make(map[string]bool, 2)
	for _, p := range rep.Providers {
		if !p.Available {
			continue
		}
		if f := familyOf(p.Name); f != "" {
			fams[f] = true
		}
	}
	return fams
}

// preferredFamily picks the mono family from the available set following
// monoPreference. Returns "" when no participating family is available.
func preferredFamily(fams map[string]bool) string {
	for _, f := range monoPreference {
		if fams[f] {
			return f
		}
	}
	return ""
}

// Resolve computes the effective review topology from a detection report
// and an operator override.
//
// override is one of "dual", "mono", or "auto" (also "" / any unknown →
// auto):
//   - "dual"      → always dual; monoFamily is "".
//   - "mono"      → always mono; monoFamily is the preferred available
//     family (claude first), or "" when none is detected (the run will
//     then fail loudly on the missing credential, same as today).
//   - "auto"/""   → dual when ≥2 families are available; mono on the single
//     available family otherwise; dual (no downgrade) when no participating
//     family is detected, so an unconfigured host fails the normal way
//     instead of on an empty router.
//
// The returned mode is always concrete ("mono" or "dual"), never "auto".
func Resolve(rep detect.Report, override string) (mode string, monoFamily string) {
	fams := availableFamilies(rep)
	switch strings.ToLower(strings.TrimSpace(override)) {
	case ModeDual:
		return ModeDual, ""
	case ModeMono:
		return ModeMono, preferredFamily(fams)
	default: // auto / "" / unknown
		if len(fams) >= 2 {
			return ModeDual, ""
		}
		if f := preferredFamily(fams); f != "" {
			return ModeMono, f
		}
		return ModeDual, ""
	}
}

// declaresVar reports whether the workflow declares the named var. This is
// the opt-in gate: only bots that declare review_mode get topology vars
// injected — everything else is left untouched.
func declaresVar(wf *ir.Workflow, name string) bool {
	if wf == nil || wf.Vars == nil {
		return false
	}
	_, ok := wf.Vars[name]
	return ok
}

// InjectIfDeclared resolves the review topology and writes review_mode +
// mono_family into inputs, but ONLY when the workflow opts in by declaring
// a review_mode var. Bots that don't use the topology are left untouched.
//
// The override precedence is: an explicit flagOverride (e.g. the CLI
// --review-mode flag) wins; otherwise a concrete value already in
// inputs[review_mode] (from --var / a preset) is used; otherwise "auto".
// A bot's own default of "auto" therefore triggers auto-detection, while
// an operator asking for "mono"/"dual" is honoured verbatim.
//
// Returns the resolved (mode, monoFamily) and whether injection happened
// (for logging by the caller).
func InjectIfDeclared(wf *ir.Workflow, inputs map[string]interface{}, rep detect.Report, flagOverride string) (mode, monoFamily string, injected bool) {
	if !declaresVar(wf, VarReviewMode) {
		return "", "", false
	}
	override := strings.TrimSpace(flagOverride)
	if override == "" && inputs != nil {
		if v, ok := inputs[VarReviewMode]; ok {
			if s, ok := v.(string); ok {
				override = strings.TrimSpace(s)
			}
		}
	}
	mode, monoFamily = Resolve(rep, override)
	if inputs != nil {
		inputs[VarReviewMode] = mode
		inputs[VarMonoFamily] = monoFamily
	}
	return mode, monoFamily, true
}
