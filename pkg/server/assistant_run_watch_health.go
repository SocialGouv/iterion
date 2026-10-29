package server

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/clock"
	"github.com/SocialGouv/iterion/pkg/lease"
	"github.com/SocialGouv/iterion/pkg/runwatch"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The holder_state values of the watch-health answer: what the shared lease
// store said about the assistant sweep's lease when the route was asked.
const (
	assistantWatchHolderHeld       = "held"       // a replica holds it, named by holder
	assistantWatchHolderFree       = "free"       // nobody holds it: a hand-over in progress
	assistantWatchHolderExpired    = "expired"    // a holder nobody replaced: the sweep is dead
	assistantWatchHolderUnreadable = "unreadable" // the lease store did not answer
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
	Present bool `json:"present"`
	// Elected says whether THIS replica holds the sweep's lease. The sweep
	// runs on one replica at a time (#2636), so a replica that answers
	// `elected: false` owes no sweep: its last-sweep fields are the ones it
	// recorded in its last term, if any, and its own sweeps never make it
	// Stale. Stale without Elected has three other sources: no coordinator
	// at all (Present false), a coordinator whose project moved under the
	// request (a local project switch between the capture and the read), or
	// a lease past its expiry — a holder nobody replaced, so the sweep is
	// dead fleet-wide and the answer says so wherever it is read
	// (attention sweep_holder_expired).
	Elected bool `json:"elected"`
	// Holder names the replica the shared lease store records as holding the
	// sweep's lease, with the expiry it recorded (HolderExpiresAt). Read from
	// the store, so every replica's answer names who holds the lease, where
	// Elected only says whether this one does (#2680). It names the lease,
	// not progress: a holder whose passes fail still holds it, and only its
	// own answer is Stale for that. HolderState says what the store answered
	// — held, free (nobody holds it: the gap between a release and the
	// successor's next attempt, normal during a hand-over), expired (a holder
	// nobody replaced, attention sweep_holder_expired) or unreadable (the
	// store did not answer, attention sweep_holder_unreadable).
	Holder          string     `json:"holder,omitempty"`
	HolderExpiresAt *time.Time `json:"holder_expires_at,omitempty"`
	HolderState     string     `json:"holder_state,omitempty"`
	// Handover is THIS replica's handover state: whether its term can end on
	// the work failing at all, where the current run of failing passes
	// stands, how many terms it has handed over, and whether it has stopped
	// handing over because the peers gave the lease back just as sick
	// (#2685). It is the replica's, like Elected and unlike Holder: the
	// policy and its counters live in the campaign, not in the lease
	// document, so the answer to read is the one from the replica that
	// SERVES — and `elected: true` with a run of failing passes is exactly
	// the sick-but-alive leader this field exists to make visible. Absent
	// when no campaign armed a policy.
	Handover *lease.HandoverState `json:"handover,omitempty"`
	// termStartedAt is the heartbeat's term start, for sweepStaleIsAttention;
	// not part of the answer.
	termStartedAt        time.Time
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
	snapshot := s.captureAssistantWatch()
	r = r.WithContext(snapshot.withBotPaths(r.Context()))
	run, err := snapshot.runs.LoadRunCtx(r.Context(), r.PathValue("id"))
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
	now := clock.Default.Now().UTC()
	if c := s.assistantWatch; c != nil {
		now = c.now()
	}
	if s.assistantWatch != nil {
		out.Coordinator = projectAssistantWatchHeartbeat(snapshot.heartbeat())
		// The heartbeat knows whether THIS replica holds the lease; the lease
		// store knows who does. Reading it here lets any replica's answer
		// name the holder, instead of nine answers in ten saying only "not
		// me".
		h, held, err := s.sweepLeaseHolder(r.Context())
		switch {
		case err != nil:
			// A request the client already abandoned fails the read too;
			// that is not the store's failure, and nobody reads the answer.
			if r.Context().Err() == nil {
				s.logWarn("assistant watch health: read the sweep lease holder: %v", err)
			}
			out.Coordinator.HolderState = assistantWatchHolderUnreadable
			out.Attention = append(out.Attention, "sweep_holder_unreadable")
		case !held:
			// Nobody holds it: the gap between a holder's release and its
			// successor's next attempt, normal during a hand-over — a state
			// to read, never an attention code that turns a rollout into an
			// alarm.
			out.Coordinator.HolderState = assistantWatchHolderFree
		case !h.ExpiresAt.After(now):
			// Nobody renews and nobody took over: the sweep is dead for the
			// whole fleet, not just for the replica asked.
			expires := h.ExpiresAt
			out.Coordinator.Holder, out.Coordinator.HolderExpiresAt = h.Owner, &expires
			out.Coordinator.HolderState = assistantWatchHolderExpired
			out.Coordinator.Stale = true
			out.Attention = append(out.Attention, "sweep_holder_expired")
		default:
			expires := h.ExpiresAt
			out.Coordinator.Holder, out.Coordinator.HolderExpiresAt = h.Owner, &expires
			out.Coordinator.HolderState = assistantWatchHolderHeld
		}
		// The handover of the replica that answers. Reported whether or not
		// it is elected: a replica that just handed a term over is not
		// elected any more, and its counters are what says why the lease
		// moved.
		if handover := snapshot.handoverState(); handover.Armed || handover.Handovers > 0 {
			out.Coordinator.Handover = &handover
			if handover.CircuitOpen {
				// Every peer took the lease and gave it back just as sick,
				// so this replica keeps serving a net whose passes fail: no
				// hand-over left to make, and nothing recovers it but the far
				// side coming back.
				out.Attention = append(out.Attention, "sweep_handover_circuit_open")
			}
		}
		if sweepStaleIsAttention(out.Coordinator, now) {
			out.Attention = append(out.Attention, "sweep_stale")
		}
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
	if snapshot.store != nil {
		watches, err = snapshot.listCoveringAssistantWatches(r.Context(), run)
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

		assistant, assistantErr := snapshot.runs.LoadRunCtx(r.Context(), watch.AssistantRunID)
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

		episodes, episodesErr := snapshot.store.ListEpisodesByWatch(r.Context(), watch.ID, watch.TenantID, assistantWatchHealthEpisodePageSize)
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

// sweepStaleIsAttention says whether a coordinator's staleness is the holder's
// own dead sweep — attention, not a red field the CLI exits 0 on — or one of
// the stale states that are no outage: a follower (it owes no sweep; stale
// only for a project moved under the request), an expired lease (its own
// code), or a term's first moments. Those moments are counted from the TERM's
// start, never from the last pass that started: a failing pass starts again
// every interval, and a holder whose every pass failed since its election
// would otherwise read fresh for ever. "Stopped" is three sweep intervals of
// the term without a pass completed in it — a completion from an earlier
// term (a re-elected replica) does not count.
func sweepStaleIsAttention(c assistantWatchCoordinatorHealth, now time.Time) bool {
	if !c.Elected || !c.Stale || c.HolderState == assistantWatchHolderExpired {
		return false
	}
	completedThisTerm := c.LastSweepCompletedAt != nil && !c.LastSweepCompletedAt.Before(c.termStartedAt)
	fresh := !completedThisTerm && now.Sub(c.termStartedAt) <= 3*assistantWatchSweepInterval
	return !fresh
}

// sweepLeaseHolder reads who holds the assistant sweep's lease from the shared
// lease store. A server built without one cannot answer: that is an error the
// route reports, never a healthy-looking silence.
func (s *Server) sweepLeaseHolder(ctx context.Context) (lease.Holding, bool, error) {
	if s.leases == nil {
		return lease.Holding{}, false, errors.New("no lease store is wired")
	}
	return s.leases.Holder(ctx, leaseAssistantWatchSweep)
}

func projectAssistantWatchHeartbeat(h assistantWatchHeartbeat) assistantWatchCoordinatorHealth {
	out := assistantWatchCoordinatorHealth{Present: h.Present, Elected: h.Elected, Stale: h.Stale, termStartedAt: h.TermStartedAt}
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
