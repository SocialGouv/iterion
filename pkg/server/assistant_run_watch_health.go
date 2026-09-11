package server

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/clock"
	"github.com/SocialGouv/iterion/pkg/runwatch"
	"github.com/SocialGouv/iterion/pkg/store"
)

const (
	assistantWatchHealthEpisodePageSize = 50
	assistantWatchHealthWatchPageSize   = 50
)

// These are deliberately projections rather than the persisted Watch and
// Episode structs. In particular, LastError is not returned: it may contain
// a command, a path, or a provider response that is not useful in a health
// endpoint and would make the endpoint a second log sink.
type assistantWatchHealthResponse struct {
	RunID        string                          `json:"run_id"`
	TargetStatus store.RunStatus                 `json:"target_status"`
	Coordinator  assistantWatchCoordinatorHealth `json:"coordinator"`
	Watches      []assistantWatchHealthWatch     `json:"watches"`
	Attention    []string                        `json:"attention"`
	Truncated    bool                            `json:"truncated,omitempty"`
}

type assistantWatchCoordinatorHealth struct {
	Present              bool       `json:"present"`
	LastSweepStartedAt   *time.Time `json:"last_sweep_started_at,omitempty"`
	LastSweepCompletedAt *time.Time `json:"last_sweep_completed_at,omitempty"`
	LastSweepDurationMS  int64      `json:"last_sweep_duration_ms,omitempty"`
	Stale                bool       `json:"stale"`
}

type assistantWatchHealthWatch struct {
	WatchID                string                        `json:"watch_id"`
	TargetRunID            string                        `json:"target_run_id"`
	AssistantRunID         string                        `json:"assistant_run_id"`
	CoveredRunID           string                        `json:"covered_run_id,omitempty"`
	State                  runwatch.WatchState           `json:"state"`
	Active                 bool                          `json:"active"`
	DuplicateActiveWatch   bool                          `json:"duplicate_active_watch,omitempty"`
	LastObservedEventSeq   int64                         `json:"last_observed_event_seq"`
	LastDeliveredAt        *time.Time                    `json:"last_delivered_at,omitempty"`
	DeliveredEpisodes      int                           `json:"delivered_episodes"`
	AssistantStatus        store.RunStatus               `json:"assistant_status,omitempty"`
	AssistantChatAvailable bool                          `json:"assistant_chat_available"`
	Episodes               []assistantWatchHealthEpisode `json:"episodes"`
	EpisodesTruncated      bool                          `json:"episodes_truncated,omitempty"`
	Attention              []string                      `json:"attention"`
}

type assistantWatchHealthEpisode struct {
	EpisodeID       string                `json:"episode_id"`
	Kind            string                `json:"kind,omitempty"`
	State           runwatch.EpisodeState `json:"state"`
	Attempts        int                   `json:"attempts"`
	NextAttemptAt   time.Time             `json:"next_attempt_at"`
	LastErrorReason string                `json:"last_error_reason,omitempty"`
	Due             bool                  `json:"due"`
	Overdue         bool                  `json:"overdue"`
	CreatedAt       time.Time             `json:"created_at"`
	UpdatedAt       time.Time             `json:"updated_at"`
	DeliveredAt     *time.Time            `json:"delivered_at,omitempty"`
}

