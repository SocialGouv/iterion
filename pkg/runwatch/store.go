// Package runwatch persists the link between a failed target run and the
// conversational assistant run that will inspect it. It deliberately does not
// share the native-ticket watch path: the delivery semantics are different
// (durable episodes + a claimed assistant turn instead of a lossy inbox).
package runwatch

import (
	"context"
	"errors"
	"time"
)

var (
	ErrNotFound      = errors.New("runwatch: not found")
	ErrAlreadyExists = errors.New("runwatch: active watch already exists")
)

// maxEpisodeReadLimit bounds read-only health projections independently of
// caller input. Delivery's due-page limit remains a separate concern.
const maxEpisodeReadLimit = 250

type Mode string

const (
	ModeDiagnose Mode = "diagnose"
	ModePropose  Mode = "propose"
)

type WatchState string

const (
	WatchActive   WatchState = "active"
	WatchResolved WatchState = "resolved"
	WatchStopped  WatchState = "stopped"
)

type EpisodeState string

const (
	EpisodePending    EpisodeState = "pending"
	EpisodeProcessing EpisodeState = "processing"
	EpisodeDone       EpisodeState = "done"
	EpisodeBlocked    EpisodeState = "blocked"
)

// RunObservation is the durable reconciliation position for one run in a
// watched tree. FirstObservedAt is diagnostic only: replay eligibility is
// governed by Watch.TreeTrackingStartedAt so a short-lived child cannot fall
// through the gap between two coordinator sweeps.
type RunObservation struct {
	RunID           string    `json:"run_id" bson:"run_id"`
	EventSeq        int64     `json:"event_seq" bson:"event_seq"`
	FirstObservedAt time.Time `json:"first_observed_at" bson:"first_observed_at"`
}

type Watch struct {
	ID             string     `json:"id" bson:"_id"`
	TenantID       string     `json:"tenant_id,omitempty" bson:"tenant_id,omitempty"`
	OwnerID        string     `json:"owner_id,omitempty" bson:"owner_id,omitempty"`
	TargetRunID    string     `json:"target_run_id" bson:"target_run_id"`
	AssistantRunID string     `json:"assistant_run_id" bson:"assistant_run_id"`
	Mode           Mode       `json:"mode" bson:"mode"`
	Kinds          []string   `json:"kinds" bson:"kinds"`
	State          WatchState `json:"state" bson:"state"`
	// MaxEpisodes is retained for persisted/wire compatibility with watches
	// created before watches became lifetime-bound. It is no longer enforced:
	// an active watch ends only when its target finishes or it is explicitly
	// stopped (apart from undeliverable ownership cleanup).
	MaxEpisodes       int        `json:"max_episodes" bson:"max_episodes"`
	DeliveredEpisodes int        `json:"delivered_episodes" bson:"delivered_episodes"`
	CooldownSeconds   int        `json:"cooldown_seconds" bson:"cooldown_seconds"`
	LastDeliveredAt   *time.Time `json:"last_delivered_at,omitempty" bson:"last_delivered_at,omitempty"`
	// LastObservedEventSeq is the durable reconciliation cursor for
	// run-health events on TargetRunID. -1 means the target had no events at
	// watch creation. A pending/suppressed stall deliberately leaves this
	// immediately before the health event so a later sweep can re-evaluate it.
	LastObservedEventSeq int64 `json:"last_observed_event_seq" bson:"last_observed_event_seq"`
	// TreeTrackingStartedAt is the replay floor for descendant outcomes. New
	// watches set it to CreatedAt. A legacy watch stamps it once on its first
	// tree-aware sweep, baselining descendants that predate the upgrade without
	// losing children that are born and finish between later sweeps.
	TreeTrackingStartedAt *time.Time       `json:"tree_tracking_started_at,omitempty" bson:"tree_tracking_started_at,omitempty"`
	Observations          []RunObservation `json:"observations,omitempty" bson:"observations,omitempty"`
	StopReason            string           `json:"stop_reason,omitempty" bson:"stop_reason,omitempty"`
	CreatedAt             time.Time        `json:"created_at" bson:"created_at"`
	UpdatedAt             time.Time        `json:"updated_at" bson:"updated_at"`
}

