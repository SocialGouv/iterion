package worker

import (
	"github.com/SocialGouv/claw-code-go/internal/runtime/trust"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ---------------------------------------------------------------------------
// WorkerStatus
// ---------------------------------------------------------------------------

type WorkerStatus int

const (
	StatusSpawning WorkerStatus = iota
	StatusTrustRequired
	StatusReadyForPrompt
	StatusRunning
	StatusFinished
	StatusFailed
)

var workerStatusNames = [...]string{
	"spawning",
	"trust_required",
	"ready_for_prompt",
	"running",
	"finished",
	"failed",
}

var workerStatusByName map[string]WorkerStatus

func init() {
	workerStatusByName = make(map[string]WorkerStatus, len(workerStatusNames))
	for i, name := range workerStatusNames {
		workerStatusByName[name] = WorkerStatus(i)
	}
}

func (s WorkerStatus) String() string {
	if int(s) < len(workerStatusNames) {
		return workerStatusNames[s]
	}
	return fmt.Sprintf("WorkerStatus(%d)", int(s))
}

func (s WorkerStatus) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.String())
}

func (s *WorkerStatus) UnmarshalJSON(data []byte) error {
	var name string
	if err := json.Unmarshal(data, &name); err != nil {
		return err
	}
	v, ok := workerStatusByName[name]
	if !ok {
		return fmt.Errorf("unknown WorkerStatus %q", name)
	}
	*s = v
	return nil
}

// ---------------------------------------------------------------------------
// WorkerFailureKind
// ---------------------------------------------------------------------------

type WorkerFailureKind int

const (
	FailureTrustGate WorkerFailureKind = iota
	FailurePromptDelivery
	FailureProtocol
	FailureProvider
)

var failureKindNames = [...]string{
	"trust_gate",
	"prompt_delivery",
	"protocol",
	"provider",
}

var failureKindByName map[string]WorkerFailureKind

func init() {
	failureKindByName = make(map[string]WorkerFailureKind, len(failureKindNames))
	for i, name := range failureKindNames {
		failureKindByName[name] = WorkerFailureKind(i)
	}
}

func (k WorkerFailureKind) String() string {
	if int(k) < len(failureKindNames) {
		return failureKindNames[k]
	}
	return fmt.Sprintf("WorkerFailureKind(%d)", int(k))
}

func (k WorkerFailureKind) MarshalJSON() ([]byte, error) {
	return json.Marshal(k.String())
}

func (k *WorkerFailureKind) UnmarshalJSON(data []byte) error {
	var name string
	if err := json.Unmarshal(data, &name); err != nil {
		return err
	}
	v, ok := failureKindByName[name]
	if !ok {
		return fmt.Errorf("unknown WorkerFailureKind %q", name)
	}
	*k = v
	return nil
}

// ---------------------------------------------------------------------------
// WorkerEventKind
// ---------------------------------------------------------------------------

type WorkerEventKind int

const (
	EventSpawning WorkerEventKind = iota
	EventTrustRequired
	EventTrustResolved
	EventReadyForPrompt
	EventPromptMisdelivery
	EventPromptReplayArmed
	EventRunning
	EventRestarted
	EventFinished
	EventFailed
)

var eventKindNames = [...]string{
	"spawning",
	"trust_required",
	"trust_resolved",
	"ready_for_prompt",
	"prompt_misdelivery",
	"prompt_replay_armed",
	"running",
	"restarted",
	"finished",
	"failed",
}

var eventKindByName map[string]WorkerEventKind

func init() {
	eventKindByName = make(map[string]WorkerEventKind, len(eventKindNames))
	for i, name := range eventKindNames {
		eventKindByName[name] = WorkerEventKind(i)
	}
}

func (k WorkerEventKind) String() string {
	if int(k) < len(eventKindNames) {
		return eventKindNames[k]
	}
	return fmt.Sprintf("WorkerEventKind(%d)", int(k))
}

func (k WorkerEventKind) MarshalJSON() ([]byte, error) {
	return json.Marshal(k.String())
}

func (k *WorkerEventKind) UnmarshalJSON(data []byte) error {
	var name string
	if err := json.Unmarshal(data, &name); err != nil {
		return err
	}
	v, ok := eventKindByName[name]
	if !ok {
		return fmt.Errorf("unknown WorkerEventKind %q", name)
	}
	*k = v
	return nil
}

// ---------------------------------------------------------------------------
// WorkerTrustResolution
// ---------------------------------------------------------------------------

type WorkerTrustResolution int

const (
	TrustAutoAllowlisted WorkerTrustResolution = iota
	TrustManualApproval
)

