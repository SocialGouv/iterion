// Package quality is the subjective quality + value-for-money assessment
// layer for iterion's live e2e tests. It runs a cross-family LLM judge
// panel over the REAL work product a bot/feature run produced (a git diff,
// the board issues it created, the docs it edited, the findings it
// surfaced…) together with the run's price metrics (cost, tokens,
// duration), and emits a structured Snapshot. Snapshots are persisted as
// an append-only per-target history (see store.go) so successive runs can
// be compared — primarily against the last — to attest improvement or
// regression of a bot's quality/value over time.
//
// Goodhart resistance is the whole point of the design:
//   - The assessed bot never sees this rubric or these judges; the
//     assessment is external + post-hoc, so the bot cannot optimise to it
//     and it is NEVER wired back into any bot loop.
//   - Judges grade the real artifact (the diff/work), never the bot's
//     self-reported claims.
//   - The panel is cross-family (two model families) so no single-model
//     bias dominates; a judge sharing the bot's family is flagged.
//   - Scoring is multi-dimensional + narrative, not one gameable number,
//     and the headline comparison is RELATIVE to the previous snapshot
//     (more reliable than absolute scoring across non-deterministic runs).
//
// The deterministic half (snapshot store, compare, regression gate) lives
// in store.go and is unit-tested without any LLM call. The live half (the
// judge panel) is exercised only from the //go:build live e2e tests.
package quality

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	"github.com/SocialGouv/iterion/pkg/backend/model"
)

// SchemaVersion is bumped when the Snapshot JSON shape changes
// incompatibly, so a reader can detect and skip stale history files.
const SchemaVersion = 1

// Dimension is one axis of the stable quality rubric. The set is fixed so
// snapshots stay comparable across runs; a multi-dimensional rubric (vs a
// single score) is itself a Goodhart safeguard.
type Dimension string

const (
	DimEfficacy      Dimension = "efficacy"        // did it truly accomplish the task (grounded in the artifact)
	DimCompleteness  Dimension = "completeness"    // full scope, no façade/stub/partial
	DimOutputQuality Dimension = "output_quality"  // idiomatic, maintainable, correct, no over-engineering
	DimRestraint     Dimension = "restraint"       // minimal diff, no unrequested churn or scope creep
	DimReliability   Dimension = "reliability"     // clean convergence, no thrash, acceptable termination
	DimValueForMoney Dimension = "value_for_money" // quality achieved per $ + time spent
	DimOverall       Dimension = "overall"         // holistic, NOT a mechanical average
)

// Dimensions is the canonical ordered rubric.
var Dimensions = []Dimension{
	DimEfficacy, DimCompleteness, DimOutputQuality, DimRestraint,
	DimReliability, DimValueForMoney, DimOverall,
}

// Relative is a per-dimension judgment of the current run versus the
// previous snapshot.
type Relative string

const (
	RelBetter Relative = "better"
	RelSame   Relative = "same"
	RelWorse  Relative = "worse"
	RelNA     Relative = "n/a" // no comparable previous snapshot
)

// Metrics is the price side of value-for-money: what the run consumed.
type Metrics struct {
	CostUSD    float64 `json:"cost_usd"`
	Tokens     int     `json:"tokens"`
	DurationMS int64   `json:"duration_ms"`
	Iterations int     `json:"iterations"`
	ModelCalls int     `json:"model_calls"`
	Retries    int     `json:"retries"`
}

// Evidence is the real-artifact bundle handed to every judge. The caller
// (the e2e glue) gathers it from the run — the engine never inspects a
// workspace or a store itself, keeping it pure and provider-agnostic.
type Evidence struct {
	Kind          string  // "bot" | "feature"
	Name          string  // target name, e.g. "review-pr" or "permission"
	Persona       string  // optional persona, e.g. "Revi"
	PrimaryFamily string  // the assessed run's primary model family ("anthropic", "openai", …)
	Task          string  // scenario / vars summary — what the run was asked to do
	WorkProduct   string  // the REAL artifact: git diff, board issues JSON, doc diff, findings…
	Outcome       string  // run status + acceptable-error reason + which nodes finished
	Metrics       Metrics // price side
}

