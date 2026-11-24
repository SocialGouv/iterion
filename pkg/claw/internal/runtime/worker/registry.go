package worker

import (
	"errors"
	"fmt"
	"strings"
	"sync"
)

var (
	ErrWorkerNotFound = errors.New("worker not found")
	ErrInvalidState   = errors.New("invalid worker state for this operation")
)

// WorkerRegistry manages the lifecycle of all workers.
type WorkerRegistry struct {
	mu      sync.Mutex
	workers map[string]*Worker
	counter uint64
}

// NewWorkerRegistry creates an empty registry.
func NewWorkerRegistry() *WorkerRegistry {
	return &WorkerRegistry{
		workers: make(map[string]*Worker),
	}
}

// Create spawns a new worker and registers it.
func (r *WorkerRegistry) Create(cwd string, trustedRoots []string, autoRecoverPromptMisdelivery bool) *Worker {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.counter++
	now := nowSecs()
	id := fmt.Sprintf("worker_%08x_%d", now, r.counter)

	trustAuto := false
	for _, root := range trustedRoots {
		if pathMatchesAllowlist(cwd, root) {
			trustAuto = true
			break
		}
	}

	w := &Worker{
		WorkerID:                     id,
		Cwd:                          cwd,
		Status:                       StatusSpawning,
		TrustAutoResolve:             trustAuto,
		TrustGateCleared:             false,
		AutoRecoverPromptMisdelivery: autoRecoverPromptMisdelivery,
		PromptDeliveryAttempts:       0,
		PromptInFlight:               false,
		CreatedAt:                    now,
		UpdatedAt:                    now,
		Events:                       make([]WorkerEvent, 0, 8),
	}
	w.appendEvent(EventSpawning, "worker created", nil)

	r.workers[id] = w
	return w.clone()
}

// Get returns a defensive copy of the worker, or nil if not found.
func (r *WorkerRegistry) Get(workerID string) *Worker {
	r.mu.Lock()
	defer r.mu.Unlock()

	w, ok := r.workers[workerID]
	if !ok {
		return nil
	}
	return w.clone()
}

// Observe processes screen text through the state machine.
func (r *WorkerRegistry) Observe(workerID, screenText string) (*Worker, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	w, ok := r.workers[workerID]
	if !ok {
		return nil, ErrWorkerNotFound
	}

	lowered := strings.ToLower(screenText)

	// 1. Trust prompt detection.
	if !w.TrustGateCleared && detectTrustPrompt(lowered) {
		w.Status = StatusTrustRequired
		w.LastError = &WorkerFailure{
			Kind:      FailureTrustGate,
			Message:   "worker boot blocked on trust prompt",
			CreatedAt: nowSecs(),
		}
		w.appendEvent(EventTrustRequired, "trust prompt detected", &WorkerEventPayload{
			Type: "trust_prompt",
			Cwd:  w.Cwd,
		})

		if w.TrustAutoResolve {
			r.resolveTrustInternal(w, TrustAutoAllowlisted)
		}
		return w.clone(), nil
	}

	// 2. Prompt misdelivery detection (only when a prompt is in flight).
	if w.PromptInFlight {
		obs := detectPromptMisdelivery(screenText, lowered, w.LastPrompt, w.Cwd)
		if obs != nil {
			detail := promptMisdeliveryDetail(obs.Target)
			target := obs.Target
			payload := &WorkerEventPayload{
				Type:           "prompt_delivery",
				ObservedTarget: &target,
				RecoveryArmed:  w.AutoRecoverPromptMisdelivery,
			}
			if obs.ObservedCwd != nil {
				payload.ObservedCwd = *obs.ObservedCwd
			}
			if w.LastPrompt != nil {
				payload.PromptPreview = promptPreview(*w.LastPrompt)
			}

			w.PromptInFlight = false
			w.appendEvent(EventPromptMisdelivery, detail, payload)

			if w.AutoRecoverPromptMisdelivery && w.LastPrompt != nil {
				// Arm replay.
				prompt := *w.LastPrompt
				w.ReplayPrompt = &prompt
				w.Status = StatusReadyForPrompt
				w.appendEvent(EventPromptReplayArmed, "replay armed for misdelivered prompt", nil)
			} else {
				w.Status = StatusFailed
				w.LastError = &WorkerFailure{
					Kind:      FailurePromptDelivery,
					Message:   detail,
					CreatedAt: nowSecs(),
				}
				w.appendEvent(EventFailed, detail, nil)
			}
			return w.clone(), nil
		}
	}

	// 3. Running cue detection (only when prompt is in flight).
	if w.PromptInFlight && detectRunningCue(lowered) {
		w.Status = StatusRunning
		w.PromptInFlight = false
		w.appendEvent(EventRunning, "running cue detected", nil)
		return w.clone(), nil
	}

	// 4. Ready-for-prompt detection.
	if detectReadyForPrompt(screenText, lowered) {
		if w.Status != StatusReadyForPrompt {
			w.Status = StatusReadyForPrompt
			w.PromptInFlight = false
			// Clear trust gate error when becoming ready.
			if w.LastError != nil && w.LastError.Kind == FailureTrustGate {
				w.LastError = nil
			}
			w.appendEvent(EventReadyForPrompt, "ready for prompt", nil)
		}
		return w.clone(), nil
	}

	return w.clone(), nil
}

