package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/SocialGouv/iterion/pkg/forge"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A gating run the reconciler has found SETTLED — its pull request merged or
// closed, its head moved on, a real verdict on the head it reviewed — owes
// nothing the sweep could repair. The sweep still offered it every minute for
// an hour and every deep pass for eight days, a forge read per offer: on a busy
// repo, 95 % of the runs in the window were settled and the net spent the App
// installation's hourly budget re-reading them.
//
// So the reconciler writes down what it found, keyed by run and bound to the
// run's terminal episode, and the sweep reads those marks — one round-trip per
// page — before offering anything. The marks live beside the publish grant
// they qualify (Valkey when the deployment has it, memory otherwise): their
// use ends with the grant's, and a lost mark costs one re-read, never a wrong
// answer.

// The settlement reasons. A permanent one holds for the rest of the run's
// horizon; a reversible one is re-checked after gateSettleRecheck, because the
// fact can change back — a closed pull request reopened, a head force-pushed
// back to the reviewed commit — and the repair must then be able to act.
const (
	gateSettledMerged         = "merged"
	gateSettledClosed         = "closed"
	gateSettledHeadMoved      = "head_moved"
	gateSettledVerdictSuccess = "verdict_success"
	gateSettledVerdictFailure = "verdict_failure"
	gateSettledUnpinned       = "unpinned"
)

// gateSettleRecheck is how long a reversible settlement holds before the sweep
// reads the pull request again. A reopen or a force-push back that launches no
// fresh review is repaired within it, at ~one read per run per recheck.
const gateSettleRecheck = 6 * time.Hour

// gateSettleMargin keeps a permanent mark alive a little past the end of the
// run's horizon, so no pass can find the run still in its window and the mark
// already gone.
const gateSettleMargin = time.Hour

func gateSettleReversible(reason string) bool {
	return reason == gateSettledClosed || reason == gateSettledHeadMoved
}

// gateSettlement is one mark. Episode is the run's updated_at, in
// milliseconds, at the terminal episode the mark was written for: a resumed
// run that ends again is a new episode, and the old mark no longer applies.
type gateSettlement struct {
	Reason  string `json:"reason"`
	SHA     string `json:"sha,omitempty"`
	Episode int64  `json:"episode"`
}

// appliesTo reports whether the mark was written for the episode updatedAt
// names.
func (g gateSettlement) appliesTo(updatedAt time.Time) bool {
	return !updatedAt.IsZero() && g.Episode == updatedAt.UnixMilli()
}

// gateSettleStore holds the marks. settle is best-effort by contract — a mark
// that fails to land costs a re-read, and the store says so in the log —
// while settled reports a failure to its caller, which then offers every run
// as if nothing were settled.
type gateSettleStore interface {
	settle(runID string, g gateSettlement, ttl time.Duration)
	settled(ctx context.Context, runIDs []string) (map[string]gateSettlement, error)
}

// settleGateRun marks run settled for reason, for as long as the reason holds
// and the run stays inside the sweep's horizon. A run already past the
// horizon is not offered again, and needs no mark. A reversible mark lapses
// after gateSettleRecheck — and at the latest one deep interval before the run
// leaves the window, so the re-read it exists for still happens inside it;
// too close to the edge, it is not written at all.
func (s *Server) settleGateRun(run *store.Run, reason, sha string) {
	if s == nil || s.gateSettles == nil || run == nil || run.ID == "" || run.UpdatedAt.IsZero() {
		return
	}
	remaining := gateSweepHorizon - s.gateNow().Sub(run.UpdatedAt)
	if remaining <= 0 {
		return
	}
	ttl := remaining + gateSettleMargin
	if gateSettleReversible(reason) {
		_, deepEvery := s.gateSweepCadence()
		lastReread := remaining - time.Duration(deepEvery)*s.gateSweepEvery()
		if lastReread <= 0 {
			return
		}
		ttl = min(gateSettleRecheck, lastReread)
	}
	s.gateSettles.settle(run.ID, gateSettlement{Reason: reason, SHA: sha, Episode: run.UpdatedAt.UnixMilli()}, ttl)
}

