// Package lane implements lane-level event tracking, commit provenance,
// branch locking, and freshness analysis for multi-lane orchestration.
package lane

import (
	"encoding/json"
	"fmt"
)

// DetailCompressor is a pluggable function for compressing detail strings.
// The runtime package sets this to summary_compression.CompressSummaryText
// at init time, avoiding a circular import from lane → runtime.
// When nil, Finished() passes detail through uncompressed.
var DetailCompressor func(string) string

// LaneEventName identifies the type of lane event on the wire.
type LaneEventName string

const (
	EventStarted                LaneEventName = "lane.started"
	EventReady                  LaneEventName = "lane.ready"
	EventPromptMisdelivery      LaneEventName = "lane.prompt_misdelivery"
	EventBlocked                LaneEventName = "lane.blocked"
	EventRed                    LaneEventName = "lane.red"
	EventGreen                  LaneEventName = "lane.green"
	EventCommitCreated          LaneEventName = "lane.commit.created"
	EventPrOpened               LaneEventName = "lane.pr.opened"
	EventMergeReady             LaneEventName = "lane.merge.ready"
	EventFinished               LaneEventName = "lane.finished"
	EventFailed                 LaneEventName = "lane.failed"
	EventReconciled             LaneEventName = "lane.reconciled"
	EventMerged                 LaneEventName = "lane.merged"
	EventSuperseded             LaneEventName = "lane.superseded"
	EventClosed                 LaneEventName = "lane.closed"
	EventBranchStaleAgainstMain LaneEventName = "branch.stale_against_main"
)

// LaneEventStatus describes the lane's current status.
type LaneEventStatus string

const (
	StatusRunning    LaneEventStatus = "running"
	StatusReady      LaneEventStatus = "ready"
	StatusBlocked    LaneEventStatus = "blocked"
	StatusRed        LaneEventStatus = "red"
	StatusGreen      LaneEventStatus = "green"
	StatusCompleted  LaneEventStatus = "completed"
	StatusFailed     LaneEventStatus = "failed"
	StatusReconciled LaneEventStatus = "reconciled"
	StatusMerged     LaneEventStatus = "merged"
	StatusSuperseded LaneEventStatus = "superseded"
	StatusClosed     LaneEventStatus = "closed"
)

// LaneFailureClass categorizes the type of failure.
type LaneFailureClass string

const (
	FailurePromptDelivery   LaneFailureClass = "prompt_delivery"
	FailureTrustGate        LaneFailureClass = "trust_gate"
	FailureBranchDivergence LaneFailureClass = "branch_divergence"
	FailureCompile          LaneFailureClass = "compile"
	FailureTest             LaneFailureClass = "test"
	FailurePluginStartup    LaneFailureClass = "plugin_startup"
	FailureMcpStartup       LaneFailureClass = "mcp_startup"
	FailureMcpHandshake     LaneFailureClass = "mcp_handshake"
	FailureGatewayRouting   LaneFailureClass = "gateway_routing"
	FailureToolRuntime      LaneFailureClass = "tool_runtime"
	FailureInfra            LaneFailureClass = "infra"
)

// LaneEventBlocker describes what is blocking a lane.
type LaneEventBlocker struct {
	FailureClass LaneFailureClass `json:"failureClass"`
	Detail       string           `json:"detail"`
}

// LaneCommitProvenance tracks the provenance of a commit within a lane.
type LaneCommitProvenance struct {
	Commit          string   `json:"commit"`
	Branch          string   `json:"branch"`
	Worktree        *string  `json:"worktree,omitempty"`
	CanonicalCommit *string  `json:"canonicalCommit,omitempty"`
	SupersededBy    *string  `json:"supersededBy,omitempty"`
	Lineage         []string `json:"lineage,omitempty"`
}

// LaneEventData is a sealed interface for typed lane event payloads.
// Implementations must provide the unexported marker method. This replaces
// the previous json.RawMessage Data field for type safety while maintaining
// wire compatibility via custom JSON marshaling.
type LaneEventData interface {
	isLaneEventData() // sealed marker — only types in this package may implement

	// RawJSON returns the JSON encoding of the typed payload. This provides
	// backward compatibility for callers that previously used json.RawMessage.
	RawJSON() json.RawMessage
}

