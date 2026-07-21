package server

import (
	"encoding/json"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"net/http"
	"sort"
	"strings"
)

// Planner provenance + hard-dependency enrichment for the pipeline board
// projection, and the ticket upsert path. Split out of pipeline_boards.go
// so the projection file stays about lanes and progress.

func isPipelineTerminalOrActive(state string) bool {
	switch state {
	case native.StateInProgress, native.StateAwaitingInput, native.StateReview,
		native.StateDone, native.StateBlocked:
		return true
	default:
		return false
	}
}

// pipelineIssueRole returns bot_args.role when set.
func pipelineIssueRole(iss *native.Issue) string {
	if iss == nil || iss.BotArgs == nil {
		return ""
	}
	return strings.TrimSpace(iss.BotArgs[native.BotArgRole])
}

// pipelineIssueParentID returns the planner provenance pointer for an issue.
func pipelineIssueParentID(iss *native.Issue) string {
	if iss == nil {
		return ""
	}
	if id := strings.TrimSpace(iss.ParentID); id != "" {
		return id
	}
	if iss.BotArgs != nil {
		return strings.TrimSpace(iss.BotArgs[native.BotArgSpawnedFrom])
	}
	return ""
}

// formatBlockerIDs is a short human list for error messages.
func formatBlockerIDs(open []native.BlockerInfo) string {
	if len(open) == 0 {
		return "[]"
	}
	parts := make([]string, 0, len(open))
	for _, b := range open {
		if b.Title != "" {
			parts = append(parts, fmt.Sprintf("%s (%s)", b.ID, b.Title))
		} else {
			parts = append(parts, b.ID)
		}
	}
	return strings.Join(parts, ", ")
}

// enrichParentChildLinks fills ParentIssueID / Children / ChildrenSummary /
// Role on every issue-backed card from the native issue graph.
func (b *pipelineProjectionBuilder) enrichParentChildLinks() {
	if len(b.allIssues) == 0 || len(b.cards) == 0 {
		return
	}
	issueByID := make(map[string]*native.Issue, len(b.allIssues))
	childrenByParent := map[string][]*native.Issue{}
	for _, iss := range b.allIssues {
		if iss == nil {
			continue
		}
		issueByID[iss.ID] = iss
		if pid := pipelineIssueParentID(iss); pid != "" {
			childrenByParent[pid] = append(childrenByParent[pid], iss)
		}
	}
	cardByIssue := make(map[string]int, len(b.cards))
	for i := range b.cards {
		if id := b.cards[i].IssueID; id != "" {
			cardByIssue[id] = i
		}
	}
	for i := range b.cards {
		c := &b.cards[i]
		if c.IssueID == "" {
			continue
		}
		iss := issueByID[c.IssueID]
		if iss == nil {
			continue
		}
		pid := pipelineIssueParentID(iss)
		c.ParentIssueID = pid
		if pid != "" {
			if p := issueByID[pid]; p != nil {
				c.ParentTitle = p.Title
			}
		}
		kids := childrenByParent[c.IssueID]
		if len(kids) == 0 {
			if c.Role == "" && pid != "" {
				c.Role = "producer"
			}
			continue
		}
		if c.Role == "" {
			c.Role = "planner"
		}
		// Stable order: priority desc, then created asc (same as board).
		sort.SliceStable(kids, func(a, b int) bool {
			if kids[a].Priority != kids[b].Priority {
				return kids[a].Priority > kids[b].Priority
			}
			return kids[a].CreatedAt.Before(kids[b].CreatedAt)
		})
		summary := &PipelineBoardChildrenSummary{Total: len(kids)}
		refs := make([]PipelineBoardChildRef, 0, len(kids))
		for _, k := range kids {
			ref := PipelineBoardChildRef{
				IssueID: k.ID,
				Title:   k.Title,
				State:   k.State,
				BotID:   k.Bot,
			}
			if j, ok := cardByIssue[k.ID]; ok {
				child := b.cards[j]
				ref.CardID = child.ID
				switch {
				case child.ColumnID == pipelineColumnInProgress:
					summary.InProgress++
				case child.ColumnID == pipelineColumnClosed && child.Failed:
					summary.Failed++
				case child.ColumnID == pipelineColumnClosed:
					summary.Done++
				case child.Ready:
					summary.Ready++
				default:
					summary.Open++
				}
			} else {
				// Child not on board (no bot) — count from issue state.
				switch k.State {
				case native.StateDone:
					summary.Done++
				case native.StateInProgress, native.StateAwaitingInput, native.StateReview:
					summary.InProgress++
				case native.StateBlocked:
					summary.Failed++
				case native.StateReady:
					summary.Ready++
				default:
					summary.Open++
				}
			}
			refs = append(refs, ref)
		}
		c.Children = refs
		c.ChildrenSummary = summary
	}
}