func (s *Server) handleAssistantWatchHealth(w http.ResponseWriter, r *http.Request) {
	run, err := s.runs.LoadRunCtx(r.Context(), r.PathValue("id"))
	if err != nil || run == nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "run not found")
		return
	}

	out := assistantWatchHealthResponse{
		RunID:        run.ID,
		TargetStatus: run.Status,
		Watches:      []assistantWatchHealthWatch{},
		Attention:    []string{},
	}
	if c := s.assistantWatch; c != nil {
		out.Coordinator = projectAssistantWatchHeartbeat(c.heartbeat())
	} else {
		// A route can be served while the coordinator is starting, disabled, or
		// deliberately absent in a test server. That is a stale health state,
		// never an implicit "healthy" answer.
		out.Coordinator = assistantWatchCoordinatorHealth{Present: false, Stale: true}
	}

	switch run.Status {
	case store.RunStatusFailed, store.RunStatusFailedResumable:
		out.Attention = append(out.Attention, "target_failed")
	case store.RunStatusCancelled:
		out.Attention = append(out.Attention, "target_cancelled")
	}

	var watches []assistantWatchResponse
	if s.assistantWatches != nil {
		watches, err = s.listCoveringAssistantWatches(r.Context(), run)
		if err != nil {
			s.httpErrorFor(w, r, http.StatusInternalServerError, "list watches: %v", err)
			return
		}
	}
	if len(watches) == 0 {
		out.Attention = append(out.Attention, "no_covering_watch")
	}
	if len(watches) > assistantWatchHealthWatchPageSize {
		watches = watches[:assistantWatchHealthWatchPageSize]
		out.Truncated = true
	}

	// listCoveringAssistantWatches already deduplicates row IDs. A second row
	// can still cover the same run through an ancestor and a descendant; make
	// that redundant coverage visible instead of silently calling it healthy.
	coverageCounts := map[string]int{}
	for _, watch := range watches {
		key := watch.OwnerID + "\x00" + watch.AssistantRunID + "\x00" + run.ID
		coverageCounts[key]++
	}
	now := clock.Default.Now().UTC()
	if c := s.assistantWatch; c != nil {
		now = c.now()
	}
	for _, watch := range watches {
		key := watch.OwnerID + "\x00" + watch.AssistantRunID + "\x00" + run.ID
		row := assistantWatchHealthWatch{
			WatchID: watch.ID, TargetRunID: watch.TargetRunID, AssistantRunID: watch.AssistantRunID,
			CoveredRunID: watch.CoveredRunID, State: watch.State, Active: watch.State == runwatch.WatchActive,
			DuplicateActiveWatch: coverageCounts[key] > 1 && watch.State == runwatch.WatchActive,
			LastObservedEventSeq: watch.LastObservedEventSeq, LastDeliveredAt: watch.LastDeliveredAt,
			DeliveredEpisodes: watch.DeliveredEpisodes, Episodes: []assistantWatchHealthEpisode{},
			Attention: []string{},
		}
		if row.DuplicateActiveWatch {
			row.Attention = append(row.Attention, "duplicate_active_watch")
			out.Attention = append(out.Attention, "duplicate_active_watch")
		}

		assistant, assistantErr := s.runs.LoadRunCtx(r.Context(), watch.AssistantRunID)
		if assistantErr != nil || assistant == nil {
			row.Attention = append(row.Attention, "assistant_unavailable")
			out.Attention = append(out.Attention, "assistant_unavailable")
		} else {
			row.AssistantStatus = assistant.Status
			if assistant.Status == store.RunStatusPausedWaitingHuman {
				_, _, pauseErr := s.safeAssistantWatchPause(r.Context(), assistant)
				row.AssistantChatAvailable = pauseErr == nil
			} else {
				_, _, capabilityErr := s.resolveAssistantChatCapability(r.Context(), assistant)
				row.AssistantChatAvailable = capabilityErr == nil
			}
			if code := assistantWatchStatusAttention(assistant.Status); code != "" {
				row.Attention = append(row.Attention, code)
				out.Attention = append(out.Attention, code)
			}
			if !row.AssistantChatAvailable {
				row.Attention = append(row.Attention, "assistant_chat_unavailable")
				out.Attention = append(out.Attention, "assistant_chat_unavailable")
			}
		}

		episodes, episodesErr := s.assistantWatches.ListEpisodesByWatch(r.Context(), watch.ID, watch.TenantID, assistantWatchHealthEpisodePageSize)
		if episodesErr != nil {
			s.httpErrorFor(w, r, http.StatusInternalServerError, "list watch episodes: %v", episodesErr)
			return
		}
		row.EpisodesTruncated = len(episodes) == assistantWatchHealthEpisodePageSize
		for _, ep := range episodes {
			projection := projectAssistantWatchEpisode(ep, now)
			row.Episodes = append(row.Episodes, projection)
			if code := assistantWatchEpisodeAttention(projection, ep.LastError); code != "" {
				row.Attention = append(row.Attention, code)
				out.Attention = append(out.Attention, code)
			}
		}
		row.Attention = uniqueStrings(row.Attention)
		out.Watches = append(out.Watches, row)
	}

	out.Attention = uniqueStrings(out.Attention)
	// The route is intentionally an object even for zero watches: callers can
	// distinguish "nothing is watching" from a legacy empty-array response.
	s.writeJSONFor(w, r, out)
}