var trustResolutionNames = [...]string{
	"auto_allowlisted",
	"manual_approval",
}

var trustResolutionByName map[string]WorkerTrustResolution

func init() {
	trustResolutionByName = make(map[string]WorkerTrustResolution, len(trustResolutionNames))
	for i, name := range trustResolutionNames {
		trustResolutionByName[name] = WorkerTrustResolution(i)
	}
}

func (r WorkerTrustResolution) String() string {
	if int(r) < len(trustResolutionNames) {
		return trustResolutionNames[r]
	}
	return fmt.Sprintf("WorkerTrustResolution(%d)", int(r))
}

func (r WorkerTrustResolution) MarshalJSON() ([]byte, error) {
	return json.Marshal(r.String())
}

func (r *WorkerTrustResolution) UnmarshalJSON(data []byte) error {
	var name string
	if err := json.Unmarshal(data, &name); err != nil {
		return err
	}
	v, ok := trustResolutionByName[name]
	if !ok {
		return fmt.Errorf("unknown WorkerTrustResolution %q", name)
	}
	*r = v
	return nil
}

// ---------------------------------------------------------------------------
// WorkerPromptTarget
// ---------------------------------------------------------------------------

type WorkerPromptTarget int

const (
	TargetShell WorkerPromptTarget = iota
	TargetWrongTarget
	TargetUnknown
)

var promptTargetNames = [...]string{
	"shell",
	"wrong_target",
	"unknown",
}

var promptTargetByName map[string]WorkerPromptTarget

func init() {
	promptTargetByName = make(map[string]WorkerPromptTarget, len(promptTargetNames))
	for i, name := range promptTargetNames {
		promptTargetByName[name] = WorkerPromptTarget(i)
	}
}

func (t WorkerPromptTarget) String() string {
	if int(t) < len(promptTargetNames) {
		return promptTargetNames[t]
	}
	return fmt.Sprintf("WorkerPromptTarget(%d)", int(t))
}

func (t WorkerPromptTarget) MarshalJSON() ([]byte, error) {
	return json.Marshal(t.String())
}

func (t *WorkerPromptTarget) UnmarshalJSON(data []byte) error {
	var name string
	if err := json.Unmarshal(data, &name); err != nil {
		return err
	}
	v, ok := promptTargetByName[name]
	if !ok {
		return fmt.Errorf("unknown WorkerPromptTarget %q", name)
	}
	*t = v
	return nil
}

// ---------------------------------------------------------------------------
// Structs
// ---------------------------------------------------------------------------

// WorkerFailure captures why a worker failed.
type WorkerFailure struct {
	Kind      WorkerFailureKind `json:"kind"`
	Message   string            `json:"message"`
	CreatedAt uint64            `json:"created_at"`
}

// WorkerEventPayload carries type-discriminated detail for certain events.
type WorkerEventPayload struct {
	Type           string                 `json:"type"` // "trust_prompt" or "prompt_delivery"
	Cwd            string                 `json:"cwd,omitempty"`
	Resolution     *WorkerTrustResolution `json:"resolution,omitempty"`
	PromptPreview  string                 `json:"prompt_preview,omitempty"`
	ObservedTarget *WorkerPromptTarget    `json:"observed_target,omitempty"`
	ObservedCwd    string                 `json:"observed_cwd,omitempty"`
	RecoveryArmed  bool                   `json:"recovery_armed,omitempty"`
}

// WorkerEvent is an immutable record in the worker's event log.
type WorkerEvent struct {
	Seq       uint64              `json:"seq"`
	Kind      WorkerEventKind     `json:"kind"`
	Status    WorkerStatus        `json:"status"`
	Detail    string              `json:"detail,omitempty"`
	Payload   *WorkerEventPayload `json:"payload,omitempty"`
	Timestamp uint64              `json:"timestamp"`
}

// Worker is the boot-phase state machine for a single subprocess worker.
type Worker struct {
	WorkerID                     string         `json:"worker_id"`
	Cwd                          string         `json:"cwd"`
	Status                       WorkerStatus   `json:"status"`
	TrustAutoResolve             bool           `json:"trust_auto_resolve"`
	TrustGateCleared             bool           `json:"trust_gate_cleared"`
	AutoRecoverPromptMisdelivery bool           `json:"auto_recover_prompt_misdelivery"`
	PromptDeliveryAttempts       uint32         `json:"prompt_delivery_attempts"`
	PromptInFlight               bool           `json:"prompt_in_flight"`
	LastPrompt                   *string        `json:"last_prompt,omitempty"`
	ReplayPrompt                 *string        `json:"replay_prompt,omitempty"`
	LastError                    *WorkerFailure `json:"last_error,omitempty"`
	CreatedAt                    uint64         `json:"created_at"`
	UpdatedAt                    uint64         `json:"updated_at"`
	Events                       []WorkerEvent  `json:"events"`

	// TrustResolver is an optional centralized trust resolver. When non-nil,
	// the worker delegates trust gate decisions to it instead of using its
	// own ad-hoc logic. The field is not serialized.
	TrustResolver *trust.TrustResolver `json:"-"`
}