// JudgeVerdict is one judge's structured assessment.
type JudgeVerdict struct {
	Model             string                 `json:"model"`
	Family            string                 `json:"family"`
	SameFamilyAsBot   bool                   `json:"same_family_as_bot"`
	Scores            map[Dimension]float64  `json:"scores"`
	Narrative         string                 `json:"narrative"`
	RelativeVsPrev    map[Dimension]Relative `json:"relative_vs_prev,omitempty"`
	RelativeNarrative string                 `json:"relative_narrative,omitempty"`
	Confidence        float64                `json:"confidence"`
}

// Aggregate is the combined panel result.
type Aggregate struct {
	Verdicts     []JudgeVerdict        `json:"verdicts"`
	MeanScores   map[Dimension]float64 `json:"mean_scores"`
	Disagreement map[Dimension]float64 `json:"disagreement,omitempty"` // max-min spread per dimension
	Note         string                `json:"note,omitempty"`         // e.g. judges skipped, family overlap
}

// judgeRaw mirrors the forced-tool JSON the judge returns. The shape is
// deliberately FLAT (every score a top-level field, no nested objects, no
// min/max constraints) — this matches the schema shape iterion's own
// claude_code judges use (ir.SchemaToJSON) and which the claude_code CLI's
// structured-output extraction handles reliably; a nested `scores` object
// made the CLI fall back to wrapped prose (the first cross-family live run).
type judgeRaw struct {
	Efficacy          float64 `json:"efficacy"`
	Completeness      float64 `json:"completeness"`
	OutputQuality     float64 `json:"output_quality"`
	Restraint         float64 `json:"restraint"`
	Reliability       float64 `json:"reliability"`
	ValueForMoney     float64 `json:"value_for_money"`
	Overall           float64 `json:"overall"`
	Narrative         string  `json:"narrative"`
	RelativeOverall   string  `json:"relative_overall"`
	RelativeNarrative string  `json:"relative_narrative"`
	Confidence        float64 `json:"confidence"`
}

// DefaultJudgeModels returns the cross-family judge panel. Override with
// ITERION_LIVE_JUDGE_MODELS (comma-separated provider/model specs). The
// default deliberately spans two families (Anthropic + OpenAI) so the
// panel is cross-family out of the box.
func DefaultJudgeModels() []string {
	if v := strings.TrimSpace(os.Getenv("ITERION_LIVE_JUDGE_MODELS")); v != "" {
		var out []string
		for _, p := range strings.Split(v, ",") {
			if s := strings.TrimSpace(p); s != "" {
				out = append(out, s)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return []string{"openai/gpt-5.5", "anthropic/claude-sonnet-4-6"}
}

// family extracts the provider family from a model spec ("anthropic/x" →
// "anthropic"). A spec with no "/" is returned verbatim as its own family.
func family(spec string) string {
	if i := strings.Index(spec, "/"); i >= 0 {
		return spec[:i]
	}
	return spec
}

// JudgeInvoker runs ONE judge: given a model spec, the rubric system
// prompt, the rendered evidence user message, and the forced-tool JSON
// schema, it returns the judge's structured verdict object. Abstracting the
// call lets callers route different judges through different backends — e.g.
// an OpenAI judge via claw's direct-generation path and an Anthropic judge
// via the claude_code OAuth delegate (so a true cross-family panel works
// without an ANTHROPIC_API_KEY). See ClawInvoker for the default.
type JudgeInvoker func(ctx context.Context, modelSpec, system, userMsg string, schema json.RawMessage) (map[string]interface{}, error)

// ClawInvoker is the default judge invoker: it resolves the model spec to a
// claw client and forces the structured-output tool. Requires an API key
// for the model's provider (claw cannot use Claude Code OAuth).
func ClawInvoker(reg *model.Registry) JudgeInvoker {
	return func(ctx context.Context, spec, system, userMsg string, schema json.RawMessage) (map[string]interface{}, error) {
		client, err := reg.Resolve(spec)
		if err != nil {
			return nil, err
		}
		res, err := model.GenerateObjectDirect[map[string]interface{}](ctx, client, model.GenerationOptions{
			Model:          spec,
			System:         system,
			ExplicitSchema: schema,
			SchemaName:     "quality_assessment",
			MaxTokens:      4096,
			Messages:       []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: userMsg}}}},
		})
		if err != nil {
			return nil, err
		}
		return res.Object, nil
	}
}