// settleOwnVerdict closes the books on a run whose owed verdict the publish
// endpoint recorded on its grant: settled with no forge read, and the grant
// cut back to the post-run grace unless something may still read it — a red
// verdict on a repo whose auto-fix lane is on (the lane launches its fixer
// with it). Cutting it back is the window a posted verdict makes safe to
// close, for a bearer that can post a green required check.
func (s *Server) settleOwnVerdict(ctx context.Context, run *store.Run, token string, grant ForgePublishGrant, v *gateVerdict) {
	reason := gateSettledVerdictFailure
	if v.State == string(forge.CommitStateSuccess) {
		reason = gateSettledVerdictSuccess
	}
	s.settleGateRun(run, reason, v.SHA)
	if reason == gateSettledVerdictSuccess || !s.autofixMayReadGrant(ctx, grant) {
		s.cutBackGrant(run, token)
	}
}

// autofixMayReadGrant reports whether the auto-fix lane of the grant's repo is
// on — the one reader a red verdict leaves. A store that cannot answer says
// yes: keeping a grant longer is the safe error.
func (s *Server) autofixMayReadGrant(ctx context.Context, grant ForgePublishGrant) bool {
	if s.forgeIntegrations == nil {
		return false
	}
	integration, err := s.forgeIntegrations.GetByConnRepo(store.WithoutTenantFilter(ctx), grant.TeamID, grant.ConnectionID, grant.Repo)
	if err != nil {
		return !errors.Is(err, forge.ErrIntegrationNotFound)
	}
	return integration.AutoFixOnGateFailure
}

// cutBackGrant brings a run's grant down to the ordinary post-run grace once
// nothing will post with it again — unless the run is not over (resumed after
// the sweep listed it), or a second run shares the grant (cutBack).
func (s *Server) cutBackGrant(run *store.Run, token string) {
	if run == nil || !run.Status.IsTerminal() || s.forgePublishTokens == nil {
		return
	}
	if _, err := s.cutBack(token, s.postRunGrace()); err != nil && s.logger != nil {
		s.logger.Warn("forge gate: run %s owes nothing more, but its grant could not be cut back — it lives out its gate grace: %v", run.ID, err)
	}
}

// cutBack shortens the grant behind token to grace — every shortening to the
// post-run grace goes through it, the grant reaper's included — unless a
// second run shares the grant. The decision and the flag that forbids a later
// share land in ONE update of the grant, so a launch pinning the token races
// it cleanly: it shares the grant first and the grant is kept, or it finds the
// grant cut back and is refused (shareGrant) — never a shared grant shortened
// under its second run. A grant already gone has nothing to cut.
func (s *Server) cutBack(token string, grace time.Duration) (bool, error) {
	if s.forgePublishTokens == nil || token == "" {
		return false, nil
	}
	cut := false
	_, err := s.forgePublishTokens.update(token, func(g *ForgePublishGrant) {
		cut = false // the update may run again on a contended write
		if g.Shared {
			return
		}
		g.CutBack = true
		cut = true
	})
	if err != nil {
		return false, err
	}
	if cut {
		s.forgePublishTokens.expireIn(token, grace)
	}
	return cut, nil
}

// shareGrant marks the grant behind token shared: a second run now publishes
// with it (a launch that pinned the token, a fork), so neither run's verdict
// nor its end may cut it back. It reports cutBack when one already did — the
// grant then lives only the post-run grace — and found=false when the grant is
// gone; the caller decides: a pinned launch on a cut-back grant is refused, a
// fork warns.
func (s *Server) shareGrant(token string) (cutBack, found bool, err error) {
	if s.forgePublishTokens == nil || token == "" {
		return false, false, nil
	}
	found, err = s.forgePublishTokens.update(token, func(g *ForgePublishGrant) {
		cutBack = g.CutBack // the update may run again on a contended write
		if !g.CutBack {
			g.Shared = true
		}
	})
	return cutBack, found, err
}

// --- Valkey ------------------------------------------------------------------

const gateSettleKeyPrefix = "iterion:gate:settled:"

type valkeyGateSettleStore struct {
	rdb    redis.UniversalClient
	logger *iterlog.Logger
}

func newValkeyGateSettleStore(rdb redis.UniversalClient, logger *iterlog.Logger) *valkeyGateSettleStore {
	return &valkeyGateSettleStore{rdb: rdb, logger: logger}
}

