// Package reviewtopology resolves the mono/dual review topology for
// bi-model review-loop bots (whole-improve-loop, branch-improve-loop,
// feature-dev, docs-refresh, secured-renovacy) from detected credentials
// (provider keys AND usable backends — e.g. the claude_code forfait), and
// injects the resolved review_mode / mono_family vars
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
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/detect"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// Var names injected into a run's inputs. A bot opts into topology
// resolution by declaring VarReviewMode in its vars: block, and into
// plan-review resolution by declaring VarPlanReview.
const (
	VarReviewMode = "review_mode"
	VarMonoFamily = "mono_family"
	VarPlanReview = "plan_review"
	// VarLLMFamilies is the generic, role-free fact behind the two above: a
	// bot declaring it receives the sorted comma-joined list of review
	// families backed by a credential (e.g. "claude,gpt"). It exists so a
	// bot can build its OWN policy in a compute node without the engine
	// learning a new role var per pattern — the engine publishes the fact a
	// bot cannot probe (host/run credentials), nothing more.
	VarLLMFamilies = "llm_families"
)

// Topology modes. ModeAuto is the pre-resolution default a bot ships with;
// Resolve never returns it (it collapses auto → mono|dual).
const (
	ModeAuto = "auto"
	ModeMono = "mono"
	ModeDual = "dual"
)