// RunPanelWith runs every judge model over the evidence via invoke and
// aggregates their verdicts. A judge that errors is skipped with a note
// rather than failing the panel, so a single available family still yields
// an assessment. An error is returned only when NO judge produced a verdict.
func RunPanelWith(ctx context.Context, models []string, invoke JudgeInvoker, ev Evidence, prev *Snapshot) (Aggregate, error) {
	if len(models) == 0 {
		models = DefaultJudgeModels()
	}
	sys := rubricSystemPrompt()
	user := renderEvidence(ev, prev)
	schema := judgeSchemaJSON()

	var agg Aggregate
	var notes []string
	for _, spec := range models {
		obj, err := invoke(ctx, spec, sys, user, schema)
		if err != nil {
			notes = append(notes, fmt.Sprintf("judge %s errored: %v", spec, err))
			continue
		}
		raw, err := mapToJudgeRaw(obj)
		if err != nil {
			notes = append(notes, fmt.Sprintf("judge %s parse: %v", spec, err))
			continue
		}
		v := toVerdict(spec, ev.PrimaryFamily, raw)
		if strings.TrimSpace(v.Narrative) == "" {
			// A real verdict always carries a narrative (a required field).
			// An empty narrative means the backend returned non-conforming
			// output (e.g. claude_code wrapping prose into {"text":…} when
			// its structured-output extraction failed) → drop it + note it,
			// rather than pollute the panel with an all-zero verdict.
			notes = append(notes, fmt.Sprintf("judge %s returned no usable verdict (dropped)", spec))
			continue
		}
		agg.Verdicts = append(agg.Verdicts, v)
	}

	if len(agg.Verdicts) == 0 {
		return Aggregate{Note: strings.Join(notes, "; ")}, fmt.Errorf("quality: no judge produced a verdict (%s)", strings.Join(notes, "; "))
	}
	agg.MeanScores, agg.Disagreement = aggregateScores(agg.Verdicts)
	if len(notes) > 0 {
		agg.Note = strings.TrimSpace(strings.Join(notes, "; "))
	}
	return agg, nil
}

// mapToJudgeRaw converts a decoded structured-output object into the typed
// judgeRaw via a JSON round-trip (the invoker returns a generic map so it
// can come from any backend).
func mapToJudgeRaw(obj map[string]interface{}) (judgeRaw, error) {
	b, err := json.Marshal(obj)
	if err != nil {
		return judgeRaw{}, err
	}
	var raw judgeRaw
	if err := json.Unmarshal(b, &raw); err != nil {
		return judgeRaw{}, err
	}
	return raw, nil
}

// toVerdict normalises a raw judge payload into a JudgeVerdict, clamping
// scores to [0,1] and mapping the relative strings.
func toVerdict(spec, botFamily string, raw judgeRaw) JudgeVerdict {
	v := JudgeVerdict{
		Model:           spec,
		Family:          family(spec),
		SameFamilyAsBot: botFamily != "" && family(spec) == botFamily,
		Scores: map[Dimension]float64{
			DimEfficacy:      clamp01(raw.Efficacy),
			DimCompleteness:  clamp01(raw.Completeness),
			DimOutputQuality: clamp01(raw.OutputQuality),
			DimRestraint:     clamp01(raw.Restraint),
			DimReliability:   clamp01(raw.Reliability),
			DimValueForMoney: clamp01(raw.ValueForMoney),
			DimOverall:       clamp01(raw.Overall),
		},
		Narrative:         raw.Narrative,
		RelativeNarrative: raw.RelativeNarrative,
		Confidence:        clamp01(raw.Confidence),
	}
	if r := normRelative(raw.RelativeOverall); r != RelNA {
		v.RelativeVsPrev = map[Dimension]Relative{DimOverall: r}
	}
	return v
}