func (v *valkeyGateSettleStore) settle(runID string, g gateSettlement, ttl time.Duration) {
	if ttl <= 0 {
		// Valkey keeps a key set with no expiry forever: a mark that outlives
		// the run's horizon would hide nothing, and leak.
		if v.logger != nil {
			v.logger.Warn("forge gate: refusing a settle mark for run %s with no lifetime (%s)", runID, ttl)
		}
		return
	}
	b, err := json.Marshal(g)
	if err == nil {
		ctx, cancel := valkeyCtx()
		defer cancel()
		err = v.rdb.Set(ctx, gateSettleKeyPrefix+runID, b, ttl).Err()
	}
	if err != nil && v.logger != nil {
		v.logger.Warn("forge gate: run %s is settled (%s) but the mark did not land — the sweep will read its pull request again: %v", runID, g.Reason, err)
	}
}

func (v *valkeyGateSettleStore) settled(ctx context.Context, runIDs []string) (map[string]gateSettlement, error) {
	out := map[string]gateSettlement{}
	if len(runIDs) == 0 {
		return out, nil
	}
	keys := make([]string, len(runIDs))
	for i, id := range runIDs {
		keys[i] = gateSettleKeyPrefix + id
	}
	ctx, cancel := context.WithTimeout(ctx, valkeyOpTimeout)
	defer cancel()
	vals, err := v.rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, fmt.Errorf("read settled gating runs: %w", err)
	}
	for i, val := range vals {
		raw, ok := val.(string)
		if !ok {
			continue // absent
		}
		var g gateSettlement
		if json.Unmarshal([]byte(raw), &g) != nil {
			continue // unreadable: the run is offered, which is the safe side
		}
		out[runIDs[i]] = g
	}
	return out, nil
}

// --- memory ------------------------------------------------------------------

// gateSettleMaxMarks bounds the in-memory twin. A mark it cannot hold costs a
// re-read, so saturation is a cost, reported once per episode, never an error.
const gateSettleMaxMarks = 100_000

type memoryGateSettleStore struct {
	mu        sync.Mutex
	marks     map[string]memoryGateSettle
	now       func() time.Time
	logger    *iterlog.Logger
	saturate  bool      // inside a saturation episode, reported once; it ends below 90 %
	lastEvict time.Time // the last full scan for expired marks, at most one a minute
}

type memoryGateSettle struct {
	g       gateSettlement
	expires time.Time
}

func newMemoryGateSettleStore(logger *iterlog.Logger) *memoryGateSettleStore {
	return &memoryGateSettleStore{marks: map[string]memoryGateSettle{}, now: time.Now, logger: logger}
}

func (m *memoryGateSettleStore) settle(runID string, g gateSettlement, ttl time.Duration) {
	if ttl <= 0 {
		if m.logger != nil {
			m.logger.Warn("forge gate: refusing a settle mark for run %s with no lifetime (%s)", runID, ttl)
		}
		return
	}
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, held := m.marks[runID]; !held && len(m.marks) >= gateSettleMaxMarks {
		if now.Sub(m.lastEvict) >= time.Minute {
			m.lastEvict = now
			for id, mk := range m.marks {
				if !now.Before(mk.expires) {
					delete(m.marks, id)
				}
			}
		}
		if len(m.marks) >= gateSettleMaxMarks {
			if !m.saturate && m.logger != nil {
				m.logger.Warn("forge gate: the in-memory settle marks are full (%d) — settled runs past it are read again until marks expire", gateSettleMaxMarks)
			}
			m.saturate = true
			return
		}
	}
	if m.saturate && len(m.marks) < gateSettleMaxMarks*9/10 {
		m.saturate = false
	}
	m.marks[runID] = memoryGateSettle{g: g, expires: now.Add(ttl)}
}

func (m *memoryGateSettleStore) settled(_ context.Context, runIDs []string) (map[string]gateSettlement, error) {
	now := m.now()
	out := map[string]gateSettlement{}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range runIDs {
		if mk, ok := m.marks[id]; ok && now.Before(mk.expires) {
			out[id] = mk.g
		}
	}
	return out, nil
}
