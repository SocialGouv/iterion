package sessionboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/detect"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// decisionSchema is the structured-output contract handed to
// GenerateObjectDirect's synthetic tool. It constrains `kind` to the known
// widget registry so the model can't invent a kind the studio can't
// render, and documents the per-kind `props` shape inline.
const decisionSchema = `{
  "type": "object",
  "properties": {
    "upsert": {
      "type": "array",
      "description": "Widgets to create or replace (matched by id). Only add widgets that show something the technical run view (events/logs/graph/cost/task list) does NOT already show.",
      "items": {
        "type": "object",
        "required": ["id", "kind"],
        "properties": {
          "id":    {"type": "string", "description": "Stable id; reuse the same id to update a widget instead of duplicating it."},
          "kind":  {"type": "string", "enum": ["note", "metric", "checklist", "progress", "bar_chart"]},
          "title": {"type": "string"},
          "props": {"type": "object", "description": "Kind-specific. note: {text}. metric: {value, hint}. checklist: {items:[{text, done}]}. progress: {value, max}. bar_chart: {data:[{label, value}]}."}
        }
      }
    },
    "remove": {"type": "array", "items": {"type": "string"}, "description": "Widget ids to remove (no longer relevant)."},
    "reason": {"type": "string", "description": "Short rationale (logs only)."},
    "done":   {"type": "boolean", "description": "True when the board is in good shape and you have nothing to add right now."}
  }
}`

// EvalInput is everything the curation bot sees for one wake.
type EvalInput struct {
	BotID        string   // the running bot's id, for framing (may be empty)
	ActiveNode   string   // node currently executing
	WakeReason   string   // "turn_boundary" or a monitor description
	RecentEvents []string // rendered recent events (oldest first)
	Current      Spec     // the board as it stands now, so the bot diffs
}

// Evaluator decides how to evolve the board for one wake. The interface
// lets the coordinator be unit-tested with a stub.
type Evaluator interface {
	Evaluate(ctx context.Context, in EvalInput) (*BoardDecision, EvalUsage, error)
}

// EvalUsage carries the token cost of one evaluation.
type EvalUsage struct {
	InputTokens  int
	OutputTokens int
}

// ErrNoModel is returned when no model is pinned and none is detectable.
var ErrNoModel = errors.New("sessionboard: no model configured and no provider credential detected")

// LLMEvaluator is the production Evaluator: a direct claw structured call,
// mirroring pkg/supervise's LLMEvaluator.
type LLMEvaluator struct {
	registry  *model.Registry
	client    api.APIClient
	modelSpec string
	pinned    string
}

// NewLLMEvaluator constructs an evaluator. modelSpec pins the model; pass
// "" to resolve from ITERION_DEFAULT_SESSIONBOARD_MODEL or auto-detection.
// The client resolves lazily on first Evaluate so construction never
// blocks.
func NewLLMEvaluator(modelSpec string) *LLMEvaluator {
	return &LLMEvaluator{registry: model.NewRegistry(), pinned: modelSpec}
}

func resolveModel(pinned string) (string, error) {
	if pinned != "" {
		return pinned, nil
	}
	if env := ir.LookupEnv("ITERION_DEFAULT_SESSIONBOARD_MODEL"); env != "" {
		return env, nil
	}
	report := detect.Detect(context.Background())
	if spec := detect.SuggestedModel(detect.BackendClaw, report.Providers); spec != "" {
		return spec, nil
	}
	return "", ErrNoModel
}

// Evaluate implements Evaluator.
func (e *LLMEvaluator) Evaluate(ctx context.Context, in EvalInput) (*BoardDecision, EvalUsage, error) {
	if e.client == nil {
		spec, err := resolveModel(e.pinned)
		if err != nil {
			return nil, EvalUsage{}, err
		}
		client, err := e.registry.Resolve(spec)
		if err != nil {
			return nil, EvalUsage{}, fmt.Errorf("sessionboard: resolve model %q: %w", spec, err)
		}
		e.client = client
		e.modelSpec = spec
	}

	opts := model.GenerationOptions{
		Model:          model.ProviderlessModelID(e.modelSpec),
		System:         buildSystemPrompt(),
		Messages:       []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: buildUserPrompt(in)}}}},
		ExplicitSchema: json.RawMessage(decisionSchema),
		SchemaName:     "session_board_decision",
		MaxTokens:      2_000,
	}
	res, err := model.GenerateObjectDirect[BoardDecision](ctx, e.client, opts)
	if err != nil {
		return nil, EvalUsage{}, fmt.Errorf("sessionboard: evaluation call: %w", err)
	}
	usage := EvalUsage{InputTokens: res.TotalUsage.InputTokens, OutputTokens: res.TotalUsage.OutputTokens}
	d := res.Object
	return &d, usage, nil
}

// buildSystemPrompt frames the curator's role: complement, never
// duplicate, the run view, and converge (emit diffs, stay stable).
func buildSystemPrompt() string {
	return "You curate a small live DASHBOARD for one AI coding session, shown beside the agent's task list. " +
		"Your job is to add a few high-signal widgets that capture what THIS session is about — its milestones, blockers, " +
		"a one-line narrative of where it is, or a simple chart of something meaningful — so a human glancing at the board " +
		"instantly understands the session.\n\n" +
		"Hard rules:\n" +
		"- Do NOT duplicate what the run view already shows: the event log, raw logs, the execution graph, cost/token meters, " +
		"artifacts, or the literal task-list checklist. Add only NEW, session-specific context.\n" +
		"- Emit DIFFS, not redraws: reuse a widget's id to update it; only change a widget when the situation actually changed; " +
		"remove a widget when it stops being relevant. A stable board is the goal — do not churn it every turn.\n" +
		"- Keep it small (a handful of widgets). When the board already reflects the session well, set done=true and add nothing.\n\n" +
		"Widget kinds: note (a short status sentence), metric (one labelled number), checklist (milestones with done flags), " +
		"progress (a value/max bar), bar_chart (labelled values). Choose the simplest kind that conveys the point."
}

// buildUserPrompt renders the current situation for one evaluation.
func buildUserPrompt(in EvalInput) string {
	var b strings.Builder
	if in.BotID != "" {
		fmt.Fprintf(&b, "Bot: %s\n", in.BotID)
	}
	fmt.Fprintf(&b, "Wake reason: %s\n", in.WakeReason)
	if in.ActiveNode != "" {
		fmt.Fprintf(&b, "Active node: %s\n", in.ActiveNode)
	}
	if data, err := json.Marshal(in.Current.Widgets); err == nil {
		fmt.Fprintf(&b, "Current board widgets: %s\n", data)
	}
	b.WriteString("\nRecent activity (oldest first):\n")
	if len(in.RecentEvents) == 0 {
		b.WriteString("(no events yet)\n")
	}
	for _, ev := range in.RecentEvents {
		b.WriteString("  ")
		b.WriteString(ev)
		b.WriteString("\n")
	}
	b.WriteString("\nDecide which widgets to upsert/remove to keep the board an accurate, non-redundant summary of this session.")
	return b.String()
}
