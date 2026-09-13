package server

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

const (
	assistantContextMaxReferences = 9
	assistantContextMaxReference  = 200
	assistantContextMaxMessage    = 32 << 10
	assistantContextMaxTitle      = 240
	assistantContextMaxError      = 1200
)

var assistantContextID = regexp.MustCompile(`^[A-Za-z0-9_:-]{1,128}$`)

var assistantContextMention = regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9_:/-])#?(native:[A-Za-z0-9_-]{1,64})(?:$|[^A-Za-z0-9_-])`)

type assistantContextResolveRequest struct {
	References []string `json:"references"`
	Message    string   `json:"message,omitempty"`
}

type assistantContextResolveResponse struct {
	References []assistantResolvedReference `json:"references"`
}

type assistantResolvedReference struct {
	Reference string                 `json:"reference"`
	Resolved  bool                   `json:"resolved"`
	Kind      string                 `json:"kind,omitempty"`
	Reason    string                 `json:"reason,omitempty"`
	Task      *assistantResolvedTask `json:"task,omitempty"`
	Run       *assistantResolvedRun  `json:"run,omitempty"`
}

type assistantResolvedTask struct {
	ID            string `json:"id"`
	Title         string `json:"title,omitempty"`
	State         string `json:"state,omitempty"`
	LastRunID     string `json:"last_run_id,omitempty"`
	AwaitingInput bool   `json:"awaiting_input,omitempty"`
}

type assistantResolvedRun struct {
	ID           string                  `json:"id"`
	Status       store.RunStatus         `json:"status"`
	WorkflowName string                  `json:"workflow_name,omitempty"`
	Resumable    bool                    `json:"resumable,omitempty"`
	Rewindable   bool                    `json:"rewindable,omitempty"`
	FailingNode  string                  `json:"failing_node,omitempty"`
	ErrorCode    string                  `json:"error_code,omitempty"`
	Error        string                  `json:"error,omitempty"`
	Repair       *assistantRepairContext `json:"repair,omitempty"`
}

type assistantRepairContext struct {
	Repairable    bool   `json:"repairable"`
	Reason        string `json:"reason,omitempty"`
	SourceProject string `json:"source_project_id,omitempty"`
	SourceRef     string `json:"source_ref,omitempty"`
	Package       string `json:"package,omitempty"`
	WorkflowPath  string `json:"workflow_path,omitempty"`
}

// handleAssistantContextResolve turns the studio's typed pointers into a
// small, host-attested snapshot. The browser never learns or reconstructs a
// filesystem store path: local and cloud requests use the RunStore and board
// store that the server already selected for this request/tenant.
func (s *Server) handleAssistantContextResolve(w http.ResponseWriter, r *http.Request) {
	var req assistantContextResolveRequest
	if !decodeJSONCapped(s, w, r, &req, 40<<10) {
		return
	}
	if len(req.References) > assistantContextMaxReferences {
		s.httpErrorFor(w, r, http.StatusBadRequest, "assistant context: at most %d references", assistantContextMaxReferences)
		return
	}
	if len(req.Message) > assistantContextMaxMessage {
		req.Message = req.Message[:assistantContextMaxMessage]
	}
	references := assistantContextReferences(req.References, req.Message)

	boardStore, err := s.resolvePipelineBoardStore(r)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "assistant context: board unavailable")
		return
	}
	s.stateMu.RLock()
	runs := s.runs
	s.stateMu.RUnlock()
	var runStore store.RunStore
	if runs != nil {
		runStore = runs.RunStore()
	}

	out, err := resolveAssistantReferences(r.Context(), references, runStore, boardStore)
	if err != nil {
		// Store implementations may wrap failures with filesystem paths or
		// connection details. This endpoint feeds model context, so its public
		// error must not become a side channel for either.
		s.httpErrorFor(w, r, http.StatusInternalServerError, "assistant context: resolution unavailable")
		return
	}
	s.writeJSONFor(w, r, assistantContextResolveResponse{References: out})
}