// ResolveTrust manually resolves the trust gate.
func (r *WorkerRegistry) ResolveTrust(workerID string) (*Worker, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	w, ok := r.workers[workerID]
	if !ok {
		return nil, ErrWorkerNotFound
	}

	if w.Status != StatusTrustRequired {
		return nil, fmt.Errorf("%w: expected trust_required, got %s", ErrInvalidState, w.Status)
	}

	r.resolveTrustInternal(w, TrustManualApproval)
	return w.clone(), nil
}

func (r *WorkerRegistry) resolveTrustInternal(w *Worker, resolution WorkerTrustResolution) {
	w.TrustGateCleared = true
	w.Status = StatusSpawning
	w.appendEvent(EventTrustResolved, fmt.Sprintf("trust resolved: %s", resolution), &WorkerEventPayload{
		Type:       "trust_prompt",
		Cwd:        w.Cwd,
		Resolution: &resolution,
	})
}

// SendPrompt delivers a prompt to the worker.
func (r *WorkerRegistry) SendPrompt(workerID string, prompt *string) (*Worker, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	w, ok := r.workers[workerID]
	if !ok {
		return nil, ErrWorkerNotFound
	}

	if w.Status != StatusReadyForPrompt {
		return nil, fmt.Errorf("%w: expected ready_for_prompt, got %s", ErrInvalidState, w.Status)
	}

	// Determine the effective prompt: explicit arg, replay, or error.
	var effectivePrompt string
	switch {
	case prompt != nil:
		effectivePrompt = *prompt
	case w.ReplayPrompt != nil:
		effectivePrompt = *w.ReplayPrompt
	default:
		return nil, fmt.Errorf("%w: no prompt provided and no replay prompt available", ErrInvalidState)
	}

	w.PromptDeliveryAttempts++
	w.PromptInFlight = true
	w.LastPrompt = &effectivePrompt
	w.ReplayPrompt = nil
	w.Status = StatusRunning
	w.appendEvent(EventRunning, fmt.Sprintf("prompt sent (attempt %d)", w.PromptDeliveryAttempts), &WorkerEventPayload{
		Type:          "prompt_delivery",
		PromptPreview: promptPreview(effectivePrompt),
	})

	return w.clone(), nil
}