type Episode struct {
	ID          string `json:"id" bson:"_id"`
	WatchID     string `json:"watch_id" bson:"watch_id"`
	TenantID    string `json:"tenant_id,omitempty" bson:"tenant_id,omitempty"`
	TargetRunID string `json:"target_run_id" bson:"target_run_id"`
	// ObservedRunID is the concrete run in the watched tree that produced the
	// outcome. Empty on legacy episodes means TargetRunID.
	ObservedRunID string `json:"observed_run_id,omitempty" bson:"observed_run_id,omitempty"`
	// AssistantRunID records who owned the watch when this episode was
	// created. Delivery always resolves the current Watch.AssistantRunID, so a
	// durable watch handoff intentionally does not rewrite historical episodes.
	AssistantRunID string `json:"assistant_run_id" bson:"assistant_run_id"`
	OutcomeEventID string `json:"outcome_event_id" bson:"outcome_event_id"`
	Kind           string `json:"kind,omitempty" bson:"kind,omitempty"`
	HealthEventSeq int64  `json:"health_event_seq,omitempty" bson:"health_event_seq,omitempty"`
	HealthNodeID   string `json:"health_node_id,omitempty" bson:"health_node_id,omitempty"`
	HealthReason   string `json:"health_reason,omitempty" bson:"health_reason,omitempty"`
	// Paused* identify the concrete human gate that woke a watcher. TargetRunID
	// remains the watched root; a subbot can be paused while that root is still
	// running, so the gate must be retained independently for safe delivery.
	PausedRunID         string       `json:"paused_run_id,omitempty" bson:"paused_run_id,omitempty"`
	PausedNodeID        string       `json:"paused_node_id,omitempty" bson:"paused_node_id,omitempty"`
	PausedInteractionID string       `json:"paused_interaction_id,omitempty" bson:"paused_interaction_id,omitempty"`
	FailureFingerprint  string       `json:"failure_fingerprint" bson:"failure_fingerprint"`
	State               EpisodeState `json:"state" bson:"state"`
	Attempts            int          `json:"attempts" bson:"attempts"`
	LeaseOwner          string       `json:"lease_owner,omitempty" bson:"lease_owner,omitempty"`
	LeaseUntil          *time.Time   `json:"lease_until,omitempty" bson:"lease_until,omitempty"`
	NextAttemptAt       time.Time    `json:"next_attempt_at" bson:"next_attempt_at"`
	LastError           string       `json:"last_error,omitempty" bson:"last_error,omitempty"`
	CreatedAt           time.Time    `json:"created_at" bson:"created_at"`
	UpdatedAt           time.Time    `json:"updated_at" bson:"updated_at"`
	DeliveredAt         *time.Time   `json:"delivered_at,omitempty" bson:"delivered_at,omitempty"`
}

// Store is purpose-built around the coordinator's claims. ClaimEpisode must
// be atomic across replicas; CreateWatch must enforce one active watcher per
// tenant/owner/target tuple.
type Store interface {
	EnsureSchema(context.Context) error
	CreateWatch(context.Context, Watch) error
	// ReconfigureActiveWatch updates only the configuration-owned fields of an
	// active watch that has the same tenant, owner, target, and assistant as
	// requested. It retains the watch identity, delivery ledger, health cursor,
	// and lifecycle fields so extending a watch never opens a blind spot.
	// found=false means no matching active watch exists.
	ReconfigureActiveWatch(context.Context, Watch) (watch Watch, found bool, err error)
	// TransferActiveWatch atomically hands an active owner/target link from
	// fromAssistantID to requested.AssistantRunID. The expected outgoing
	// assistant is part of the compare-and-swap: a concurrent handoff cannot
	// be overwritten. Only assistant ownership and request-owned policy fields
	// change; identity, lifecycle, delivery and tree reconciliation state stay
	// on the durable row. found=false means the expected incumbent changed.
	TransferActiveWatch(context.Context, string, Watch) (watch Watch, found bool, err error)
	GetWatch(context.Context, string) (Watch, error)
	ListActiveByTarget(context.Context, string, string) ([]Watch, error)
	ListActiveByAssistant(context.Context, string, string) ([]Watch, error)
	ListActive(context.Context, int) ([]Watch, error)
	StopWatch(context.Context, string, string, WatchState, string, time.Time) error
	AdvanceObservedEventSeq(context.Context, string, string, int64, time.Time) error
	InitializeTreeTracking(context.Context, string, string, time.Time, time.Time) (Watch, error)
	EnsureRunObservation(context.Context, string, string, RunObservation, time.Time) (RunObservation, bool, error)
	AdvanceObservedRunEventSeq(context.Context, string, string, string, int64, time.Time) error
	CreateEpisode(context.Context, Episode) (bool, error)
	// ListEpisodesByWatch returns a bounded, read-only newest-first summary
	// source for health projections. It is deliberately separate from
	// ListDueEpisodes: episodes in normal backoff are not due yet, but still
	// need to be explainable to an operator.
	ListEpisodesByWatch(context.Context, string, string, int) ([]Episode, error)
	ListDueEpisodes(context.Context, time.Time, int) ([]Episode, error)
	ClaimEpisode(context.Context, string, string, time.Time, time.Duration) (Episode, bool, error)
	ReleaseEpisode(context.Context, string, string, time.Time, string) error
	CompleteEpisode(context.Context, string, string, time.Time) error
	BlockEpisode(context.Context, string, string, time.Time, string) error
}