// assistantContextReferences adds task identifiers explicitly mentioned in
// the operator's prose to the visible/attached pointers supplied by the
// Studio. Pipelines commonly displays task ids as `native:...`, and operators
// naturally call those "runs" in a question. The host must turn that mention
// into a card pointer before the model sees it; otherwise runs.read receives a
// task id and can only report "run not found". Resolution remains bounded and
// store-owned below -- this parser never invents a run id or a store path.
func assistantContextReferences(explicit []string, message string) []string {
	out := make([]string, 0, assistantContextMaxReferences)
	seen := make(map[string]struct{}, assistantContextMaxReferences)
	add := func(ref string) {
		ref = strings.TrimSpace(ref)
		if ref == "" || len(out) >= assistantContextMaxReferences {
			return
		}
		if _, ok := seen[ref]; ok {
			return
		}
		seen[ref] = struct{}{}
		out = append(out, ref)
	}
	for _, ref := range explicit {
		add(ref)
	}
	for _, match := range assistantContextMention.FindAllStringSubmatch(message, assistantContextMaxReferences) {
		if len(match) > 1 {
			add("card/" + strings.ToLower(match[1]))
		}
	}
	return out
}

func resolveAssistantReferences(ctx context.Context, refs []string, runStore store.RunStore, boardStore native.BoardStore) ([]assistantResolvedReference, error) {
	out := make([]assistantResolvedReference, 0, len(refs))
	for _, ref := range refs {
		resolved, err := resolveAssistantReference(ctx, strings.TrimSpace(ref), runStore, boardStore)
		if err != nil {
			return nil, err
		}
		out = append(out, resolved)
	}
	return out, nil
}

func resolveAssistantReference(ctx context.Context, ref string, runStore store.RunStore, boardStore native.BoardStore) (assistantResolvedReference, error) {
	out := assistantResolvedReference{Reference: truncateRunes(ref, assistantContextMaxReference)}
	if ref == "" || len(ref) > assistantContextMaxReference {
		out.Reason = "invalid_reference"
		return out, nil
	}
	kind, id, ok := strings.Cut(ref, "/")
	if !ok {
		out.Reason = "invalid_reference"
		return out, nil
	}
	out.Kind = kind
	if kind == "node" {
		runID, nodeID, nodeOK := strings.Cut(id, "/")
		if !nodeOK || !assistantContextID.MatchString(runID) || !assistantContextID.MatchString(nodeID) {
			out.Reason = "invalid_reference"
			return out, nil
		}
		return resolveAssistantRun(ctx, out, runID, runStore)
	}
	if !assistantContextID.MatchString(id) {
		out.Reason = "invalid_reference"
		return out, nil
	}

	switch kind {
	case "run":
		return resolveAssistantRun(ctx, out, id, runStore)
	case "card":
		return resolveAssistantCard(ctx, out, id, runStore, boardStore)
	default:
		out.Reason = "unsupported_reference"
		return out, nil
	}
}

func resolveAssistantCard(ctx context.Context, out assistantResolvedReference, id string, runStore store.RunStore, boardStore native.BoardStore) (assistantResolvedReference, error) {
	if boardStore == nil {
		out.Reason = "board_unavailable"
		return out, nil
	}
	issueID := strings.TrimPrefix(id, "task:")
	issue, err := boardStore.Get(issueID)
	if err != nil {
		if errors.Is(err, tracker.ErrNotFound) {
			out.Reason = "not_found"
			return out, nil
		}
		return out, err
	}
	out.Resolved = true
	out.Task = &assistantResolvedTask{
		ID:            issue.ID,
		Title:         truncateRunes(issue.Title, assistantContextMaxTitle),
		State:         issue.State,
		LastRunID:     issue.LastRunID,
		AwaitingInput: issue.AwaitingInput,
	}
	if issue.LastRunID == "" || runStore == nil {
		return out, nil
	}
	run, err := loadAssistantRun(ctx, issue.LastRunID, runStore)
	if err != nil {
		if errors.Is(err, store.ErrRunNotFound) || errors.Is(err, store.ErrRunDeleted) {
			return out, nil
		}
		return out, err
	}
	out.Run = run
	return out, nil
}