// ReplayArmedPrompt sends the replay prompt if one is armed.
// Returns (worker, nil) on success, error if no replay prompt is available.
func (r *WorkerRegistry) ReplayArmedPrompt(workerID string) (*Worker, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	w, ok := r.workers[workerID]
	if !ok {
		return nil, ErrWorkerNotFound
	}

	if w.Status != StatusReadyForPrompt {
		return nil, fmt.Errorf("%w: expected ready_for_prompt, got %s", ErrInvalidState, w.Status)
	}
	if w.ReplayPrompt == nil {
		return nil, fmt.Errorf("%w: no replay prompt armed", ErrInvalidState)
	}

	effectivePrompt := *w.ReplayPrompt
	w.PromptDeliveryAttempts++
	w.PromptInFlight = true
	w.LastPrompt = &effectivePrompt
	w.ReplayPrompt = nil
	w.Status = StatusRunning
	w.appendEvent(EventRunning, fmt.Sprintf("replay prompt sent (attempt %d)", w.PromptDeliveryAttempts), &WorkerEventPayload{
		Type:          "prompt_delivery",
		PromptPreview: promptPreview(effectivePrompt),
		RecoveryArmed: false,
	})

	return w.clone(), nil
}

// AwaitReady returns a snapshot of the worker's readiness state.
func (r *WorkerRegistry) AwaitReady(workerID string) (*WorkerReadySnapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	w, ok := r.workers[workerID]
	if !ok {
		return nil, ErrWorkerNotFound
	}

	ready := w.Status == StatusReadyForPrompt
	blocked := w.Status == StatusTrustRequired || w.Status == StatusFailed
	replayReady := ready && w.ReplayPrompt != nil

	snap := &WorkerReadySnapshot{
		WorkerID:          w.WorkerID,
		Status:            w.Status,
		Ready:             ready,
		Blocked:           blocked,
		ReplayPromptReady: replayReady,
	}
	if w.LastError != nil {
		e := *w.LastError
		snap.LastError = &e
	}
	return snap, nil
}

// Restart resets a worker to spawning state.
func (r *WorkerRegistry) Restart(workerID string) (*Worker, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	w, ok := r.workers[workerID]
	if !ok {
		return nil, ErrWorkerNotFound
	}

	w.Status = StatusSpawning
	w.PromptInFlight = false
	w.LastError = nil
	w.ReplayPrompt = nil
	w.LastPrompt = nil
	w.TrustGateCleared = false
	w.PromptDeliveryAttempts = 0
	w.appendEvent(EventRestarted, "worker restarted", nil)

	return w.clone(), nil
}

// Terminate marks a worker as finished.
func (r *WorkerRegistry) Terminate(workerID string) (*Worker, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	w, ok := r.workers[workerID]
	if !ok {
		return nil, ErrWorkerNotFound
	}

	w.Status = StatusFinished
	w.PromptInFlight = false
	w.appendEvent(EventFinished, "worker terminated", nil)

	return w.clone(), nil
}

// IsDegraded returns true if the worker has been in its current state for longer
// than the given threshold (in seconds) without any new events.
func (r *WorkerRegistry) IsDegraded(workerID string, thresholdSecs uint64) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	w, ok := r.workers[workerID]
	if !ok {
		return false, ErrWorkerNotFound
	}

	// Only consider non-terminal states as potentially degraded.
	switch w.Status {
	case StatusFinished, StatusFailed:
		return false, nil
	}

	now := nowSecs()
	elapsed := now - w.UpdatedAt
	return elapsed >= thresholdSecs, nil
}

// ObserveCompletion processes a completion signal from the provider.
// Provider failure is detected when finishReason is "unknown" with zero output tokens,
// or when finishReason is "error".
func (r *WorkerRegistry) ObserveCompletion(workerID string, finishReason string, tokensOutput uint64) (*Worker, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	w, ok := r.workers[workerID]
	if !ok {
		return nil, ErrWorkerNotFound
	}

	w.PromptInFlight = false

	providerFailure := finishReason == "error" || (finishReason == "unknown" && tokensOutput == 0)
	if providerFailure {
		w.Status = StatusFailed
		msg := fmt.Sprintf("provider failure: finish_reason=%s, tokens_output=%d", finishReason, tokensOutput)
		w.LastError = &WorkerFailure{
			Kind:      FailureProvider,
			Message:   msg,
			CreatedAt: nowSecs(),
		}
		w.appendEvent(EventFailed, msg, nil)
	} else {
		w.Status = StatusFinished
		w.appendEvent(EventFinished, fmt.Sprintf("completed: finish_reason=%s", finishReason), nil)
	}

	return w.clone(), nil
}
