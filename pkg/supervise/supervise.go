// Package supervise implements LLM-driven supervisor agents that watch
// a running iterion workflow from a separate goroutine/process and
// enqueue steering messages the supervised run picks up at its next
// turn — like a human watching a Claude Code session and typing.
//
// The supervisor is scoped to one or more *agent nodes* (Spec.Watches):
// it is only "armed" (evaluates / injects) while one of those nodes is
// the active executing node, and each injected message is tagged with
// that node (store.QueuedUserMessage.NodeID) so a late message can't
// leak into the next node. Watching the whole run is the degenerate
// case (empty Watches).
//
// The coordinator mirrors pkg/server.watchCoordinator: it subscribes to
// the target run's event stream and calls runview.Service.QueueMessage,
// swapping watchCoordinator's kanban-issue trigger for an LLM decision.
package supervise

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// Defaults for the per-supervisor knobs. Cooldown bounds how often the
// LLM is consulted on quiet turn boundaries; MaxEvals is the hard
// token-budget backstop after which supervision degrades to a no-op.
const (
	DefaultCooldown = 30 * time.Second
	DefaultMaxEvals = 20
)

// Spec is a resolved supervisor configuration — the input to a
// Coordinator. Both the DSL `supervisor <name>:` declaration (engine
// path) and the `iterion supervise` CLI produce a Spec.
type Spec struct {
	// Name labels the supervisor in logs and injected-message framing.
	Name string
	// Model is the claw model spec ("provider/model", e.g.
	// "anthropic/claude-opus-4-8"). Empty => auto-detect a reachable
	// provider (see resolveModel).
	Model string
	// System is the resolved system-prompt text: the supervision policy
	// (what to watch for, when to intervene, how forcefully).
	System string
	// Watches lists the agent-node ids this supervisor steers. Empty =>
	// whole run (armed for every node; messages run-scoped).
	Watches []string
	// Monitors are event patterns the supervisor starts with; the bot
	// can register more at runtime via its decision.
	Monitors []Monitor
	// Cooldown is the minimum wall-clock between LLM evaluations on
	// plain turn boundaries (monitor-match wakes bypass it). Zero =>
	// DefaultCooldown.
	Cooldown time.Duration
	// MaxEvals caps total LLM evaluations for the run. Zero =>
	// DefaultMaxEvals. Once exhausted the coordinator stops evaluating.
	MaxEvals int
}

// withDefaults returns a copy of the spec with zero knobs filled in.
func (s Spec) withDefaults() Spec {
	if s.Cooldown <= 0 {
		s.Cooldown = DefaultCooldown
	}
	if s.MaxEvals <= 0 {
		s.MaxEvals = DefaultMaxEvals
	}
	return s
}

// watchesNode reports whether nodeID is in the supervisor's watch set.
// Empty Watches => watches every node (run scope).
func (s Spec) watchesNode(nodeID string) bool {
	if len(s.Watches) == 0 {
		return true
	}
	return slices.Contains(s.Watches, nodeID)
}

// Monitor is an event pattern the supervisor registers interest in.
// Every set field must match for the monitor to fire; unset fields are
// wildcards. A match wakes the coordinator immediately (bypassing the
// turn-boundary cooldown) — this is the "launch monitors on events
// related to the ongoing activity" surface.
type Monitor struct {
	// EventType matches store.EventType verbatim (e.g. "tool_error",
	// "node_finished", "budget_warning").
	EventType string `json:"event_type,omitempty"`
	// NodeID matches the event's NodeID.
	NodeID string `json:"node_id,omitempty"`
	// ToolName matches the event Data["tool"] / Data["tool_name"].
	ToolName string `json:"tool_name,omitempty"`
	// TextContains is a case-insensitive substring matched against the
	// rendered event (type + node + JSON data) — e.g. a file path, an
	// error fragment, a failing-test marker.
	TextContains string `json:"text_contains,omitempty"`
	// CostGt fires on a budget_warning whose Data["used"] exceeds this
	// (USD or token count, whichever the dimension reports).
	CostGt float64 `json:"cost_gt,omitempty"`
}

