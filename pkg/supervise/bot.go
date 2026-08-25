package supervise

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/detect"
	"github.com/SocialGouv/iterion/pkg/backend/model"
)

// Decision is the supervisor bot's structured verdict for one wake. The
// bot decides whether to steer the supervised node now (Intervene +
// Message), which event patterns to keep watching (Watch), and whether
// its job is finished (Done — stop evaluating this node).
type Decision struct {
	// Intervene is true when the supervisor wants to enqueue Message
	// into the supervised node right now (delivered at its next turn).
	Intervene bool `json:"intervene"`
	// Message is the steering text — required when Intervene is true.
	Message string `json:"message,omitempty"`
	// Reason is a short rationale, surfaced in logs only.
	Reason string `json:"reason,omitempty"`
	// Watch lists event patterns to (re)register so the coordinator
	// wakes the supervisor immediately when they fire — the
	// event-driven "launch monitors on ongoing activity" surface.
	Watch []Monitor `json:"watch,omitempty"`
	// Done signals the supervisor is satisfied and should stop
	// evaluating (until a registered monitor fires again).
	Done bool `json:"done,omitempty"`
}

// logSummary renders the decision as one log fragment; nil-safe (a nil
// decision is a real, silent case) so log call sites need no guards.
func (d *Decision) logSummary() string {
	if d == nil {
		return "no decision"
	}
	s := fmt.Sprintf("intervene=%v watch+%d done=%v", d.Intervene, len(d.Watch), d.Done)
	if d.Reason != "" {
		s += " reason=" + d.Reason
	}
	return s
}

// decisionSchema is the structured-output contract handed to
// GenerateObjectDirect's synthetic tool.
const decisionSchema = `{
  "type": "object",
  "required": ["intervene"],
  "properties": {
    "intervene": {"type": "boolean", "description": "True to enqueue a steering message into the supervised node now."},
    "message":   {"type": "string", "description": "The steering message the supervised agent will read on its next turn. Required when intervene is true. Be specific and actionable; this is read like a human operator typing mid-session."},
    "reason":    {"type": "string", "description": "Short rationale for the decision (logs only)."},
    "watch": {
      "type": "array",
      "description": "Event patterns to ADD to your watch set (existing ones persist and cannot be removed; re-listing them changes nothing). When a pattern you registered fires you are woken immediately. Register the few precise signals you care about.",
      "items": {
        "type": "object",
        "properties": {
          "event_type":    {"type": "string", "description": "Event type to match, e.g. tool_error, assistant_text, budget_warning. (The watched node's own node_finished is not observable — the supervisor disarms on it.)"},
          "node_id":       {"type": "string"},
          "tool_name":     {"type": "string", "description": "Match a tool by name, e.g. Bash, Edit."},
          "text_contains": {"type": "string", "description": "Case-insensitive substring matched against the rendered event."},
          "cost_gt":       {"type": "number", "description": "Fire when a budget_warning reports used > this value."}
        }
      }
    },
    "done": {"type": "boolean", "description": "True to stop supervising (until a watched pattern fires again)."}
  }
}`

// EvalInput is everything the bot sees for one evaluation.
type EvalInput struct {
	Spec         Spec
	ActiveNode   string    // the node currently armed (being supervised)
	WakeReason   string    // "turn_boundary" or a monitor description
	RecentEvents []string  // rendered recent events (oldest first)
	Monitors     []Monitor // currently-registered monitors
	Last         *Decision // the previous decision, for monotonic context
}

// Evaluator decides what (if anything) the supervisor should do for one
// wake. The interface lets the coordinator be tested with a stub.
type Evaluator interface {
	Evaluate(ctx context.Context, in EvalInput) (*Decision, EvalUsage, error)
}

// EvalUsage carries the token cost of one evaluation so the coordinator
// can enforce a budget.
type EvalUsage struct {
	InputTokens  int
	OutputTokens int
}

// ErrNoSupervisorModel is returned when no model is pinned on the spec
// and no provider credential is auto-detectable.
var ErrNoSupervisorModel = errors.New("supervise: no model configured and no provider credential detected (set Spec.Model or sign in via claude/codex)")

// LLMEvaluator is the production Evaluator: a direct claw structured
// call (no second engine run), mirroring runview's conflict resolver.
type LLMEvaluator struct {
	registry *model.Registry
	// client is resolved lazily on first use and cached (the model spec
	// is constant for a coordinator's life).
	client    api.APIClient
	modelSpec string
}