func resolveAssistantRun(ctx context.Context, out assistantResolvedReference, id string, runStore store.RunStore) (assistantResolvedReference, error) {
	if runStore == nil {
		out.Reason = "runs_unavailable"
		return out, nil
	}
	run, err := loadAssistantRun(ctx, id, runStore)
	if err != nil {
		if errors.Is(err, store.ErrRunNotFound) || errors.Is(err, store.ErrRunDeleted) {
			out.Reason = "not_found"
			return out, nil
		}
		return out, err
	}
	out.Resolved = true
	out.Run = run
	return out, nil
}

func loadAssistantRun(ctx context.Context, id string, runStore store.RunStore) (*assistantResolvedRun, error) {
	run, err := runStore.LoadRun(ctx, id)
	if err != nil {
		return nil, err
	}
	resolved := &assistantResolvedRun{
		ID:           run.ID,
		Status:       run.Status,
		WorkflowName: truncateRunes(run.WorkflowName, assistantContextMaxTitle),
		Resumable:    run.Status == store.RunStatusFailedResumable || run.Status == store.RunStatusCancelled || run.Status == store.RunStatusPausedOperator || run.Status == store.RunStatusPausedWaitingHuman,
		Rewindable:   runview.IsRewindableRun(run),
		Error:        truncateRunes(run.Error, assistantContextMaxError),
	}
	if run.Status == store.RunStatusFailed || run.Status == store.RunStatusFailedResumable {
		resolved.Repair = &assistantRepairContext{Repairable: false, Reason: "bot provenance unavailable"}
		if origin, originErr := inferRunBotOrigin(run); originErr == nil {
			resolved.Repair = &assistantRepairContext{
				Repairable:    origin.RepoRoot != "" || (origin.RepoURL != "" && origin.Commit != "" && origin.ConnectionID != ""),
				SourceProject: origin.ProjectID, SourceRef: origin.Commit,
				Package: origin.Package, WorkflowPath: origin.WorkflowPath,
			}
			if !resolved.Repair.Repairable {
				resolved.Repair.Reason = "source repository is unavailable to this host"
			}
		} else {
			resolved.Repair.Reason = truncateRunes(originErr.Error(), 240)
		}
	}
	if run.Checkpoint != nil {
		resolved.FailingNode = truncateRunes(run.Checkpoint.NodeID, assistantContextMaxTitle)
	}
	// The run document owns status/error, while run_failed owns the precise
	// failing node and classification. Keep only the latest such event and do
	// not expose arbitrary tool/event payloads in the stamped prompt.
	if err := runStore.ScanEvents(ctx, id, func(evt *store.Event) bool {
		if evt == nil || evt.Type != store.EventRunFailed {
			return true
		}
		if evt.NodeID != "" {
			resolved.FailingNode = truncateRunes(evt.NodeID, assistantContextMaxTitle)
		}
		if code, ok := evt.Data["code"].(string); ok {
			resolved.ErrorCode = truncateRunes(code, 80)
		}
		if message, ok := evt.Data["error"].(string); ok && resolved.Error == "" {
			resolved.Error = truncateRunes(message, assistantContextMaxError)
		}
		return true
	}); err != nil {
		return nil, err
	}
	return resolved, nil
}

func truncateRunes(value string, limit int) string {
	if limit <= 0 || value == "" {
		return ""
	}
	if utf8.RuneCountInString(value) <= limit {
		return value
	}
	runes := []rune(value)
	return string(runes[:limit])
}