// matches reports whether evt satisfies every set field of the monitor.
func (m Monitor) matches(evt *store.Event) bool {
	if evt == nil {
		return false
	}
	if m.EventType != "" && string(evt.Type) != m.EventType {
		return false
	}
	if m.NodeID != "" && evt.NodeID != m.NodeID {
		return false
	}
	if m.ToolName != "" && !strings.EqualFold(eventToolName(evt), m.ToolName) {
		return false
	}
	// Any non-zero CostGt engages the budget branch. Validation refuses
	// non-positive values, but a value that slips through must constrain,
	// never fall through to the final !isEmpty() — a NaN/negative that
	// skipped this branch would turn the monitor into a match-everything
	// wildcard that drains the eval budget on the first burst of events.
	if m.CostGt != 0 {
		// A threshold that is not a positive finite number cannot
		// constrain anything — treat it as "never", not as satisfied.
		if !(m.CostGt > 0) {
			return false
		}
		if evt.Type != store.EventBudgetWarning {
			return false
		}
		if used, ok := numField(evt.Data, "used"); !ok || used <= m.CostGt {
			return false
		}
	}
	if m.TextContains != "" && !strings.Contains(strings.ToLower(RenderEvent(evt)), strings.ToLower(m.TextContains)) {
		return false
	}
	// A monitor with no fields set is treated as "never" (avoids a
	// wildcard that would match every event and defeat cooldown).
	return !m.isEmpty()
}

func (m Monitor) isEmpty() bool {
	return m.EventType == "" && m.NodeID == "" && m.ToolName == "" && m.TextContains == "" && m.CostGt == 0
}

// eventToolName extracts the tool name from a tool event's data,
// tolerating the two keys the emit sites use ("tool" in the claw/
// claude_code hooks, "tool_name" in the per-step summary).
func eventToolName(evt *store.Event) string {
	if evt.Data == nil {
		return ""
	}
	for _, k := range []string{"tool", "tool_name"} {
		if s, ok := evt.Data[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// numField reads a numeric data field, tolerating int/int64/float64
// (JSON round-trips through float64; in-process events may carry ints).
func numField(data map[string]any, key string) (float64, bool) {
	if data == nil {
		return 0, false
	}
	switch v := data[key].(type) {
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	default:
		return 0, false
	}
}

// RenderEvent produces a compact one-line rendering of an event for the
// supervisor prompt and for TextContains matching. Stable and cheap.
func RenderEvent(evt *store.Event) string {
	if evt == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "#%d %s", evt.Seq, evt.Type)
	if evt.NodeID != "" {
		fmt.Fprintf(&b, " node=%s", evt.NodeID)
	}
	if len(evt.Data) > 0 {
		if data, err := json.Marshal(evt.Data); err == nil {
			b.WriteString(" ")
			b.Write(data)
		}
	}
	return b.String()
}

// IsTurnBoundary reports whether an event marks a point where a watched
// agent yields control — a good moment to (re-)evaluate. Exported so the
// session-board coordinator reuses the same classification instead of
// duplicating it.
func IsTurnBoundary(evt *store.Event) bool {
	switch evt.Type {
	// assistant_text is the only per-turn signal a CLI-agent backend
	// (claude_code) emits — without it a supervised claude_code node has
	// no debounced wake between node_started and node_finished, and the
	// cooldown governs nothing. claw emits both; the debounce coalesces.
	case store.EventLLMStepFinished, store.EventAssistantText, store.EventNodeFinished, store.EventNodeStarted, store.EventRunPaused:
		return true
	default:
		return false
	}
}

// IsTerminal reports whether an event signals the run has ended, so a
// coordinator can self-close. Exported alongside IsTurnBoundary.
func IsTerminal(evt *store.Event) bool {
	switch evt.Type {
	case store.EventRunFinished, store.EventRunFailed, store.EventRunCancelled:
		return true
	default:
		return false
	}
}
