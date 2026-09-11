// Package assistantmission persists bounded, host-authorized control loops
// between a conversational assistant and one target run.
package assistantmission

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

const (
	SchemaVersion   = 1
	ContractVersion = "assistant-actions/v1"
	ActionResume    = "run.resume"
	ActionRewind    = "run.rewind"
)

var (
	ErrNotFound  = errors.New("assistantmission: not found")
	ErrConflict  = errors.New("assistantmission: binding or policy conflict")
	ErrClaimLost = errors.New("assistantmission: claim lost")
)

type State string

const (
	StateActive            State = "active"
	StateWaitingHuman      State = "waiting_human"
	StateCapabilityBlocked State = "capability_blocked"
	StateCompleted         State = "completed"
	StateExpired           State = "expired"
	StateStopped           State = "stopped"
	StateExhausted         State = "exhausted"
)

func (s State) Terminal() bool {
	switch s {
	case StateCompleted, StateExpired, StateStopped, StateExhausted:
		return true
	default:
		return false
	}
}

type ReceiptState string

const (
	ReceiptPrepared  ReceiptState = "prepared"
	ReceiptIssued    ReceiptState = "issued"
	ReceiptSucceeded ReceiptState = "succeeded"
	ReceiptRejected  ReceiptState = "rejected"
	ReceiptUncertain ReceiptState = "uncertain"
)

type Policy struct {
	Actions         []string  `json:"actions" bson:"actions"`
	TTLSeconds      int64     `json:"ttl_seconds" bson:"ttl_seconds"`
	ExpiresAt       time.Time `json:"expires_at" bson:"expires_at"`
	MaxActions      int       `json:"max_actions" bson:"max_actions"`
	ContractVersion string    `json:"contract_version" bson:"contract_version"`
}

func (p Policy) Canonical() Policy {
	p.Actions = append([]string(nil), p.Actions...)
	sort.Strings(p.Actions)
	return p
}

func (p Policy) Equal(other Policy) bool {
	p, other = p.Canonical(), other.Canonical()
	if p.TTLSeconds != other.TTLSeconds || p.MaxActions != other.MaxActions || p.ContractVersion != other.ContractVersion || len(p.Actions) != len(other.Actions) {
		return false
	}
	for i := range p.Actions {
		if p.Actions[i] != other.Actions[i] {
			return false
		}
	}
	return true
}

type DeliveryReceipt struct {
	ID          string       `json:"id" bson:"id"`
	Kind        string       `json:"kind" bson:"kind"`
	State       ReceiptState `json:"state" bson:"state"`
	AttemptedAt *time.Time   `json:"attempted_at,omitempty" bson:"attempted_at,omitempty"`
	CompletedAt *time.Time   `json:"completed_at,omitempty" bson:"completed_at,omitempty"`
	Error       string       `json:"error,omitempty" bson:"error,omitempty"`
}

type ActionReceipt struct {
	ID              string           `json:"id" bson:"id"`
	Action          string           `json:"action" bson:"action"`
	Digest          string           `json:"digest" bson:"digest"`
	AssistantRunID  string           `json:"assistant_run_id" bson:"assistant_run_id"`
	ArtifactNodeID  string           `json:"artifact_node_id" bson:"artifact_node_id"`
	ArtifactVersion int              `json:"artifact_version" bson:"artifact_version"`
	ProposalIndex   int              `json:"proposal_index" bson:"proposal_index"`
	Args            map[string]any   `json:"args" bson:"args"`
	ExpectedStatus  store.RunStatus  `json:"expected_status,omitempty" bson:"expected_status,omitempty"`
	ExpectedPivot   string           `json:"expected_pivot,omitempty" bson:"expected_pivot,omitempty"`
	State           ReceiptState     `json:"state" bson:"state"`
	CreatedAt       time.Time        `json:"created_at" bson:"created_at"`
	UpdatedAt       time.Time        `json:"updated_at" bson:"updated_at"`
	EventSeq        int64            `json:"event_seq,omitempty" bson:"event_seq,omitempty"`
	Error           string           `json:"error,omitempty" bson:"error,omitempty"`
	ResultDelivery  *DeliveryReceipt `json:"result_delivery,omitempty" bson:"result_delivery,omitempty"`
}

type Mission struct {
	Version           int              `json:"version" bson:"version"`
	ID                string           `json:"id" bson:"_id"`
	InvocationKey     string           `json:"invocation_key" bson:"invocation_key"`
	TenantID          string           `json:"tenant_id,omitempty" bson:"tenant_id,omitempty"`
	ProjectID         string           `json:"project_id" bson:"project_id"`
	OperatorID        string           `json:"operator_id" bson:"operator_id"`
	TargetRunID       string           `json:"target_run_id" bson:"target_run_id"`
	WatchID           string           `json:"watch_id" bson:"watch_id"`
	AssistantRunID    string           `json:"assistant_run_id" bson:"assistant_run_id"`
	Policy            Policy           `json:"policy" bson:"policy"`
	State             State            `json:"state" bson:"state"`
	Reason            string           `json:"reason,omitempty" bson:"reason,omitempty"`
	Activation        *DeliveryReceipt `json:"activation,omitempty" bson:"activation,omitempty"`
	ProposalFrontier  map[string]int   `json:"proposal_frontier,omitempty" bson:"proposal_frontier,omitempty"`
	Receipts          []ActionReceipt  `json:"receipts,omitempty" bson:"receipts,omitempty"`
	DispatchedActions int              `json:"dispatched_actions" bson:"dispatched_actions"`
	Revision          int64            `json:"revision" bson:"revision"`
	LeaseOwner        string           `json:"lease_owner,omitempty" bson:"lease_owner,omitempty"`
	LeaseEpoch        int64            `json:"lease_epoch,omitempty" bson:"lease_epoch,omitempty"`
	LeaseUntil        *time.Time       `json:"lease_until,omitempty" bson:"lease_until,omitempty"`
	CreatedAt         time.Time        `json:"created_at" bson:"created_at"`
	UpdatedAt         time.Time        `json:"updated_at" bson:"updated_at"`
	TerminalAt        *time.Time       `json:"terminal_at,omitempty" bson:"terminal_at,omitempty"`
	// ActiveTargetKey exists only for non-terminal Mongo documents and backs
	// the unique active target invariant across operators.
	ActiveTargetKey string `json:"-" bson:"active_target_key,omitempty"`
}

func (m Mission) SameRequest(other Mission) bool {
	return m.TenantID == other.TenantID && m.OperatorID == other.OperatorID && m.ProjectID == other.ProjectID &&
		m.TargetRunID == other.TargetRunID && m.WatchID == other.WatchID && m.AssistantRunID == other.AssistantRunID && m.Policy.Equal(other.Policy)
}

type Scope struct {
	TenantID    string
	OperatorID  string
	TargetRunID string
}

type Store interface {
	EnsureSchema(context.Context) error
	CreateOrGet(context.Context, Mission) (Mission, bool, error)
	Get(context.Context, Scope, string) (Mission, error)
	GetByInvocation(context.Context, Scope, string) (Mission, error)
	List(context.Context, Scope, int) ([]Mission, error)
	ListReconcileCandidates(context.Context, time.Time, int) ([]Mission, error)
	Claim(context.Context, string, string, time.Time, time.Duration) (Mission, bool, error)
	UpdateClaimed(context.Context, Mission, string) (Mission, error)
	RequestStop(context.Context, Scope, string, string, time.Time) (Mission, error)
}
