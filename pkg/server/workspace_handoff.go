package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

const (
	workspaceHandoffSchemaVersion = 1
	workspaceHandoffDirName       = "workspace-handoffs"
	workspaceHandoffLimit         = 128
	workspaceHandoffSummaryMax    = 20_000
	workspaceHandoffResultMax     = 4_000
)

var (
	workspaceHandoffCommit = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
	workspaceHandoffBranch = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,239}$`)
)

// workspaceHandoff is the host-owned correlation between two Studio chat
// runs. The opening ticket is only a bearer capability for the destination
// pane; its digest is persisted, while source/destination run provenance is
// checked against the run store before either endpoint can advance the record.
type workspaceHandoff struct {
	Version int    `json:"version"`
	ID      string `json:"id"`

	SourceID             string `json:"source_project_id"`
	SourceStoreDir       string `json:"source_store_dir"`
	SourceRunID          string `json:"source_run_id,omitempty"`
	SourceClientID       string `json:"source_client_id,omitempty"`
	SourceConversationID string `json:"source_conversation_id,omitempty"`

	DestinationID             string `json:"destination_project_id"`
	DestinationGeneration     string `json:"destination_generation"`
	DestinationClientID       string `json:"destination_client_id,omitempty"`
	DestinationConversationID string `json:"destination_conversation_id,omitempty"`
	DestinationRunID          string `json:"destination_run_id,omitempty"`

	Summary        string    `json:"summary"`
	TicketHash     string    `json:"ticket_hash"`
	TicketConsumed bool      `json:"ticket_consumed"`
	ExpiresAt      time.Time `json:"expires_at"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`

	Outcome       *workspaceHandoffReceipt `json:"outcome,omitempty"`
	OutcomeDigest string                   `json:"outcome_digest,omitempty"`
	ReceiptID     string                   `json:"receipt_id,omitempty"`

	DeliveryAttempts   int       `json:"delivery_attempts,omitempty"`
	NextDeliveryAt     time.Time `json:"next_delivery_at,omitempty"`
	DeliveryGeneration string    `json:"delivery_generation,omitempty"`
	DeliveryStartedAt  time.Time `json:"delivery_started_at,omitempty"`
	DeliveredAt        time.Time `json:"delivered_at,omitempty"`
	LastDeliveryError  string    `json:"last_delivery_error,omitempty"`
}

type workspaceHandoffReceipt struct {
	Status       string   `json:"status"`
	Summary      string   `json:"summary"`
	ChangedFiles []string `json:"changed_files,omitempty"`
	CommitSHA    string   `json:"commit_sha,omitempty"`
	Branch       string   `json:"branch,omitempty"`
	PRURL        string   `json:"pr_url,omitempty"`
	NextStep     string   `json:"next_step,omitempty"`
}

func workspaceHandoffTicketHash(ticket string) string {
	digest := sha256.Sum256([]byte(ticket))
	return hex.EncodeToString(digest[:])
}

func (h *WorkspaceHost) handoffPath(record *workspaceHandoff) string {
	return filepath.Join(record.SourceStoreDir, workspaceHandoffDirName, record.ID+".json")
}

func (h *WorkspaceHost) persistHandoffLocked(record *workspaceHandoff) error {
	if record.SourceStoreDir == "" || record.ID == "" {
		return errors.New("workspace handoff has no durable source binding")
	}
	record.UpdatedAt = time.Now().UTC()
	body, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal workspace handoff: %w", err)
	}
	path := h.handoffPath(record)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create workspace handoff store: %w", err)
	}
	if err := store.WriteFileAtomic(path, append(body, '\n'), 0o600); err != nil {
		return fmt.Errorf("persist workspace handoff: %w", err)
	}
	return nil
}

// loadWorkspaceHandoffs deliberately derives paths from the registry's pinned
// project stores. Tests therefore inject temporary StoreDir values and never
// fall through to the user's shared project registry path.
func (h *WorkspaceHost) loadWorkspaceHandoffs() {
	now := time.Now()
	for _, project := range h.registry.RecentProjects {
		if project.StoreDir == "" {
			continue
		}
		dir := filepath.Join(project.StoreDir, workspaceHandoffDirName)
		entries, err := os.ReadDir(dir)
		if err != nil {
			if !os.IsNotExist(err) {
				h.logger.Warn("workspace: read handoffs for %s: %v", project.Name, err)
			}
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
				continue
			}
			body, readErr := os.ReadFile(filepath.Join(dir, entry.Name()))
			if readErr != nil {
				h.logger.Warn("workspace: read handoff %s: %v", entry.Name(), readErr)
				continue
			}
			var record workspaceHandoff
			if err := json.Unmarshal(body, &record); err != nil ||
				record.Version != workspaceHandoffSchemaVersion ||
				record.ID == "" || record.SourceID != project.ID ||
				record.SourceStoreDir != project.StoreDir {
				h.logger.Warn("workspace: ignore invalid handoff record %s", entry.Name())
				continue
			}
			copy := record
			h.handoffs[record.ID] = &copy
			if !record.TicketConsumed && now.Before(record.ExpiresAt) && record.TicketHash != "" {
				h.handoffTickets[record.TicketHash] = record.ID
			}
		}
	}
}

func decodeWorkspaceHandoffJSON(w http.ResponseWriter, r *http.Request, max int64, value any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, max))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request must contain exactly one JSON value")
	}
	return nil
}

func (h *WorkspaceHost) createHandoff(w http.ResponseWriter, r *http.Request, sourceID string) {
	if !workspaceSafeOrigin(r) {
		h.writeError(w, http.StatusForbidden, "cross-origin workspace mutation rejected")
		return
	}
	var req struct {
		Destination string `json:"destination_project"`
		Summary     string `json:"summary"`
		SourceRunID string `json:"source_run_id,omitempty"`
	}
	if err := decodeWorkspaceHandoffJSON(w, r, 64<<10, &req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	req.Destination = strings.TrimSpace(req.Destination)
	req.Summary = strings.TrimSpace(req.Summary)
	req.SourceRunID = strings.TrimSpace(req.SourceRunID)
	if req.Destination == "" || req.Summary == "" || len(req.Summary) > workspaceHandoffSummaryMax {
		h.writeError(w, http.StatusBadRequest, "destination_project and a bounded summary are required")
		return
	}

	h.mu.RLock()
	source := h.runtimes[sourceID]
	var destination *workspaceRuntime
	for id, candidate := range h.runtimes {
		if id == req.Destination || strings.EqualFold(candidate.project.Name, req.Destination) {
			if destination != nil && destination.project.ID != id {
				h.mu.RUnlock()
				h.writeError(w, http.StatusConflict, "destination project name is ambiguous; use its id")
				return
			}
			destination = candidate
		}
	}
	if destination == nil {
		h.mu.RUnlock()
		h.writeError(w, http.StatusNotFound, "destination project not found")
		return
	}
	if destination.project.ID == sourceID {
		h.mu.RUnlock()
		h.writeError(w, http.StatusBadRequest, "destination project must differ from the source")
		return
	}
	if destination.state != "ready" || destination.server == nil {
		h.mu.RUnlock()
		h.writeError(w, http.StatusServiceUnavailable, "destination project runtime is unavailable")
		return
	}
	destinationProject := destination.project
	destinationGeneration := destination.generation
	h.mu.RUnlock()

	var sourceClientID, sourceConversationID string
	if req.SourceRunID != "" {
		if source == nil || source.state != "ready" || source.server == nil || source.server.runs == nil {
			h.writeError(w, http.StatusServiceUnavailable, "source project runtime is unavailable")
			return
		}
		run, err := source.server.runs.LoadRunCtx(r.Context(), req.SourceRunID)
		if err != nil {
			h.writeError(w, http.StatusNotFound, "source assistant run not found")
			return
		}
		if run.Source == nil || run.Source.Kind != store.RunSourceKindStudioChat ||
			validateStudioChatSourceID("client_id", run.Source.ClientID) != nil ||
			validateStudioChatSourceID("conversation_id", run.Source.ConversationID) != nil {
			h.writeError(w, http.StatusConflict, "source run is not a bound Studio chat conversation")
			return
		}
		sourceClientID = run.Source.ClientID
		sourceConversationID = run.Source.ConversationID
	}

	now := time.Now().UTC()
	ticket := workspaceGeneration() + workspaceGeneration()
	if source == nil {
		h.writeError(w, http.StatusNotFound, "source project not found")
		return
	}
	record := &workspaceHandoff{
		Version: workspaceHandoffSchemaVersion, ID: workspaceGeneration(),
		SourceID: sourceID, SourceStoreDir: source.project.StoreDir,
		SourceRunID: req.SourceRunID, SourceClientID: sourceClientID,
		SourceConversationID: sourceConversationID,
		DestinationID:        destinationProject.ID, DestinationGeneration: destinationGeneration,
		Summary: req.Summary, TicketHash: workspaceHandoffTicketHash(ticket),
		ExpiresAt: now.Add(5 * time.Minute), CreatedAt: now, UpdatedAt: now,
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if req.SourceRunID != "" {
		unresolved := 0
		for _, existing := range h.handoffs {
			if existing.SourceID == sourceID && existing.SourceRunID != "" && existing.DeliveredAt.IsZero() {
				unresolved++
			}
		}
		if unresolved >= workspaceHandoffLimit {
			h.writeError(w, http.StatusTooManyRequests, "too many unresolved workspace handoffs")
			return
		}
	}
	if err := h.persistHandoffLocked(record); err != nil {
		h.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.handoffs[record.ID] = record
	h.handoffTickets[record.TicketHash] = record.ID
	h.writeJSON(w, map[string]string{
		"ticket": ticket, "handoff_id": record.ID,
		"destination_project_id":   destinationProject.ID,
		"destination_project_name": destinationProject.Name,
	})
}

func (h *WorkspaceHost) redeemHandoff(w http.ResponseWriter, r *http.Request, destinationID string) {
	if !workspaceSafeOrigin(r) {
		h.writeError(w, http.StatusForbidden, "cross-origin workspace mutation rejected")
		return
	}
	var req struct {
		Ticket                    string `json:"ticket"`
		DestinationClientID       string `json:"destination_client_id,omitempty"`
		DestinationConversationID string `json:"destination_conversation_id,omitempty"`
	}
	if err := decodeWorkspaceHandoffJSON(w, r, 16<<10, &req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	hash := workspaceHandoffTicketHash(strings.TrimSpace(req.Ticket))
	h.mu.Lock()
	defer h.mu.Unlock()
	id, ok := h.handoffTickets[hash]
	if !ok {
		h.writeError(w, http.StatusNotFound, "handoff ticket is unknown or already used")
		return
	}
	record := h.handoffs[id]
	destination := h.runtimes[destinationID]
	if record == nil {
		delete(h.handoffTickets, hash)
		h.writeError(w, http.StatusNotFound, "handoff ticket is unknown or already used")
		return
	}
	if time.Now().After(record.ExpiresAt) {
		delete(h.handoffTickets, hash)
		h.writeError(w, http.StatusGone, "handoff ticket expired")
		return
	}
	if record.DestinationID != destinationID || destination == nil ||
		record.DestinationGeneration != destination.generation {
		h.writeError(w, http.StatusConflict, "handoff destination generation changed")
		return
	}
	if record.SourceRunID != "" {
		if validateStudioChatSourceID("destination_client_id", req.DestinationClientID) != nil ||
			validateStudioChatSourceID("destination_conversation_id", req.DestinationConversationID) != nil {
			h.writeError(w, http.StatusBadRequest, "destination chat identity is required")
			return
		}
		record.DestinationClientID = req.DestinationClientID
		record.DestinationConversationID = req.DestinationConversationID
	}
	record.TicketConsumed = true
	if err := h.persistHandoffLocked(record); err != nil {
		record.TicketConsumed = false
		h.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	delete(h.handoffTickets, hash)
	h.writeJSON(w, map[string]string{
		"handoff_id": record.ID, "source_project_id": record.SourceID,
		"destination_project_id": record.DestinationID,
		"summary":                record.Summary, "bot_id": "copilot",
	})
}

func (h *WorkspaceHost) bindHandoff(w http.ResponseWriter, r *http.Request, destinationID string) {
	if !workspaceSafeOrigin(r) {
		h.writeError(w, http.StatusForbidden, "cross-origin workspace mutation rejected")
		return
	}
	var req struct {
		HandoffID        string `json:"handoff_id"`
		DestinationRunID string `json:"destination_run_id"`
	}
	if err := decodeWorkspaceHandoffJSON(w, r, 16<<10, &req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	record, run, status, message := h.validatedDestinationRun(r.Context(), destinationID, req.HandoffID, req.DestinationRunID)
	if status != 0 {
		h.writeError(w, status, message)
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	current := h.handoffs[record.ID]
	if current == nil || current.DestinationID != destinationID {
		h.writeError(w, http.StatusNotFound, "workspace handoff not found")
		return
	}
	if current.DestinationRunID != "" && current.DestinationRunID != run.ID {
		h.writeError(w, http.StatusConflict, "workspace handoff is bound to another destination run")
		return
	}
	current.DestinationRunID = run.ID
	if err := h.persistHandoffLocked(current); err != nil {
		h.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.writeJSON(w, map[string]any{"handoff_id": current.ID, "bound": true})
}

func (h *WorkspaceHost) validatedDestinationRun(ctx context.Context, destinationID, handoffID, runID string) (*workspaceHandoff, *store.Run, int, string) {
	h.mu.RLock()
	record := h.handoffs[strings.TrimSpace(handoffID)]
	runtime := h.runtimes[destinationID]
	var snapshot workspaceHandoff
	if record != nil {
		snapshot = *record
	}
	h.mu.RUnlock()
	if record == nil || snapshot.SourceRunID == "" || snapshot.DestinationID != destinationID || !snapshot.TicketConsumed {
		return nil, nil, http.StatusNotFound, "workspace handoff not found"
	}
	if runtime == nil || runtime.state != "ready" || runtime.server == nil || runtime.server.runs == nil {
		return nil, nil, http.StatusServiceUnavailable, "destination project runtime is unavailable"
	}
	run, err := runtime.server.runs.LoadRunCtx(ctx, strings.TrimSpace(runID))
	if err != nil {
		return nil, nil, http.StatusNotFound, "destination assistant run not found"
	}
	if run.Source == nil || run.Source.Kind != store.RunSourceKindStudioChat ||
		run.Source.ClientID != snapshot.DestinationClientID ||
		run.Source.ConversationID != snapshot.DestinationConversationID {
		return nil, nil, http.StatusConflict, "destination run does not own this workspace handoff"
	}
	return &snapshot, run, 0, ""
}

func (h *WorkspaceHost) completeHandoff(w http.ResponseWriter, r *http.Request, destinationID string) {
	if !workspaceSafeOrigin(r) {
		h.writeError(w, http.StatusForbidden, "cross-origin workspace mutation rejected")
		return
	}
	var req struct {
		HandoffID        string   `json:"handoff_id"`
		DestinationRunID string   `json:"destination_run_id"`
		Status           string   `json:"status"`
		Summary          string   `json:"summary"`
		ChangedFiles     []string `json:"changed_files,omitempty"`
		CommitSHA        string   `json:"commit_sha,omitempty"`
		Branch           string   `json:"branch,omitempty"`
		PRURL            string   `json:"pr_url,omitempty"`
		NextStep         string   `json:"next_step,omitempty"`
	}
	if err := decodeWorkspaceHandoffJSON(w, r, 32<<10, &req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	record, run, status, message := h.validatedDestinationRun(r.Context(), destinationID, req.HandoffID, req.DestinationRunID)
	if status != 0 {
		h.writeError(w, status, message)
		return
	}
	if record.DestinationRunID != "" && record.DestinationRunID != run.ID {
		h.writeError(w, http.StatusConflict, "workspace handoff is bound to another destination run")
		return
	}
	receipt := workspaceHandoffReceipt{
		Status: strings.TrimSpace(req.Status), Summary: strings.TrimSpace(req.Summary),
		ChangedFiles: req.ChangedFiles, CommitSHA: strings.TrimSpace(req.CommitSHA),
		Branch: strings.TrimSpace(req.Branch), PRURL: strings.TrimSpace(req.PRURL),
		NextStep: strings.TrimSpace(req.NextStep),
	}
	if err := validateWorkspaceHandoffReceipt(&receipt); err != nil {
		h.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	body, _ := json.Marshal(receipt)
	digestBytes := sha256.Sum256(body)
	digest := hex.EncodeToString(digestBytes[:])

	h.mu.Lock()
	current := h.handoffs[record.ID]
	if current == nil || current.DestinationID != destinationID {
		h.mu.Unlock()
		h.writeError(w, http.StatusNotFound, "workspace handoff not found")
		return
	}
	if current.DestinationRunID == "" {
		current.DestinationRunID = run.ID
	}
	if current.DestinationRunID != run.ID {
		h.mu.Unlock()
		h.writeError(w, http.StatusConflict, "workspace handoff is bound to another destination run")
		return
	}
	if current.Outcome != nil {
		if current.OutcomeDigest != digest {
			h.mu.Unlock()
			h.writeError(w, http.StatusConflict, "workspace handoff already has a different terminal outcome")
			return
		}
		response := map[string]any{
			"handoff_id": current.ID, "receipt_id": current.ReceiptID,
			"delivery_status": handoffDeliveryStatus(current), "idempotent": true,
		}
		h.mu.Unlock()
		h.writeJSON(w, response)
		return
	}
	now := time.Now().UTC()
	current.Outcome = &receipt
	current.OutcomeDigest = digest
	current.ReceiptID = workspaceGeneration()
	current.NextDeliveryAt = now
	current.LastDeliveryError = ""
	if err := h.persistHandoffLocked(current); err != nil {
		current.Outcome = nil
		current.OutcomeDigest = ""
		current.ReceiptID = ""
		h.mu.Unlock()
		h.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	response := map[string]any{
		"handoff_id": current.ID, "receipt_id": current.ReceiptID,
		"delivery_status": handoffDeliveryStatus(current), "idempotent": false,
	}
	h.mu.Unlock()
	h.wakeHandoffDelivery()
	h.writeJSON(w, response)
}

func validateWorkspaceHandoffReceipt(receipt *workspaceHandoffReceipt) error {
	if receipt.Status != "completed" && receipt.Status != "failed" {
		return errors.New("status must be completed or failed")
	}
	if receipt.Summary == "" || len(receipt.Summary) > workspaceHandoffResultMax || hasUnsafeReceiptText(receipt.Summary) {
		return errors.New("summary must be bounded text without control characters")
	}
	if len(receipt.NextStep) > 2_000 || hasUnsafeReceiptText(receipt.NextStep) {
		return errors.New("next_step must be bounded text without control characters")
	}
	if len(receipt.ChangedFiles) > 50 {
		return errors.New("changed_files must contain at most 50 paths")
	}
	seen := make(map[string]struct{}, len(receipt.ChangedFiles))
	for i, raw := range receipt.ChangedFiles {
		candidate := strings.TrimSpace(raw)
		if candidate == "" || len(candidate) > 240 || filepath.IsAbs(candidate) ||
			strings.Contains(candidate, `\`) || strings.ContainsRune(candidate, 0) ||
			candidate == "." || candidate == ".." || strings.HasPrefix(candidate, "../") ||
			strings.Contains(candidate, "/../") {
			return errors.New("changed_files must contain safe relative paths")
		}
		if _, duplicate := seen[candidate]; duplicate {
			return errors.New("changed_files must be unique")
		}
		seen[candidate] = struct{}{}
		receipt.ChangedFiles[i] = candidate
	}
	if receipt.CommitSHA != "" && !workspaceHandoffCommit.MatchString(receipt.CommitSHA) {
		return errors.New("commit_sha must be a full hexadecimal Git object id")
	}
	if receipt.Branch != "" && (!workspaceHandoffBranch.MatchString(receipt.Branch) ||
		strings.Contains(receipt.Branch, "..") || strings.Contains(receipt.Branch, "//")) {
		return errors.New("branch is invalid")
	}
	if receipt.PRURL != "" {
		if len(receipt.PRURL) > 2_048 {
			return errors.New("pr_url is too long")
		}
		u, err := url.Parse(receipt.PRURL)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" {
			return errors.New("pr_url must be a plain HTTPS pull-request URL")
		}
		host := strings.ToLower(u.Hostname())
		route := strings.ToLower(u.Path)
		knownHost := host == "github.com" || host == "gitlab.com" || host == "codeberg.org"
		knownRoute := strings.Contains(route, "/pull/") || strings.Contains(route, "/pulls/") || strings.Contains(route, "/merge_requests/")
		if !knownHost || !knownRoute {
			return errors.New("pr_url host or route is not recognized")
		}
	}
	return nil
}

func hasUnsafeReceiptText(value string) bool {
	for _, r := range value {
		if r == 0 || (r < 0x20 && r != '\n' && r != '\t') {
			return true
		}
	}
	return false
}

func handoffDeliveryStatus(record *workspaceHandoff) string {
	if !record.DeliveredAt.IsZero() {
		return "delivered"
	}
	if !record.DeliveryStartedAt.IsZero() {
		return "accepted"
	}
	return "pending"
}

func (h *WorkspaceHost) startHandoffDelivery() {
	ctx, cancel := context.WithCancel(context.Background())
	h.handoffCancel = cancel
	h.handoffWG.Add(1)
	go func() {
		defer h.handoffWG.Done()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			h.deliverPendingHandoffs(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			case <-h.handoffWake:
			}
		}
	}()
}

func (h *WorkspaceHost) wakeHandoffDelivery() {
	select {
	case h.handoffWake <- struct{}{}:
	default:
	}
}

func (h *WorkspaceHost) deliverPendingHandoffs(ctx context.Context) {
	now := time.Now()
	h.mu.RLock()
	ids := make([]string, 0)
	for id, record := range h.handoffs {
		if record.Outcome != nil && record.DeliveredAt.IsZero() &&
			(record.NextDeliveryAt.IsZero() || !record.NextDeliveryAt.After(now)) {
			ids = append(ids, id)
		}
	}
	h.mu.RUnlock()
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		h.deliverHandoff(ctx, id)
	}
}

func (h *WorkspaceHost) deliverHandoff(ctx context.Context, id string) {
	h.mu.RLock()
	record := h.handoffs[id]
	var snapshot workspaceHandoff
	if record != nil {
		snapshot = *record
	}
	source := h.runtimes[snapshot.SourceID]
	h.mu.RUnlock()
	if record == nil || snapshot.Outcome == nil || !snapshot.DeliveredAt.IsZero() {
		return
	}
	if source == nil || source.state != "ready" || source.server == nil || source.server.runs == nil {
		h.recordHandoffDeliveryFailure(id, "source project runtime is unavailable")
		return
	}
	recorded, err := source.server.workspaceHandoffReceiptRecorded(ctx, snapshot.SourceRunID, snapshot.ReceiptID)
	if err != nil {
		h.recordHandoffDeliveryFailure(id, "could not inspect source receipt")
		return
	}
	if recorded {
		h.mu.Lock()
		if current := h.handoffs[id]; current != nil && current.DeliveredAt.IsZero() {
			previousDeliveredAt := current.DeliveredAt
			previousError := current.LastDeliveryError
			current.DeliveredAt = time.Now().UTC()
			current.LastDeliveryError = ""
			if persistErr := h.persistHandoffLocked(current); persistErr != nil {
				current.DeliveredAt = previousDeliveredAt
				current.LastDeliveryError = previousError
				current.NextDeliveryAt = time.Now().UTC().Add(time.Second)
				h.logger.Warn("workspace: persist delivered handoff %s: %v", id, persistErr)
			}
		}
		h.mu.Unlock()
		return
	}
	// A successful Resume is asynchronous. While this exact runtime generation
	// stays alive, wait for its durable human_answers_recorded event instead of
	// starting the same host input twice. After restart the generation changes,
	// so a missing event is safely retried against the reconciled run.
	if snapshot.DeliveryGeneration == source.generation && !snapshot.DeliveryStartedAt.IsZero() {
		h.scheduleHandoffInspection(id, time.Second)
		return
	}
	event := map[string]any{
		"receipt_id":             snapshot.ReceiptID,
		"destination_project_id": snapshot.DestinationID,
		"status":                 snapshot.Outcome.Status,
		"summary":                snapshot.Outcome.Summary,
		"changed_files":          snapshot.Outcome.ChangedFiles,
		"commit_sha":             snapshot.Outcome.CommitSHA,
		"branch":                 snapshot.Outcome.Branch,
		"pr_url":                 snapshot.Outcome.PRURL,
		"next_step":              snapshot.Outcome.NextStep,
	}
	if err := source.server.deliverHostEvent(ctx, snapshot.SourceRunID, "workspace-handoff-completed", event); err != nil {
		h.recordHandoffDeliveryFailure(id, err.Error())
		return
	}
	h.mu.Lock()
	if current := h.handoffs[id]; current != nil && current.DeliveredAt.IsZero() {
		current.DeliveryGeneration = source.generation
		current.DeliveryStartedAt = time.Now().UTC()
		current.NextDeliveryAt = time.Now().UTC().Add(time.Second)
		current.LastDeliveryError = ""
		_ = h.persistHandoffLocked(current)
	}
	h.mu.Unlock()
}

func (h *WorkspaceHost) scheduleHandoffInspection(id string, delay time.Duration) {
	h.mu.Lock()
	if record := h.handoffs[id]; record != nil && record.DeliveredAt.IsZero() {
		record.NextDeliveryAt = time.Now().UTC().Add(delay)
	}
	h.mu.Unlock()
}

func (h *WorkspaceHost) recordHandoffDeliveryFailure(id, message string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	record := h.handoffs[id]
	if record == nil || !record.DeliveredAt.IsZero() {
		return
	}
	record.DeliveryAttempts++
	delay := time.Second
	switch {
	case record.DeliveryAttempts >= 5:
		delay = 30 * time.Second
	case record.DeliveryAttempts == 4:
		delay = 10 * time.Second
	case record.DeliveryAttempts == 3:
		delay = 5 * time.Second
	case record.DeliveryAttempts == 2:
		delay = 2 * time.Second
	}
	record.NextDeliveryAt = time.Now().UTC().Add(delay)
	record.LastDeliveryError = strings.TrimSpace(message)
	if len(record.LastDeliveryError) > 500 {
		record.LastDeliveryError = record.LastDeliveryError[:500]
	}
	_ = h.persistHandoffLocked(record)
}