// ResolveTrust evaluates screen text for trust prompts using the centralized
// resolver (if set). Returns the decision; callers should check IsRequired().
// When no resolver is configured, returns NotRequired (nil-safe fallback).
func (w *Worker) ResolveTrust(screenText string) trust.TrustDecision {
	if w.TrustResolver == nil {
		return trust.NotRequired()
	}
	return w.TrustResolver.Resolve(w.Cwd, screenText)
}

// WorkerReadySnapshot is a lightweight view of worker readiness.
type WorkerReadySnapshot struct {
	WorkerID          string         `json:"worker_id"`
	Status            WorkerStatus   `json:"status"`
	Ready             bool           `json:"ready"`
	Blocked           bool           `json:"blocked"`
	ReplayPromptReady bool           `json:"replay_prompt_ready"`
	LastError         *WorkerFailure `json:"last_error,omitempty"`
}

// ---------------------------------------------------------------------------
// Worker helpers
// ---------------------------------------------------------------------------

func (w *Worker) appendEvent(kind WorkerEventKind, detail string, payload *WorkerEventPayload) {
	now := nowSecs()
	w.Events = append(w.Events, WorkerEvent{
		Seq:       uint64(len(w.Events)) + 1,
		Kind:      kind,
		Status:    w.Status,
		Detail:    detail,
		Payload:   payload,
		Timestamp: now,
	})
	w.UpdatedAt = now
	w.emitStateFile()
}

// workerStateSnapshot is the JSON structure written to .claw/worker-state.json.
type workerStateSnapshot struct {
	WorkerID           string `json:"worker_id"`
	Status             string `json:"status"`
	IsReady            bool   `json:"is_ready"`
	TrustGateCleared   bool   `json:"trust_gate_cleared"`
	PromptInFlight     bool   `json:"prompt_in_flight"`
	LastEvent          string `json:"last_event"`
	UpdatedAt          uint64 `json:"updated_at"`
	SecondsSinceUpdate uint64 `json:"seconds_since_update"`
}

// emitStateFile writes a snapshot of the worker state to .claw/worker-state.json under the worker's cwd.
func (w *Worker) emitStateFile() {
	if w.Cwd == "" {
		return
	}

	lastEvent := ""
	if len(w.Events) > 0 {
		lastEvent = w.Events[len(w.Events)-1].Kind.String()
	}

	now := nowSecs()
	snap := workerStateSnapshot{
		WorkerID:           w.WorkerID,
		Status:             w.Status.String(),
		IsReady:            w.Status == StatusReadyForPrompt,
		TrustGateCleared:   w.TrustGateCleared,
		PromptInFlight:     w.PromptInFlight,
		LastEvent:          lastEvent,
		UpdatedAt:          w.UpdatedAt,
		SecondsSinceUpdate: now - w.UpdatedAt,
	}

	dir := filepath.Join(w.Cwd, ".claw")
	_ = os.MkdirAll(dir, 0o755)

	data, err := json.Marshal(snap)
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, "worker-state.json"), data, 0o644)
}

// clone returns a deep copy of the worker.
func (w *Worker) clone() *Worker {
	cp := *w
	cp.Events = make([]WorkerEvent, len(w.Events))
	copy(cp.Events, w.Events)
	// Deep copy payload pointers inside events.
	for i, ev := range w.Events {
		if ev.Payload != nil {
			p := *ev.Payload
			if ev.Payload.Resolution != nil {
				r := *ev.Payload.Resolution
				p.Resolution = &r
			}
			if ev.Payload.ObservedTarget != nil {
				t := *ev.Payload.ObservedTarget
				p.ObservedTarget = &t
			}
			cp.Events[i].Payload = &p
		}
	}
	if w.LastPrompt != nil {
		s := *w.LastPrompt
		cp.LastPrompt = &s
	}
	if w.ReplayPrompt != nil {
		s := *w.ReplayPrompt
		cp.ReplayPrompt = &s
	}
	if w.LastError != nil {
		e := *w.LastError
		cp.LastError = &e
	}
	return &cp
}

// ---------------------------------------------------------------------------
// Time helper
// ---------------------------------------------------------------------------

func nowSecs() uint64 {
	return uint64(time.Now().Unix())
}
