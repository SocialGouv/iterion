// Package reviewtopology resolves the review topology for review-loop bots
// (review-pr, evolve) and injects the resolved review_mode var into a run's
// inputs when the target workflow opts in by declaring one.
//
// The vocabulary names ROLES, not vendors (ADR-169): a run reviews with
// `single` reviewer, or with `paired` — two reviewers from different model
// families, trading frugality for cross-model confirmation. Which backend
// and model serves each role is a routing question, answered by the
// catalogue's literals and the llmroute cells over them, never by this
// package: the bot cannot probe host credentials, but it no longer has to —
// it names `reviewer` and `reviewer_alternate`, and those names mean the
// same thing on every deployment.
package reviewtopology

import (
	"os"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// Var names injected into a run's inputs. A bot opts into topology
// resolution by declaring VarReviewMode in its vars: block, and into
// plan-review resolution by declaring VarPlanReview.
const (
	VarReviewMode = "review_mode"
	VarPlanReview = "plan_review"
	// VarLLMFamilies is the generic, role-free fact beside the two role vars:
	// a bot declaring it receives the sorted comma-joined list of review
	// families backed by a credential (e.g. "claude,gpt"). It exists so a
	// bot can build its OWN policy in a compute node without the engine
	// learning a new role var per pattern — the engine publishes the fact a
	// bot cannot probe (host/run credentials), nothing more.
	VarLLMFamilies = "llm_families"
)

// Topology modes. ModeAuto is the pre-resolution default a bot ships with;
// Resolve never returns it (it collapses auto → single|paired).
const (
	ModeAuto   = "auto"
	ModeSingle = "single"
	ModePaired = "paired"
)

// retiredModes maps the pre-ADR-169 spellings onto the role vocabulary.
// They are refused nowhere because a launch surface that has not been
// re-pinned yet still sends them — the catalogue's integrations carry
// them as launch vars, and a run must not die on a rename while the
// operator's own mapping migrates (#2699). Resolve writes the concrete
// role back into the inputs, so the bot's declared enum only ever sees
// single|paired.
var retiredModes = map[string]string{
	"mono": ModeSingle,
	"dual": ModePaired,
}

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

// FamilySet is the set of distinct review families backed by an available
// credential — the input the plan-review and llm_families resolutions read.
// Built from a host detection report (InventoryFromReport) or from a
// caller's own credential names (InventoryFromCredentialNames — the cloud
// publisher derives one from the sealed per-run bundle).
type FamilySet map[string]bool

// Resolve computes the effective review topology from an operator override.
//
// override is one of "single", "paired", or "auto" (also "" / any unknown →
// auto):
//   - "paired"    → two reviewers, one per model family. A deliberate
//     spend, opt-in only.
//   - "single"    → one reviewer. The bot's role name says WHICH one
//     (`reviewer`); the deployment's routing cells say where it runs.
//   - "auto"/""   → single. Frugality is the default because auto is what
//     every unconfigured caller gets: cross-family confirmation is a
//     deliberate spend, never something a host opts into merely by having
//     two providers configured.
//
// The returned mode is always concrete ("single" or "paired"), never
// "auto". The pre-ADR-169 spellings are still accepted (see retiredModes).
//
// It reads no credential, and that is the point: with the family no longer a
// var, a host with one credential and a host with ten resolve the same way,
// and a run whose role no credential serves fails at its first node with
// that node's own error — the ordinary fate of every bot's literal route,
// not a launch-time opinion this package would have to keep true.
func Resolve(override string) string {
	mode := strings.ToLower(strings.TrimSpace(override))
	if retired, ok := retiredModes[mode]; ok {
		return retired
	}
	switch mode {
	case ModePaired:
		return ModePaired
	default: // auto / "" / unknown / single
		return ModeSingle
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
//     credential. With today's two participating families (claude, gpt)
//     this is equivalent to "claude AND gpt" — which matches the shipped
//     bots, whose peer node is PINNED to the gpt route
//     (claw + openai/*). Adding a third family must revisit this
//     resolution together with the bots' peer pinning, or a
//     {claude, <new>} host would resolve "on" and the peer would chase
//     an absent OpenAI credential. Anything less than two families means
//     the pair review cannot be a cross-model check, so the bot keeps
//     its plan-in-stride shape at zero extra cost.
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

// InjectReviewModeIfDeclared resolves the review topology and writes
// review_mode into inputs, but ONLY when the workflow opts in by declaring
// a review_mode var. Bots that don't use the topology are left untouched.
//
// The override precedence is: an explicit flagOverride (e.g. the CLI
// --review-mode flag) wins; otherwise a concrete value already in
// inputs[review_mode] (from --var / a preset) is used; otherwise "auto".
// Auto and any unrecognised value resolve to single.
//
// The concrete mode is written back, so a launch var still carrying a
// retired spelling (mono|dual) reaches the bot's declared enum as
// single|paired and never trips it. An injection that wrote nothing (a nil
// inputs map) answers injected=false.
func InjectReviewModeIfDeclared(wf *ir.Workflow, inputs map[string]any, flagOverride string) (mode string, injected bool) {
	if !declaresVar(wf, VarReviewMode) {
		return "", false
	}
	override := strings.TrimSpace(flagOverride)
	if override == "" {
		override = inputOverride(inputs, VarReviewMode)
	}
	mode = Resolve(override)
	if inputs == nil {
		// Nothing to write into: the mode is resolved, but no injection
		// happened — reporting one would log a write that does not exist.
		return mode, false
	}
	inputs[VarReviewMode] = mode
	return mode, true
}

// PlanReviewEnv is the machine/deployment-wide default for the
// plan-review switch, read when a run carries no explicit override. It
// exists because auto-resolution can flip the phase ON for launches that
// have no per-run surface at all (webhook / dispatcher / cron lanes on a
// cloud instance whose platform tier credentials both families) — an
// operator needs a brake that is not a per-run --var. Precedence:
// --var plan_review → ITERION_PLAN_REVIEW → auto.
const PlanReviewEnv = "ITERION_PLAN_REVIEW"

// InjectPlanReviewIfDeclared resolves the cross-model plan-review switch and
// writes plan_review into inputs, but ONLY when the workflow opts in by
// declaring a plan_review var. Bots without a plan phase are left untouched.
//
// There is no dedicated CLI/API field: an operator override travels as an
// ordinary --var plan_review=on|off already present in inputs; the
// ITERION_PLAN_REVIEW env is the deployment-wide default under it; a bot's
// own default of "auto" triggers credential-based resolution.
//
// Returns the resolved mode and whether injection happened (for logging).
func InjectPlanReviewIfDeclared(wf *ir.Workflow, inputs map[string]any, inv FamilyInventory) (mode string, injected bool) {
	if !declaresVar(wf, VarPlanReview) {
		return "", false
	}
	fams := inv.Families()
	override := inputOverride(inputs, VarPlanReview)
	if override == "" || strings.EqualFold(override, PlanReviewAuto) {
		if env := strings.TrimSpace(os.Getenv(PlanReviewEnv)); env != "" {
			// A brake must fail SAFE: any set-but-unrecognised value reads
			// as OFF (the frugal direction), never as a silent fall-through
			// to auto → on. "auto" is this dial's own neutral member and
			// stays neutral (credential-based resolution).
			switch strings.ToLower(env) {
			case "on", "true", "1", "yes":
				override = PlanReviewOn
			case "auto":
				// neutral: keep the credential-based resolution
			default:
				override = PlanReviewOff
			}
		}
	}
	mode = ResolvePlanReview(fams, override)
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
func InjectLLMFamiliesIfDeclared(wf *ir.Workflow, inputs map[string]any, inv FamilyInventory) (list string, injected bool) {
	if !declaresVar(wf, VarLLMFamilies) {
		return "", false
	}
	fams := inv.Families()
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
	ReviewModeInjected  bool
	PlanReview          string
	PlanReviewInjected  bool
	LLMFamilies         string
	LLMFamiliesInjected bool
}

// InjectAll applies every opt-in injection this package owns (review_mode,
// plan_review, llm_families) from one family inventory, so a launch surface
// calls one function and logs one summary. reviewModeOverride is the
// surface-level review-mode override (CLI --review-mode / API field); the
// other vars have no dedicated field and override via inputs only.
//
// It returns no error: with the family no longer a var, nothing here can
// refuse a launch.
func InjectAll(wf *ir.Workflow, inputs map[string]any, inv FamilyInventory, reviewModeOverride string) Injection {
	inj := InjectDerivedIfDeclared(wf, inputs, inv)
	inj.ReviewMode, inj.ReviewModeInjected = InjectReviewModeIfDeclared(wf, inputs, reviewModeOverride)
	return inj
}

// InjectDerivedIfDeclared applies the two injections that READ the credential
// set — plan_review and llm_families. They are the ones a surface owes the
// run only when it can see the credentials; the review mode is not one of
// them (it reads no credential since ADR-169), which is why a lane that
// resolved no family still resolves the mode through
// InjectReviewModeIfDeclared.
func InjectDerivedIfDeclared(wf *ir.Workflow, inputs map[string]any, inv FamilyInventory) Injection {
	var inj Injection
	inj.PlanReview, inj.PlanReviewInjected = InjectPlanReviewIfDeclared(wf, inputs, inv)
	inj.LLMFamilies, inj.LLMFamiliesInjected = InjectLLMFamiliesIfDeclared(wf, inputs, inv)
	return inj
}

// Summary renders the applied injections for a launch log line, or "" when
// the workflow declared none of the vars.
func (i Injection) Summary() string {
	parts := make([]string, 0, 3)
	if i.ReviewModeInjected {
		parts = append(parts, "review topology: "+i.ReviewMode)
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