// aggregateScores returns the per-dimension mean and the max-min spread
// (disagreement) across the panel.
func aggregateScores(verdicts []JudgeVerdict) (mean, spread map[Dimension]float64) {
	mean = make(map[Dimension]float64, len(Dimensions))
	spread = make(map[Dimension]float64, len(Dimensions))
	for _, d := range Dimensions {
		var sum float64
		var n int
		lo, hi := math.Inf(1), math.Inf(-1)
		for _, v := range verdicts {
			s, ok := v.Scores[d]
			if !ok {
				continue
			}
			sum += s
			n++
			lo, hi = math.Min(lo, s), math.Max(hi, s)
		}
		if n > 0 {
			mean[d] = sum / float64(n)
			spread[d] = hi - lo
		}
	}
	return mean, spread
}

func normRelative(s string) Relative {
	switch Relative(strings.ToLower(strings.TrimSpace(s))) {
	case RelBetter:
		return RelBetter
	case RelWorse:
		return RelWorse
	case RelSame:
		return RelSame
	default:
		return RelNA
	}
}

func clamp01(f float64) float64 {
	if math.IsNaN(f) {
		return 0
	}
	if f < 0 {
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}

// rubricSystemPrompt is the stable judge rubric + anti-Goodhart framing.
func rubricSystemPrompt() string {
	var b strings.Builder
	b.WriteString(`You are an exacting, impartial software-quality auditor grading the REAL work product of an autonomous coding agent ("the bot"). You are external to the bot: it cannot see you, your rubric, or your scores, and your judgment is never fed back into its loop. Grade ONLY the concrete artifact and metrics you are given — never the bot's self-description or stated intentions.

Score each dimension on a 0.0–1.0 scale (0 = absent/broken, 0.5 = mediocre/partial, 1.0 = excellent). Be calibrated and stingy: reserve >0.9 for genuinely excellent work. Dimensions:

- efficacy: Did it actually accomplish the stated task, as proven by the artifact (diff compiles/makes sense, tests added actually assert, findings are real)? Reward real results, not effort.
- completeness: Is the scope fully addressed with no façade, stub, TODO-as-done, or "looks-done-but-isn't"? Penalise scaffolding presented as a finished feature.
- output_quality: Is the produced code/text idiomatic, correct, maintainable, and free of over-engineering?
- restraint: Is the change minimal and on-target — no unrequested churn, dead code, gratuitous rewrites, or scope creep?
- reliability: Did the run converge cleanly (stable approval / sensible termination) without thrash, oscillation, or an unexplained abort?
- value_for_money: Given the cost (USD), tokens, duration and iteration count, was the quality achieved a good deal? A small, correct change for a few cents is excellent value; a large spend for a façade is poor value.
- overall: Your holistic verdict — NOT a mechanical average. Weigh efficacy and completeness most; a façade caps overall low regardless of other scores.

Anti-gaming rules (critical):
- Verbosity, padding, large diffs, and confident prose are NOT quality. A façade (plausible but non-functional, or claiming success it didn't achieve) must score low on efficacy/completeness/overall.
- If the artifact contradicts the claimed outcome, trust the artifact.
- Judge substance, not style.

If a PREVIOUS assessment is provided, also set relative_overall (better/same/worse) and a relative_narrative. When comparing: reward GENUINE improvement in the real result; treat stylistic-only or cosmetic differences as "same"; do not penalise a run merely for being different. Be conservative — only say "better"/"worse" when the evidence clearly supports it. With no previous assessment, set relative_overall to "n/a".

Provide your assessment as the required structured output — EVERY field must be present: the seven scores (efficacy, completeness, output_quality, restraint, reliability, value_for_money, overall) each as a top-level number 0.0–1.0, a narrative, and confidence. narrative: 3-8 sentences grounding every score in specific evidence. confidence: 0.0–1.0 in your own assessment.`)
	return b.String()
}

// renderEvidence builds the user message: the task, the real artifact, the
// outcome, the price metrics, and (optionally) the previous snapshot.
func renderEvidence(ev Evidence, prev *Snapshot) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Target\nkind: %s\nname: %s\n", ev.Kind, ev.Name)
	if ev.Persona != "" {
		fmt.Fprintf(&b, "persona: %s\n", ev.Persona)
	}
	if ev.Task != "" {
		fmt.Fprintf(&b, "\n# Task the run was asked to perform\n%s\n", ev.Task)
	}
	fmt.Fprintf(&b, "\n# Run outcome\n%s\n", strings.TrimSpace(ev.Outcome))
	m := ev.Metrics
	fmt.Fprintf(&b, "\n# Price metrics (for value_for_money)\ncost_usd: %.4f\ntokens: %d\nduration_ms: %d\niterations: %d\nmodel_calls: %d\nretries: %d\n",
		m.CostUSD, m.Tokens, m.DurationMS, m.Iterations, m.ModelCalls, m.Retries)
	fmt.Fprintf(&b, "\n# Real work product (the artifact to grade)\n%s\n", strings.TrimSpace(ev.WorkProduct))

	if prev != nil {
		fmt.Fprintf(&b, "\n# Previous assessment (for relative comparison)\nprev_run_id: %s\nprev_at: %s\n", prev.RunID, prev.At.Format("2006-01-02T15:04:05Z"))
		if prev.Aggregate.MeanScores != nil {
			fmt.Fprintf(&b, "prev_mean_scores:\n")
			for _, d := range Dimensions {
				if s, ok := prev.Aggregate.MeanScores[d]; ok {
					fmt.Fprintf(&b, "  %s: %.2f\n", d, s)
				}
			}
		}
		if pm := prev.Metrics; pm.CostUSD > 0 || pm.Tokens > 0 {
			fmt.Fprintf(&b, "prev_cost_usd: %.4f\nprev_tokens: %d\nprev_duration_ms: %d\n", pm.CostUSD, pm.Tokens, pm.DurationMS)
		}
		if n := prevNarrative(prev); n != "" {
			fmt.Fprintf(&b, "prev_narrative: %s\n", n)
		}
	} else {
		b.WriteString("\n# Previous assessment\n(none — this is the first snapshot for this target; set every relative_vs_prev to \"n/a\")\n")
	}
	return b.String()
}