// Plan-review modes. PlanReviewAuto is the pre-resolution default a bot
// ships with; ResolvePlanReview never returns it (it collapses auto →
// on|off from the available families).
const (
	PlanReviewAuto = "auto"
	PlanReviewOn   = "on"
	PlanReviewOff  = "off"
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

// familyOfBackend maps an available backend to the review family its
// nodes run on, or "" for backends that don't imply a family. This is what
// lets a *backend* credential back a family even when no provider-style key
// is present: the claude_code forfait (OAuth in ~/.claude) is a backend
// cred, not an anthropic provider key, yet the bots' claude-family nodes
// (backend: claude_code) run on it. Without this, a forfait-only host detects
// no "claude" family, so mono/auto route to gpt and running claude models on
// the forfait needs a manual per-node backend override (see
// docs/bot-runs/whole-improve-loop.md, 2026-07-02). codex→gpt is included for
// symmetry (usually redundant with the openai provider probe). claw is
// provider-routed, so it backs no family on its own.
func familyOfBackend(backend string) string {
	switch strings.ToLower(strings.TrimSpace(backend)) {
	case detect.BackendClaudeCode:
		return FamilyClaude
	case detect.BackendCodex:
		return FamilyGPT
	default:
		return ""
	}
}

// monoPreference is the order a single family is picked in when resolving
// mono: prefer claude, then gpt. (Parameterisable later if needed.)
var monoPreference = []string{FamilyClaude, FamilyGPT}

// FamilySet is the set of distinct review families backed by an available
// credential — the input every resolution reads. Built from a host
// detection report (FamiliesFromReport) or assembled directly by a caller
// that knows the run's credentials from another source (the cloud
// publisher derives it from the sealed per-run bundle).
type FamilySet map[string]bool

// FamiliesFromReport returns the set of distinct review families backed by
// an available credential in the detection report — either a participating
// provider key (anthropic/zai→claude, openai→gpt) OR a usable backend whose
// nodes run that family (claude_code→claude, codex→gpt). The union is what
// makes a forfait-only host (claude_code OAuth, no anthropic key) resolve a
// "claude" family, so mono picks claude and auto can go dual.
func FamiliesFromReport(rep detect.Report) FamilySet {
	fams := make(FamilySet, 2)
	for _, p := range rep.Providers {
		if !p.Available {
			continue
		}
		if f := familyOf(p.Name); f != "" {
			fams[f] = true
		}
	}
	for _, b := range rep.Backends {
		if !b.Available {
			continue
		}
		if f := familyOfBackend(b.Name); f != "" {
			fams[f] = true
		}
	}
	return fams
}

// preferredFamily picks the mono family from the available set following
// monoPreference. Returns "" when no participating family is available.
func preferredFamily(fams FamilySet) string {
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
//   - "auto"/""   → MONO on the preferred available family. Dual costs one
//     full reviewer pass per family on every run, and auto is what every
//     unconfigured caller gets, so the default has to be the frugal one:
//     cross-family confirmation is a deliberate spend, not something a host
//     opts into merely by having two providers configured. Falls back to
//     dual only when no participating family is detected at all, so an
//     unconfigured host fails on the missing credential the normal way
//     instead of on an empty router.
//
// The returned mode is always concrete ("mono" or "dual"), never "auto".
func Resolve(rep detect.Report, override string) (mode string, monoFamily string) {
	return ResolveFamilies(FamiliesFromReport(rep), override)
}

// ResolveFamilies is Resolve on an already-assembled family set (see
// FamilySet for who assembles one without a detection report).
func ResolveFamilies(fams FamilySet, override string) (mode string, monoFamily string) {
	switch strings.ToLower(strings.TrimSpace(override)) {
	case ModeDual:
		return ModeDual, ""
	case ModeMono:
		return ModeMono, preferredFamily(fams)
	default: // auto / "" / unknown
		if f := preferredFamily(fams); f != "" {
			return ModeMono, f
		}
		return ModeDual, ""
	}
}

// ResolvePlanReview computes the effective cross-model plan-review switch
// from the available families and an operator override.
//
// override is one of "on", "off", or "auto" (also "" / any unknown → auto):
//   - "on"       → always on. Forcing it without a peer credential is
//     honoured verbatim: the peer node then fails loudly on the missing
//     credential instead of being silently skipped.
//   - "off"      → always off.
//   - "auto"/""  → on iff at least TWO distinct families are backed by a
//     credential (family-agnostic: whichever family the campaign runs on,
//     a peer from another family exists). Anything less means the pair
//     review cannot be a cross-model check, so the bot keeps its
//     plan-in-stride shape at zero extra cost.
//
// The returned mode is always concrete ("on" or "off"), never "auto".
func ResolvePlanReview(fams FamilySet, override string) string {
	switch strings.ToLower(strings.TrimSpace(override)) {
	case PlanReviewOn:
		return PlanReviewOn
	case PlanReviewOff:
		return PlanReviewOff
	default: // auto / "" / unknown
		n := 0
		for _, ok := range fams {
			if ok {
				n++
			}
		}
		if n >= 2 {
			return PlanReviewOn
		}
		return PlanReviewOff
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
func InjectIfDeclared(wf *ir.Workflow, inputs map[string]any, rep detect.Report, flagOverride string) (mode, monoFamily string, injected bool) {
	return InjectIfDeclaredFamilies(wf, inputs, FamiliesFromReport(rep), flagOverride)
}

// InjectIfDeclaredFamilies is InjectIfDeclared on an already-assembled
// family set (the cloud publisher derives one from the run's sealed
// credential bundle, where no host detection report applies).
func InjectIfDeclaredFamilies(wf *ir.Workflow, inputs map[string]any, fams FamilySet, flagOverride string) (mode, monoFamily string, injected bool) {
	if !declaresVar(wf, VarReviewMode) {
		return "", "", false
	}
	override := strings.TrimSpace(flagOverride)
	if override == "" {
		override = inputOverride(inputs, VarReviewMode)
	}
	mode, monoFamily = ResolveFamilies(fams, override)
	if inputs != nil {
		inputs[VarReviewMode] = mode
		inputs[VarMonoFamily] = monoFamily
	}
	return mode, monoFamily, true
}

// InjectPlanReviewIfDeclared resolves the cross-model plan-review switch and
// writes plan_review into inputs, but ONLY when the workflow opts in by
// declaring a plan_review var. Bots without a plan phase are left untouched.
//
// There is no dedicated CLI/API field: an operator override travels as an
// ordinary --var plan_review=on|off already present in inputs; a bot's own
// default of "auto" triggers credential-based resolution.
//
// Returns the resolved mode and whether injection happened (for logging).
func InjectPlanReviewIfDeclared(wf *ir.Workflow, inputs map[string]any, fams FamilySet) (mode string, injected bool) {
	if !declaresVar(wf, VarPlanReview) {
		return "", false
	}
	mode = ResolvePlanReview(fams, inputOverride(inputs, VarPlanReview))
	if inputs != nil {
		inputs[VarPlanReview] = mode
	}
	return mode, true
}

// InjectLLMFamiliesIfDeclared writes the sorted comma-joined available
// family list into inputs, ONLY when the workflow opts in by declaring an
// llm_families var. Unlike the role vars above there is nothing to resolve:
// an operator has no reason to override a fact, so any pre-set value is
// overwritten with the truth.
func InjectLLMFamiliesIfDeclared(wf *ir.Workflow, inputs map[string]any, fams FamilySet) (list string, injected bool) {
	if !declaresVar(wf, VarLLMFamilies) {
		return "", false
	}
	names := make([]string, 0, len(fams))
	for f, ok := range fams {
		if ok {
			names = append(names, f)
		}
	}
	sort.Strings(names)
	list = strings.Join(names, ",")
	if inputs != nil {
		inputs[VarLLMFamilies] = list
	}
	return list, true
}

// Injection summarises what InjectAll applied to a run's inputs. Zero-value
// fields mean the workflow did not declare the matching var.
type Injection struct {
	ReviewMode          string
	MonoFamily          string
	ReviewModeInjected  bool
	PlanReview          string
	PlanReviewInjected  bool
	LLMFamilies         string
	LLMFamiliesInjected bool
}

// InjectAll applies every opt-in injection this package owns (review_mode +
// mono_family, plan_review, llm_families) from one family set, so a launch
// surface calls one function and logs one summary. reviewModeOverride is
// the surface-level review-mode override (CLI --review-mode / API field);
// the other vars have no dedicated field and override via inputs only.
func InjectAll(wf *ir.Workflow, inputs map[string]any, fams FamilySet, reviewModeOverride string) Injection {
	var inj Injection
	inj.ReviewMode, inj.MonoFamily, inj.ReviewModeInjected = InjectIfDeclaredFamilies(wf, inputs, fams, reviewModeOverride)
	inj.PlanReview, inj.PlanReviewInjected = InjectPlanReviewIfDeclared(wf, inputs, fams)
	inj.LLMFamilies, inj.LLMFamiliesInjected = InjectLLMFamiliesIfDeclared(wf, inputs, fams)
	return inj
}

// Summary renders the applied injections for a launch log line, or "" when
// the workflow declared none of the vars.
func (i Injection) Summary() string {
	parts := make([]string, 0, 3)
	if i.ReviewModeInjected {
		s := "review topology: " + i.ReviewMode
		if i.MonoFamily != "" {
			s += " (family " + i.MonoFamily + ")"
		}
		parts = append(parts, s)
	}
	if i.PlanReviewInjected {
		parts = append(parts, "plan review: "+i.PlanReview)
	}
	if i.LLMFamiliesInjected {
		list := i.LLMFamilies
		if list == "" {
			list = "(none)"
		}
		parts = append(parts, "llm families: "+list)
	}
	return strings.Join(parts, " · ")
}

// inputOverride reads a string override already present in inputs (from
// --var / a preset), or "" when absent or not a string.
func inputOverride(inputs map[string]any, name string) string {
	if inputs == nil {
		return ""
	}
	if v, ok := inputs[name]; ok {
		if s, ok := v.(string); ok {
			return strings.TrimSpace(s)
		}
	}
	return ""
}