func projectAssistantWatchHeartbeat(h assistantWatchHeartbeat) assistantWatchCoordinatorHealth {
	out := assistantWatchCoordinatorHealth{Present: h.Present, Stale: h.Stale}
	if !h.StartedAt.IsZero() {
		at := h.StartedAt
		out.LastSweepStartedAt = &at
	}
	if !h.CompletedAt.IsZero() {
		at := h.CompletedAt
		out.LastSweepCompletedAt = &at
	}
	if h.Duration > 0 {
		out.LastSweepDurationMS = h.Duration.Milliseconds()
	}
	return out
}

func projectAssistantWatchEpisode(ep runwatch.Episode, now time.Time) assistantWatchHealthEpisode {
	due := ep.State == runwatch.EpisodePending && !ep.NextAttemptAt.After(now)
	if ep.State == runwatch.EpisodeProcessing && (ep.LeaseUntil == nil || !ep.LeaseUntil.After(now)) && !ep.NextAttemptAt.After(now) {
		due = true
	}
	overdue := due && !ep.NextAttemptAt.IsZero() && now.Sub(ep.NextAttemptAt) >= 3*assistantWatchSweepInterval
	return assistantWatchHealthEpisode{
		EpisodeID: ep.ID, Kind: ep.Kind, State: ep.State, Attempts: ep.Attempts,
		NextAttemptAt: ep.NextAttemptAt, LastErrorReason: assistantWatchReasonCode(ep.LastError),
		Due: due, Overdue: overdue, CreatedAt: ep.CreatedAt, UpdatedAt: ep.UpdatedAt,
		DeliveredAt: ep.DeliveredAt,
	}
}

func assistantWatchStatusAttention(status store.RunStatus) string {
	switch status {
	case store.RunStatusRunning, store.RunStatusQueued, store.RunStatusPausedWaitingHuman:
		return ""
	case store.RunStatusPausedOperator:
		return "assistant_paused_by_operator"
	case store.RunStatusFailedResumable:
		return "assistant_failed_resumable"
	case store.RunStatusFinished:
		return "assistant_finished"
	case store.RunStatusFailed:
		return "assistant_failed"
	case store.RunStatusCancelled:
		return "assistant_cancelled"
	default:
		return "assistant_" + string(status)
	}
}

func assistantWatchEpisodeAttention(ep assistantWatchHealthEpisode, rawReason string) string {
	if ep.State == runwatch.EpisodeDone {
		return ""
	}
	if ep.State == runwatch.EpisodeBlocked {
		return "episode_blocked"
	}
	if ep.Overdue {
		return "episode_overdue"
	}
	if ep.Due {
		return "episode_due"
	}
	if rawReason != "" {
		return "delivery_" + assistantWatchReasonCode(rawReason)
	}
	return ""
}

func assistantWatchReasonCode(raw string) string {
	lower := strings.ToLower(strings.TrimSpace(raw))
	known := []string{
		"assistant_unavailable", "assistant_busy", "assistant_paused_by_operator",
		"assistant_failed_resumable", "assistant_not_at_chat_boundary",
		"assistant_budget_near_cap", "cooldown", "chat_history_projection_failed",
		"watch_not_active", "target_unavailable", "outcome_run_unavailable",
	}
	for _, code := range known {
		if strings.Contains(lower, code) {
			return code
		}
	}
	if strings.HasPrefix(lower, "assistant_") && !strings.ContainsAny(lower, " \t\r\n") {
		return lower
	}
	return "other"
}

func uniqueStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, value := range in {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