// attachDeps fills the hard-dependency projection fields on a card from
// its native issue. No-op when the card has no issue provenance.
func (b *pipelineProjectionBuilder) attachDeps(card *PipelineBoardCard, issue *native.Issue) {
	if card == nil || issue == nil || b.boardStore == nil {
		return
	}
	blockers := native.ResolveBlockersForIssue(b.boardStore, issue)
	card.Blockers = blockers
	open := 0
	for _, bl := range blockers {
		if !bl.Satisfied {
			open++
		}
	}
	card.OpenBlockerCount = open
	card.Blocking = native.ReverseBlockers(b.allIssues, issue.ID)
	// launch_blocked_reason is useful even for non-ready tickets (waiting_deps
	// / open blockers) so the UI can show a badge without re-deriving the rule.
	if reason := native.LaunchBlockedReason(b.boardStore, issue); reason != "" {
		// For non-ready tickets that simply aren't staged yet, suppress the
		// generic "not_ready" noise — only surface actionable gates.
		if reason == "not_ready" && issue.State != native.StateReady {
			if open > 0 {
				// Prefer blocker_labels when any open entry is label-gated.
				for _, bl := range blockers {
					if len(bl.MissingLabels) > 0 {
						card.LaunchBlockedReason = "blocker_labels"
						return
					}
				}
				card.LaunchBlockedReason = "open_blockers"
			} else if issue.State == native.StateWaitingDeps {
				card.LaunchBlockedReason = "waiting_deps"
			}
		} else {
			card.LaunchBlockedReason = reason
		}
	}
}

// upsertPipelineTask patches title/body/labels/priority/bot_args/blockers on an
// existing ticket. Does not move out of in_progress / done / awaiting_input;
// optional Start only stages ready/waiting_deps when the ticket is still
// pre-execution (backlog/inbox/waiting_deps/ready).
func (s *Server) upsertPipelineTask(
	boardStore native.BoardStore,
	board *native.Board,
	existing *native.Issue,
	req pipelineBoardTaskRequest,
	botName string,
	botArgs map[string]string,
	blockers []string,
) (*native.Issue, error) {
	if err := native.ValidateBlockers(boardStore, existing.ID, blockers); err != nil {
		return nil, err
	}
	title := strings.TrimSpace(req.Title)
	body := strings.TrimSpace(req.Body)
	patch := native.Patch{
		Title:    &title,
		Body:     &body,
		Priority: &req.Priority,
		Bot:      &botName,
		BotArgs:  &botArgs,
		Blockers: &blockers,
	}
	if req.Labels != nil {
		labels := append([]string(nil), req.Labels...)
		patch.Labels = &labels
	}
	iss, err := boardStore.Update(existing.ID, patch)
	if err != nil {
		return nil, err
	}
	// State: only nudge pre-run tickets. Never yank a live/finished run.
	if isPipelineTerminalOrActive(iss.State) {
		return iss, nil
	}
	policy := native.BlockerPolicy{RequireLabels: native.RequireBlockerLabels(botArgs)}
	ok, _ := native.BlockersSatisfiedPolicy(boardStore, blockers, policy)
	var target string
	if req.Start {
		if ok {
			target = native.StateReady
		} else if board != nil && board.StateByName(native.StateWaitingDeps) != nil {
			target = native.StateWaitingDeps
		}
	} else if !ok && board != nil && board.StateByName(native.StateWaitingDeps) != nil {
		// Keep / move to waiting_deps when deps still open.
		target = native.StateWaitingDeps
	}
	if target != "" && target != iss.State && board != nil && board.StateByName(target) != nil {
		return boardStore.SetState(iss.ID, target)
	}
	return iss, nil
}

// writeJSONError emits a STRUCTURED error body (not the plain-text
// httpErrorFor) so the studio can branch on a machine code — the
// open-blockers refusal ships the blocker list with it.
func (s *Server) writeJSONError(w http.ResponseWriter, r *http.Request, code int, body map[string]any) {
	s.reflectAllowedOrigin(w, r)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
