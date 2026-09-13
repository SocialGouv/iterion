package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"sort"

	"github.com/SocialGouv/iterion/pkg/dsl/spec"
)

const PortExecutionVersion = 1

type PortInvocationStatus string

const (
	PortPending   PortInvocationStatus = "pending"
	PortAdmitted  PortInvocationStatus = "admitted"
	PortRunning   PortInvocationStatus = "running"
	PortSucceeded PortInvocationStatus = "succeeded"
	PortFailed    PortInvocationStatus = "failed"
	PortCanceled  PortInvocationStatus = "canceled"
	PortUncertain PortInvocationStatus = "uncertain_effect"
)

// PortExecution is stored in the existing atomic run document on both
// backends. Revision counts native coordinator commits; the enclosing run's
// CASVersion additionally excludes races with status and operator updates.
// Generation changes only during explicit recovery/invalidation. Successful
// work is reusable only after the runtime checks its complete identity.
type PortExecution struct {
	Version      int                        `json:"version" bson:"version"`
	Revision     uint64                     `json:"revision" bson:"revision"`
	Generation   uint64                     `json:"generation" bson:"generation"`
	RootRunID    string                     `json:"root_run_id" bson:"root_run_id"`
	Identity     PortExecutionIdentity      `json:"identity" bson:"identity"`
	Invocations  map[string]*PortInvocation `json:"invocations" bson:"invocations"`
	Collections  map[string]*PortCollection `json:"collections" bson:"collections"`
	Publications map[string]*PortValue      `json:"publications" bson:"publications"`
	Exports      map[string]string          `json:"exports" bson:"exports"`
	Products     []string                   `json:"products" bson:"products"`
	Budget       PortBudgetState            `json:"budget" bson:"budget"`
}

type PortExecutionIdentity struct {
	Source       string            `json:"source" bson:"source"`
	Graph        string            `json:"graph" bson:"graph"`
	Contract     string            `json:"contract" bson:"contract"`
	Policy       string            `json:"policy" bson:"policy"`
	Inputs       string            `json:"inputs" bson:"inputs"`
	Dependencies map[string]string `json:"dependencies,omitempty" bson:"dependencies,omitempty"`
}

type PortInvocation struct {
	ID               string                `json:"id" bson:"id"`
	Node             string                `json:"node" bson:"node"`
	MapIndex         *int                  `json:"map_index,omitempty" bson:"map_index,omitempty"`
	Attempt          int                   `json:"attempt" bson:"attempt"`
	Status           PortInvocationStatus  `json:"status" bson:"status"`
	Identity         PortExecutionIdentity `json:"identity" bson:"identity"`
	Inputs           map[string]string     `json:"inputs" bson:"inputs"`
	Outputs          map[string]string     `json:"outputs" bson:"outputs"`
	ChildRunID       string                `json:"child_run_id,omitempty" bson:"child_run_id,omitempty"`
	Failure          string                `json:"failure,omitempty" bson:"failure,omitempty"`
	EffectDispatched bool                  `json:"effect_dispatched,omitempty" bson:"effect_dispatched,omitempty"`
	RecoveryDecision string                `json:"recovery_decision,omitempty" bson:"recovery_decision,omitempty"`
	RecoveryAttempt  int                   `json:"recovery_attempt,omitempty" bson:"recovery_attempt,omitempty"`
}

// A collection lists stable invocation IDs in input order. Complete is valid
// only when every item succeeded; an empty collection has no item records.
// Collection publications use its ID as Producer and Attempt zero.
type PortCollection struct {
	ID       string            `json:"id" bson:"id"`
	Node     string            `json:"node" bson:"node"`
	Items    []string          `json:"items" bson:"items"`
	Complete bool              `json:"complete" bson:"complete"`
	Outputs  map[string]string `json:"outputs" bson:"outputs"`
}