// prevNarrative returns the first available judge narrative from a prior
// snapshot, for the comparison context.
func prevNarrative(prev *Snapshot) string {
	for _, v := range prev.Aggregate.Verdicts {
		if strings.TrimSpace(v.Narrative) != "" {
			return v.Narrative
		}
	}
	return ""
}

// judgeSchemaJSON is the forced-tool JSON Schema for a judge verdict. It is
// FLAT (every score a top-level number field) on purpose — see judgeRaw —
// so both claw and the claude_code CLI's structured-output extraction
// populate it reliably. Scores are clamped to [0,1] in toVerdict rather
// than via min/max constraints (which the CLI handled less reliably).
func judgeSchemaJSON() json.RawMessage {
	props := map[string]any{}
	required := make([]string, 0, len(Dimensions)+2)
	for _, d := range Dimensions {
		props[string(d)] = map[string]any{"type": "number"}
		required = append(required, string(d))
	}
	props["narrative"] = map[string]any{"type": "string"}
	props["relative_overall"] = map[string]any{"type": "string", "enum": []string{"better", "same", "worse", "n/a"}}
	props["relative_narrative"] = map[string]any{"type": "string"}
	props["confidence"] = map[string]any{"type": "number"}
	required = append(required, "narrative", "confidence")
	schema := map[string]any{
		"type":                 "object",
		"properties":           props,
		"required":             required,
		"additionalProperties": false,
	}
	b, _ := json.Marshal(schema)
	return b
}