// CommitProvenanceData wraps LaneCommitProvenance as a typed event payload.
type CommitProvenanceData struct {
	LaneCommitProvenance
}

func (CommitProvenanceData) isLaneEventData() {}

// RawJSON returns the JSON encoding of the commit provenance.
func (d CommitProvenanceData) RawJSON() json.RawMessage {
	data, _ := json.Marshal(d.LaneCommitProvenance)
	return data
}

// RawEventData wraps a json.RawMessage for backward compatibility with
// untyped payloads during the transition period.
type RawEventData struct {
	Raw json.RawMessage
}

func (RawEventData) isLaneEventData() {}

// RawJSON returns the underlying raw JSON.
func (d RawEventData) RawJSON() json.RawMessage {
	return d.Raw
}

// StaleBranchEventData wraps a StaleBranchEvent as a typed event payload.
type StaleBranchEventData struct {
	StaleBranchEvent
}

func (StaleBranchEventData) isLaneEventData() {}

// RawJSON returns the JSON encoding of the stale branch event.
func (d StaleBranchEventData) RawJSON() json.RawMessage {
	data, _ := json.Marshal(d.StaleBranchEvent)
	return data
}

// StaleBranchEvent describes a branch staleness event with 3 variants.
// Matches Rust's StaleBranchEvent enum.
type StaleBranchEvent struct {
	Kind   StaleBranchEventKind `json:"kind"`
	Detail string               `json:"detail,omitempty"`
	Behind int                  `json:"behind,omitempty"`
	Ahead  int                  `json:"ahead,omitempty"`
	Fixes  []string             `json:"fixes,omitempty"`
}

// StaleBranchEventKind identifies the variant of StaleBranchEvent.
type StaleBranchEventKind string

const (
	StaleBranchFresh    StaleBranchEventKind = "fresh"
	StaleBranchStale    StaleBranchEventKind = "stale"
	StaleBranchDiverged StaleBranchEventKind = "diverged"
)

// String returns the string representation of the kind.
func (k StaleBranchEventKind) String() string {
	return string(k)
}

// LaneEvent is a single append-only lane event record.
type LaneEvent struct {
	Event        LaneEventName     `json:"event"`
	Status       LaneEventStatus   `json:"status"`
	EmittedAt    string            `json:"emittedAt"`
	FailureClass *LaneFailureClass `json:"failureClass,omitempty"`
	Detail       *string           `json:"detail,omitempty"`

	// TypedData holds the typed event payload. Use this for type-safe access.
	TypedData LaneEventData `json:"-"`

	// Data is the JSON wire representation. It is populated automatically
	// during marshaling from TypedData, and populated during unmarshaling
	// for backward compatibility.
	Data json.RawMessage `json:"data,omitempty"`
}

// MarshalJSON produces the wire representation. If TypedData is set, its
// RawJSON output is used as the "data" field.
func (e LaneEvent) MarshalJSON() ([]byte, error) {
	type alias LaneEvent // avoid recursion
	a := alias(e)
	if e.TypedData != nil {
		a.Data = e.TypedData.RawJSON()
	}
	return json.Marshal(a)
}

// UnmarshalJSON reads a LaneEvent and attempts to reconstruct TypedData from
// the event type and raw data for known payload types.
func (e *LaneEvent) UnmarshalJSON(data []byte) error {
	type alias LaneEvent
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return fmt.Errorf("lane event: %w", err)
	}
	*e = LaneEvent(a)

	// Reconstruct typed data for known event types.
	if len(e.Data) > 0 {
		switch e.Event {
		case EventCommitCreated, EventSuperseded:
			var prov LaneCommitProvenance
			if json.Unmarshal(e.Data, &prov) == nil {
				e.TypedData = CommitProvenanceData{prov}
			}
		case EventBranchStaleAgainstMain:
			var sbe StaleBranchEvent
			if json.Unmarshal(e.Data, &sbe) == nil {
				e.TypedData = StaleBranchEventData{sbe}
			}
		default:
			e.TypedData = RawEventData{Raw: e.Data}
		}
	}
	return nil
}

