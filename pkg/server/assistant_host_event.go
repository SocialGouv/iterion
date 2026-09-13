package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The assistant's chat gate has two doors: the operator's text, and a
// host-attested value on the node's host_event_field. Until now only ONE
// producer used the second door — the run-watch coordinator, waking the
// assistant when a watched run reached a terminal state.
//
// That left a conversational bot unable to CHAIN. It proposes a rewind, the
// operator confirms, the rewind runs — and the assistant is never told,
// because a rewind leaves its target parked and parking is not an outcome.
// It has no turn, so it waits for the operator to speak. A repair that takes
// five steps gets woken at one of them, and the bot that looked capable of
// supervising a run turns out to be capable of supervising a single event.
//
// This is the missing producer, not new machinery: the delivery path, the
// safety checks and the answer field are the coordinator's, reused verbatim.

type hostEventRequest struct {
	// Kind names the event family for the bot ("action-completed"). It is
	// echoed into the payload so a bot can branch without parsing prose.
	Kind string `json:"kind"`
	// Event is the opaque payload. The host stamps authority fields over it;
	// a caller cannot forge operator authorisation through this door.
	Event map[string]any `json:"event"`
}

var (
	errAssistantHostRunNotFound = errors.New("assistant host-event run not found")
	errAssistantHostBoundary    = errors.New("assistant is not at a chat boundary")
	errAssistantHostResumable   = errors.New("assistant is no longer resumable")
)

// handleDeliverHostEvent wakes a parked conversational run with a
// host-attested event. It is the studio's way of telling an assistant that
// something it ASKED FOR has happened — the completion signal that lets a
// multi-step repair continue without the operator having to prompt each step.
func (s *Server) handleDeliverHostEvent(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	var req hostEventRequest
	if !decodeJSONCapped(s, w, r, &req, 64<<10) {
		return
	}
	if strings.TrimSpace(req.Kind) == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "kind is required")
		return
	}
	err := s.deliverHostEvent(r.Context(), r.PathValue("id"), req.Kind, req.Event)
	if err != nil {
		switch {
		case errors.Is(err, errAssistantHostRunNotFound):
			s.httpErrorFor(w, r, http.StatusNotFound, "run not found")
		case errors.Is(err, errAssistantHostBoundary):
			s.httpErrorFor(w, r, http.StatusConflict, "assistant is not at a chat boundary")
		case errors.Is(err, errAssistantHostResumable):
			s.httpErrorFor(w, r, http.StatusConflict, "%v", err)
		default:
			s.httpErrorFor(w, r, http.StatusInternalServerError, "deliver host event: %v", err)
		}
		return
	}
	runID := r.PathValue("id")
	s.writeJSONFor(w, r, map[string]any{"run_id": runID, "delivered": true, "kind": req.Kind, "status": string(store.RunStatusRunning)})
}

// deliverHostEvent is shared by the scoped HTTP endpoint and host-owned
// background producers such as cross-project handoff receipts.
func (s *Server) deliverHostEvent(ctx context.Context, runID, kind string, event map[string]any) error {
	return s.deliverHostEventWithService(ctx, s.runs, runID, kind, event, true, "")
}

// deliverHostEventWithService captures the project run service and exposes a
// strict no-force mode for durable missions. receiptID is stamped by the host
// and consumed by reconciliation; callers cannot smuggle it through event.
func (s *Server) deliverHostEventWithService(ctx context.Context, runs *runview.Service, runID, kind string, event map[string]any, force bool, receiptID string) error {
	run, err := runs.LoadRunCtx(ctx, runID)
	if err != nil {
		return fmt.Errorf("%w: %v", errAssistantHostRunNotFound, err)
	}

	// The SAME boundary check the watch coordinator applies. Most importantly
	// it refuses an assistant mid-turn: its checkpoint is then the agent node,
	// not the chat gate, and delivering here would answer a pending ask_user
	// with machine input.
	nodeID, field, err := s.safeAssistantWatchPause(ctx, run)
	if err != nil || run.Checkpoint == nil || run.Checkpoint.NodeID != nodeID {
		return errAssistantHostBoundary
	}

	payload := map[string]any{}
	for k, v := range event {
		payload[k] = v
	}
	// Stamped LAST so a caller cannot claim operator authorisation by putting
	// these keys in the body. Mirrors the watch coordinator's envelope: an
	// automatic input must never read as something the operator sanctioned.
	payload["kind"] = strings.TrimSpace(kind)
	payload["authority"] = "iterion-host"
	payload["operator_authorized"] = false
	if receiptID != "" {
		payload["receipt_id"] = receiptID
	}

	// Detach the resumed conversational run from an HTTP request or scheduler
	// iteration. The run service owns the next turn after accepting the event.
	resumeCtx := context.WithoutCancel(ctx)
	hostInputs, err := s.assistantChatHostInputsWithService(resumeCtx, runs, run)
	if err != nil {
		return fmt.Errorf("project assistant chat history: %w", err)
	}
	if _, err := runs.Resume(resumeCtx, runview.ResumeSpec{
		RunID: run.ID, FilePath: run.FilePath,
		Answers: map[string]any{field: payload}, HostInputs: hostInputs,
		// A host receipt is allowed to adopt a redeployed Copi source only
		// after the safe chat-boundary admission above.
		Force: force, ExpectedStatus: run.Status, ReceiptID: receiptID,
	}); err != nil {
		if errors.Is(err, runview.ErrRunNotResumable) {
			return fmt.Errorf("%w: %v", errAssistantHostResumable, err)
		}
		return err
	}
	return nil
}

// workspaceHandoffReceiptRecorded reconciles asynchronous Resume calls with
// the durable event log. It intentionally matches the host-stamped kind and
// host-owned receipt id instead of assistant prose.
func (s *Server) workspaceHandoffReceiptRecorded(ctx context.Context, runID, receiptID string) (bool, error) {
	return s.hostEventReceiptRecorded(ctx, s.runs, runID, "workspace-handoff-completed", receiptID)
}

func (s *Server) hostEventReceiptRecorded(ctx context.Context, runs *runview.Service, runID, expectedKind, receiptID string) (bool, error) {
	recorded := false
	err := runs.ScanEventsCtx(ctx, runID, func(event *store.Event) bool {
		if event == nil || event.Type != store.EventHumanAnswersRecorded {
			return true
		}
		answers, _ := event.Data["answers"].(map[string]any)
		if answers == nil {
			body, marshalErr := json.Marshal(event.Data["answers"])
			if marshalErr != nil || json.Unmarshal(body, &answers) != nil {
				return true
			}
		}
		for _, raw := range answers {
			hostEvent, _ := raw.(map[string]any)
			if hostEvent == nil {
				body, marshalErr := json.Marshal(raw)
				if marshalErr != nil || json.Unmarshal(body, &hostEvent) != nil {
					continue
				}
			}
			kind, _ := hostEvent["kind"].(string)
			candidate, _ := hostEvent["receipt_id"].(string)
			authority, _ := hostEvent["authority"].(string)
			operatorAuthorized, operatorValuePresent := hostEvent["operator_authorized"].(bool)
			if kind == expectedKind && candidate == receiptID &&
				authority == "iterion-host" && operatorValuePresent && !operatorAuthorized {
				recorded = true
				return false
			}
		}
		return true
	})
	return recorded, err
}