// PortValue retains exact JSON on both backends (BSON stores RawMessage as
// binary). Missing inputs have no reference; null and [] remain values.
// Fingerprint covers canonical JSON, including the exact numeric spelling.
// Files are immutable captured run-file references, never workspace paths.
type PortValue struct {
	Revision    string          `json:"revision" bson:"revision"`
	Producer    string          `json:"producer" bson:"producer"`
	Attempt     int             `json:"attempt" bson:"attempt"`
	Port        string          `json:"port" bson:"port"`
	Data        json.RawMessage `json:"data" bson:"data"`
	Fingerprint string          `json:"fingerprint" bson:"fingerprint"`
	Files       []PortFileRef   `json:"files,omitempty" bson:"files,omitempty"`
}

type PortFileRef struct {
	RunID     string `json:"run_id" bson:"run_id"`
	Path      string `json:"path" bson:"path"`
	SHA256    string `json:"sha256" bson:"sha256"`
	Size      int64  `json:"size" bson:"size"`
	MediaType string `json:"media_type" bson:"media_type"`
	Producer  string `json:"producer" bson:"producer"`
	Attempt   int    `json:"attempt" bson:"attempt"`
}

type PortBudgetAmount struct {
	Tokens     int64   `json:"tokens" bson:"tokens"`
	CostUSD    float64 `json:"cost_usd" bson:"cost_usd"`
	Iterations int64   `json:"iterations" bson:"iterations"`
}

type PortBudgetState struct {
	Consumed     PortBudgetAmount            `json:"consumed" bson:"consumed"`
	Reservations map[string]PortBudgetAmount `json:"reservations" bson:"reservations"`
}