// NewLaneEvent creates a base LaneEvent.
func NewLaneEvent(event LaneEventName, status LaneEventStatus, emittedAt string) LaneEvent {
	return LaneEvent{
		Event:     event,
		Status:    status,
		EmittedAt: emittedAt,
	}
}

// Started creates a lane.started event.
func Started(emittedAt string) LaneEvent {
	return NewLaneEvent(EventStarted, StatusRunning, emittedAt)
}

// Finished creates a lane.finished event. If a DetailCompressor is set and
// detail is non-nil and non-empty, the detail is compressed before storage.
func Finished(emittedAt string, detail *string) LaneEvent {
	e := NewLaneEvent(EventFinished, StatusCompleted, emittedAt)
	if detail != nil && *detail != "" && DetailCompressor != nil {
		compressed := DetailCompressor(*detail)
		e.Detail = &compressed
	} else {
		e.Detail = detail
	}
	return e
}

// CommitCreated creates a lane.commit.created event with provenance data.
func CommitCreated(emittedAt string, detail *string, provenance LaneCommitProvenance) LaneEvent {
	e := NewLaneEvent(EventCommitCreated, StatusCompleted, emittedAt)
	e.Detail = detail
	e.TypedData = CommitProvenanceData{provenance}
	e.Data = e.TypedData.RawJSON()
	return e
}

// Superseded creates a lane.superseded event with provenance data.
func Superseded(emittedAt string, detail *string, provenance LaneCommitProvenance) LaneEvent {
	e := NewLaneEvent(EventSuperseded, StatusSuperseded, emittedAt)
	e.Detail = detail
	e.TypedData = CommitProvenanceData{provenance}
	e.Data = e.TypedData.RawJSON()
	return e
}

// BranchStale creates a branch.stale_against_main event with stale branch data.
func BranchStale(emittedAt string, sbe StaleBranchEvent) LaneEvent {
	e := NewLaneEvent(EventBranchStaleAgainstMain, StatusBlocked, emittedAt)
	detail := sbe.Detail
	if detail != "" {
		e.Detail = &detail
	}
	e.TypedData = StaleBranchEventData{sbe}
	e.Data = e.TypedData.RawJSON()
	return e
}

// Blocked creates a lane.blocked event from a blocker.
func Blocked(emittedAt string, blocker *LaneEventBlocker) LaneEvent {
	e := NewLaneEvent(EventBlocked, StatusBlocked, emittedAt)
	fc := blocker.FailureClass
	e.FailureClass = &fc
	d := blocker.Detail
	e.Detail = &d
	return e
}

// Failed creates a lane.failed event from a blocker.
func Failed(emittedAt string, blocker *LaneEventBlocker) LaneEvent {
	e := NewLaneEvent(EventFailed, StatusFailed, emittedAt)
	fc := blocker.FailureClass
	e.FailureClass = &fc
	d := blocker.Detail
	e.Detail = &d
	return e
}

// DedupeSupersededCommitEvents removes superseded commit events,
// keeping only the latest event per canonical commit key.
func DedupeSupersededCommitEvents(events []LaneEvent) []LaneEvent {
	keep := make([]bool, len(events))
	for i := range keep {
		keep[i] = true
	}

	latestByKey := make(map[string]int) // canonical commit -> index of latest

	for i, event := range events {
		if event.Event != EventCommitCreated {
			continue
		}
		if event.Data == nil {
			continue
		}
		var data map[string]interface{}
		if json.Unmarshal(event.Data, &data) != nil {
			continue
		}

		// Check if superseded
		if _, ok := data["supersededBy"]; ok {
			if v, isStr := data["supersededBy"].(string); isStr && v != "" {
				keep[i] = false
				continue
			}
		}

		// Find the key (canonicalCommit or commit)
		var key string
		if cc, ok := data["canonicalCommit"].(string); ok && cc != "" {
			key = cc
		} else if c, ok := data["commit"].(string); ok && c != "" {
			key = c
		}
		if key != "" {
			if prev, exists := latestByKey[key]; exists {
				keep[prev] = false
			}
			latestByKey[key] = i
		}
	}

	var result []LaneEvent
	for i, event := range events {
		if keep[i] {
			result = append(result, event)
		}
	}
	return result
}