// NewLLMEvaluator constructs an evaluator. The model client is resolved
// on the first Evaluate call so construction never blocks on detection.
func NewLLMEvaluator() *LLMEvaluator {
	return &LLMEvaluator{registry: model.NewRegistry()}
}

// resolveModel picks the model spec: the spec's pin wins, then the
// ITERION_DEFAULT_SUPERVISOR_MODEL env override, then the detector's
// suggested claw model. Returns ErrNoSupervisorModel when none resolve.
func resolveModel(specModel string) (string, error) {
	if specModel != "" {
		return specModel, nil
	}
	if env := os.Getenv("ITERION_DEFAULT_SUPERVISOR_MODEL"); env != "" {
		return env, nil
	}
	report := detect.Detect(context.Background())
	if spec := detect.SuggestedModel(detect.BackendClaw, report.Providers); spec != "" {
		return spec, nil
	}
	return "", ErrNoSupervisorModel
}

// Evaluate implements Evaluator.
func (e *LLMEvaluator) Evaluate(ctx context.Context, in EvalInput) (*Decision, EvalUsage, error) {
	if e.client == nil {
		spec, err := resolveModel(in.Spec.Model)
		if err != nil {
			return nil, EvalUsage{}, err
		}
		client, err := e.registry.Resolve(spec)
		if err != nil {
			return nil, EvalUsage{}, fmt.Errorf("supervise: resolve model %q: %w", spec, err)
		}
		e.client = client
		e.modelSpec = spec
	}

	opts := model.GenerationOptions{
		Model:          model.ProviderlessModelID(e.modelSpec),
		System:         buildSystemPrompt(in.Spec),
		Messages:       []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: buildUserPrompt(in)}}}},
		ExplicitSchema: json.RawMessage(decisionSchema),
		SchemaName:     "supervisor_decision",
		MaxTokens:      2_000,
	}
	res, err := model.GenerateObjectDirect[Decision](ctx, e.client, opts)
	if err != nil {
		return nil, EvalUsage{}, fmt.Errorf("supervise: evaluation call: %w", err)
	}
	usage := EvalUsage{
		InputTokens:  res.TotalUsage.InputTokens,
		OutputTokens: res.TotalUsage.OutputTokens,
	}
	d := res.Object
	return &d, usage, nil
}

// buildSystemPrompt frames the supervisor's role and grafts the
// operator's policy (Spec.System) on top.
func buildSystemPrompt(spec Spec) string {
	var b strings.Builder
	b.WriteString("You are an autonomous SUPERVISOR watching another AI agent work in a live coding session. ")
	b.WriteString("You act like an experienced human operator looking over the agent's shoulder: most of the time you stay silent and let it work, and you intervene ONLY when you see something worth steering ")
	b.WriteString("(a wrong direction, a repeated failure, a risky action, a missed requirement). ")
	b.WriteString("When you intervene, your message is delivered to the agent at its NEXT turn — be specific and actionable, like a human typing a quick correction. ")
	b.WriteString("Prefer registering `watch` patterns for the few signals you care about over re-reading every turn. Set `done` when there is nothing more to watch.\n\n")
	if strings.TrimSpace(spec.System) != "" {
		b.WriteString("## Supervision policy\n\n")
		b.WriteString(spec.System)
	}
	return b.String()
}

// buildUserPrompt renders the current situation for one evaluation.
func buildUserPrompt(in EvalInput) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Wake reason: %s\n", in.WakeReason)
	if in.ActiveNode != "" {
		fmt.Fprintf(&b, "Supervised node: %s\n", in.ActiveNode)
	}
	if len(in.Monitors) > 0 {
		if data, err := json.Marshal(in.Monitors); err == nil {
			fmt.Fprintf(&b, "Currently watching: %s\n", data)
		}
	}
	if in.Last != nil && (in.Last.Message != "" || in.Last.Reason != "") {
		fmt.Fprintf(&b, "Your previous action: intervene=%v message=%q reason=%q\n", in.Last.Intervene, in.Last.Message, in.Last.Reason)
		b.WriteString("Do NOT repeat a steering message you already sent unless there is new evidence.\n")
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
	b.WriteString("\nDecide whether to intervene now, which patterns to keep watching, and whether you are done.")
	return b.String()
}