func PortValueFingerprint(data json.RawMessage) (string, error) {
	canonical, err := canonicalPortJSON(data)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

func canonicalPortJSON(data json.RawMessage) (json.RawMessage, error) {
	value, err := spec.DecodePublicJSON(data)
	if err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

func NewPortValue(revision, producer, port string, attempt int, data json.RawMessage) (*PortValue, error) {
	canonical, err := canonicalPortJSON(data)
	if err != nil {
		return nil, err
	}
	fingerprint, err := PortValueFingerprint(canonical)
	if err != nil {
		return nil, err
	}
	return &PortValue{Revision: revision, Producer: producer, Port: port, Attempt: attempt, Data: canonical, Fingerprint: fingerprint}, nil
}

// MarshalIndent applies whitespace inside RawMessage too. Normalize it on
// read so persisted values compare equally across FS, Mongo and editor JSON.
// DecodePublicJSON preserves numbers and refuses ambiguous duplicate keys.
func (v *PortValue) UnmarshalJSON(data []byte) error {
	type wire PortValue
	var decoded wire
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	canonical, err := canonicalPortJSON(decoded.Data)
	if err != nil {
		return err
	}
	decoded.Data = canonical
	*v = PortValue(decoded)
	return nil
}

func (s *PortExecution) Clone() (*PortExecution, error) {
	if s == nil {
		return nil, nil
	}
	data, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	var copy PortExecution
	err = json.Unmarshal(data, &copy)
	return &copy, err
}

// SavePortExecution commits the complete next snapshot through RunStore's
// existing atomic compare-and-swap. It does not retry a stale coordinator
// decision. A lost acknowledgment may be retried with the identical snapshot;
// callers must reload on a conflict and recompute their transition.
func SavePortExecution(ctx context.Context, s RunStore, id string, expected uint64, next *PortExecution) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	run, err := s.LoadRun(ctx, id)
	if err != nil {
		return err
	}
	if run.RuntimeSemantics != RuntimeSemanticsPortsV1 || next == nil {
		return fmt.Errorf("store: native checkpoint requires ports-v1 run: %w", ErrRunSemantics)
	}
	canonical, err := next.Clone()
	if err != nil {
		return invalidPortState("invalid snapshot payload: %v", err)
	}
	current := uint64(0)
	if run.PortExecution != nil {
		current = run.PortExecution.Revision
		if current == expected+1 && reflect.DeepEqual(run.PortExecution, canonical) {
			return nil
		}
	}
	if current != expected || next.Revision != expected+1 {
		return fmt.Errorf("store: native checkpoint revision changed: %w", ErrRunConflict)
	}
	run.PortExecution = canonical
	return s.SaveRun(ctx, run)
}

func invalidPortState(format string, args ...any) error {
	return fmt.Errorf("store: native checkpoint: %s: %w", fmt.Sprintf(format, args...), ErrRunSemantics)
}

// ValidatePortExecution validates persisted structure, not workflow schemas
// or file contents. Runtime publication must validate those before this CAS.
func ValidatePortExecution(s *PortExecution) error {
	if s == nil {
		return nil // explicitly created, before the first coordinator commit
	}
	if s.Version != PortExecutionVersion || s.Revision == 0 || s.Generation == 0 {
		return invalidPortState("unsupported version or missing revision/generation")
	}
	if err := ValidateRunID(s.RootRunID); err != nil || !IsNativeRunID(s.RootRunID) {
		return invalidPortState("invalid native root %q", s.RootRunID)
	}
	if s.Identity.Source == "" || s.Identity.Graph == "" || s.Identity.Contract == "" || s.Identity.Policy == "" || s.Identity.Inputs == "" {
		return invalidPortState("incomplete captured identity")
	}
	for id, invocation := range s.Invocations {
		if invocation == nil || id == "" || invocation.ID != id || invocation.Node == "" || invocation.Attempt < 1 || (invocation.MapIndex != nil && *invocation.MapIndex < 0) {
			return invalidPortState("invalid invocation %q", id)
		}
		if invocation.Identity.Source == "" || invocation.Identity.Contract == "" || invocation.Identity.Policy == "" {
			return invalidPortState("invocation %s has no captured implementation identity", id)
		}
		if invocation.Status != PortPending && invocation.Status != PortCanceled && invocation.Identity.Inputs == "" {
			return invalidPortState("invocation %s has no bound input fingerprint", id)
		}
		switch invocation.Status {
		case PortPending, PortAdmitted, PortRunning, PortSucceeded, PortFailed, PortCanceled, PortUncertain:
		default:
			return invalidPortState("unsupported invocation status %q", invocation.Status)
		}
		if invocation.ChildRunID != "" {
			if err := ValidateNativeChild(s.RootRunID, invocation.ChildRunID); err != nil {
				return err
			}
		}
		if invocation.Status != PortSucceeded && len(invocation.Outputs) != 0 {
			return invalidPortState("unfinished invocation %s exposes outputs", id)
		}
		for _, revision := range invocation.Inputs {
			if s.Publications[revision] == nil {
				return invalidPortState("invocation %s references unpublished input %s", id, revision)
			}
		}
		if err := validatePortOutputs(s, id, invocation.Attempt, invocation.Outputs); err != nil {
			return err
		}
	}
	for id, collection := range s.Collections {
		if collection == nil || id == "" || collection.ID != id || collection.Node == "" || s.Invocations[id] != nil {
			return invalidPortState("invalid collection %q", id)
		}
		if !collection.Complete && len(collection.Outputs) > 0 {
			return invalidPortState("incomplete collection %s exposes outputs", id)
		}
		for index, item := range collection.Items {
			invocation := s.Invocations[item]
			if invocation == nil || invocation.Node != collection.Node || invocation.MapIndex == nil || *invocation.MapIndex != index {
				return invalidPortState("collection %s item %d has no matching stable invocation", id, index)
			}
			if collection.Complete && invocation.Status != PortSucceeded {
				return invalidPortState("collection %s has an unfinished item", id)
			}
		}
		if err := validatePortOutputs(s, id, 0, collection.Outputs); err != nil {
			return err
		}
	}
	for revision, value := range s.Publications {
		if value == nil || revision == "" || value.Revision != revision || value.Port == "" {
			return invalidPortState("invalid publication %q", revision)
		}
		fingerprint, err := PortValueFingerprint(value.Data)
		if err != nil || fingerprint != value.Fingerprint {
			return invalidPortState("publication %s content fingerprint mismatch", revision)
		}
		if value.Producer == "input" {
			if value.Attempt != 0 {
				return invalidPortState("root input cannot have an execution attempt")
			}
		} else if invocation := s.Invocations[value.Producer]; invocation != nil {
			if invocation.Status != PortSucceeded || invocation.Attempt != value.Attempt || invocation.Outputs[value.Port] != revision {
				return invalidPortState("publication %s has no committed successful producer", revision)
			}
		} else if collection := s.Collections[value.Producer]; collection != nil {
			if !collection.Complete || value.Attempt != 0 || collection.Outputs[value.Port] != revision {
				return invalidPortState("publication %s has no committed complete collection", revision)
			}
		} else {
			return invalidPortState("publication %s has unknown producer", revision)
		}
		for _, file := range value.Files {
			if err := ValidateNativeChild(s.RootRunID, file.RunID); err != nil {
				return err
			}
			_, clean, pathErr := cleanRunFilePath(file.Path)
			_, digestErr := hex.DecodeString(file.SHA256)
			if file.Producer != value.Producer || file.Attempt != value.Attempt || file.Size < 0 || len(file.SHA256) != 64 || digestErr != nil || pathErr != nil || clean != file.Path {
				return invalidPortState("publication %s file provenance mismatch", revision)
			}
		}
	}
	for name, revision := range s.Exports {
		if name == "" || s.Publications[revision] == nil {
			return invalidPortState("export %s references unpublished result", name)
		}
	}
	if !validPortBudgetAmount(s.Budget.Consumed) {
		return invalidPortState("invalid consumed budget")
	}
	for id, amount := range s.Budget.Reservations {
		invocation := s.Invocations[id]
		if !validPortBudgetAmount(amount) || invocation == nil || (invocation.Status != PortAdmitted && invocation.Status != PortRunning && invocation.Status != PortUncertain) {
			return invalidPortState("reservation %s has no active invocation", id)
		}
	}
	return nil
}

func validatePortOutputs(s *PortExecution, producer string, attempt int, outputs map[string]string) error {
	for port, revision := range outputs {
		value := s.Publications[revision]
		if value == nil || value.Producer != producer || value.Attempt != attempt || value.Port != port {
			return invalidPortState("producer %s output %s has no matching publication", producer, port)
		}
	}
	return nil
}

func validPortBudgetAmount(value PortBudgetAmount) bool {
	return value.Tokens >= 0 && value.Iterations >= 0 && value.CostUSD >= 0 && !math.IsInf(value.CostUSD, 0) && !math.IsNaN(value.CostUSD)
}

func checkPortExecutionTransition(previous, next *PortExecution) error {
	if reflect.DeepEqual(previous, next) {
		return nil // ordinary metadata writers preserve the checkpoint
	}
	if next == nil {
		return invalidPortState("cannot discard an existing checkpoint")
	}
	if previous == nil {
		if next.Revision != 1 || next.Generation != 1 {
			return invalidPortState("first checkpoint must start at revision/generation one")
		}
		for _, invocation := range next.Invocations {
			if invocation.Status != PortPending {
				return invalidPortState("first checkpoint cannot contain dispatched work")
			}
		}
		return nil
	}
	if next.Revision != previous.Revision+1 || next.RootRunID != previous.RootRunID || next.Generation < previous.Generation || next.Generation > previous.Generation+1 {
		return invalidPortState("non-monotonic revision/generation or changed root")
	}
	if next.Budget.Consumed.Tokens < previous.Budget.Consumed.Tokens || next.Budget.Consumed.CostUSD < previous.Budget.Consumed.CostUSD || next.Budget.Consumed.Iterations < previous.Budget.Consumed.Iterations {
		return invalidPortState("consumed root budget cannot decrease, including during recovery")
	}
	if next.Generation != previous.Generation {
		for _, state := range []*PortExecution{previous, next} {
			for _, invocation := range state.Invocations {
				if invocation.Status == PortAdmitted || invocation.Status == PortRunning || invocation.Status == PortUncertain {
					return invalidPortState("generation cannot change while work or effects remain unresolved")
				}
			}
		}
		return nil // runtime has explicitly invalidated affected descendants
	}
	if !reflect.DeepEqual(previous.Identity, next.Identity) {
		return invalidPortState("captured identity changed without invalidation")
	}
	ids := make([]string, 0, len(previous.Invocations))
	for id := range previous.Invocations {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		before, after := previous.Invocations[id], next.Invocations[id]
		if after == nil || before.Node != after.Node || !reflect.DeepEqual(before.MapIndex, after.MapIndex) {
			return invalidPortState("invocation %s identity changed without invalidation", id)
		}
		beforeIdentity, afterIdentity := before.Identity, after.Identity
		if before.Status == PortPending {
			beforeIdentity.Inputs, afterIdentity.Inputs = "", ""
		} else if !reflect.DeepEqual(before.Inputs, after.Inputs) {
			return invalidPortState("invocation %s bound inputs changed without invalidation", id)
		}
		if !reflect.DeepEqual(beforeIdentity, afterIdentity) {
			return invalidPortState("invocation %s implementation changed without invalidation", id)
		}
		if before.Status == PortSucceeded && !reflect.DeepEqual(before, after) {
			return invalidPortState("successful invocation %s is immutable until invalidation", id)
		}
		if before.Attempt != after.Attempt {
			retryable := before.Status == PortFailed || before.Status == PortCanceled || (before.Status == PortUncertain && after.RecoveryDecision != "" && after.RecoveryAttempt == before.Attempt)
			if !retryable || after.Attempt != before.Attempt+1 || after.Status != PortPending || after.EffectDispatched {
				return invalidPortState("invocation %s has an invalid retry attempt", id)
			}
		} else {
			decision := after.RecoveryDecision != "" && after.RecoveryAttempt == before.Attempt
			if !portTransitionAllowed(before.Status, after.Status, decision) || (before.EffectDispatched && !after.EffectDispatched) {
				return invalidPortState("invocation %s cannot transition from %s to %s", id, before.Status, after.Status)
			}
			if before.EffectDispatched && (after.Status == PortFailed || after.Status == PortCanceled) && !decision {
				return invalidPortState("invocation %s requires uncertain-effect recovery", id)
			}
		}
	}
	for id, invocation := range next.Invocations {
		if previous.Invocations[id] == nil && invocation.Status != PortPending {
			return invalidPortState("new invocation %s must be pending before admission", id)
		}
	}
	for revision, value := range previous.Publications {
		if !reflect.DeepEqual(value, next.Publications[revision]) {
			return invalidPortState("published revision %s is immutable until invalidation", revision)
		}
	}
	for id, collection := range previous.Collections {
		after := next.Collections[id]
		if after == nil || collection.Node != after.Node || !reflect.DeepEqual(collection.Items, after.Items) || (collection.Complete && !reflect.DeepEqual(collection, after)) {
			return invalidPortState("collection %s changed its stable items or completed result", id)
		}
	}
	return nil
}

func portTransitionAllowed(before, after PortInvocationStatus, decision bool) bool {
	if before == after {
		return true
	}
	switch before {
	case PortPending:
		return after == PortAdmitted || after == PortCanceled
	case PortAdmitted:
		return after == PortRunning || after == PortCanceled
	case PortRunning:
		return after == PortSucceeded || after == PortFailed || after == PortCanceled || after == PortUncertain
	case PortUncertain:
		return decision && (after == PortSucceeded || after == PortFailed || after == PortCanceled)
	default:
		return false
	}
}
