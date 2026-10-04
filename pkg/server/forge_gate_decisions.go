package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/SocialGouv/iterion/pkg/forge"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// A verdict posted on a head is a DECISION, and the decisions on one head must
// land in the order they were made. A deferred verdict is posted late; posted
// blindly, it would overwrite a newer one — a re-review's red answered, an hour
// later, by a stale green on a required check. So every verdict the publish
// endpoint posts, fresh or replayed, first claims its check for its decision:
// one mark per (forge, repo, head, check) naming the newest decision claimed
// there and the status it posts, and a claim older than the mark is refused —
// superseded. Two decisions in flight at once can still land out of order; the
// one that landed then re-reads the mark and puts the newer status back on top
// (reassertNewerVerdict).
//
// Decisions are ordered by the clock of the replica that took each one: the
// replicas' clocks are assumed synchronized far below the gap between two
// verdicts on one head. A status written outside iterion — an operator's
// manual override — is not a decision: a replay that comes due after it
// overwrites it.

// gateDecision is one verdict decision: when the publish endpoint took it.
type gateDecision struct {
	ID string    `json:"id"`
	At time.Time `json:"at"`
}

// newGateDecision is a decision taken at now.
func newGateDecision(now time.Time) gateDecision {
	return gateDecision{ID: rand.Text(), At: now.UTC().Truncate(time.Millisecond)}
}

// gateDecisionAt is a decision taken at an explicit instant. The publish
// endpoint decides at now; the reconciler's synthetic failure decides at the
// run's TERMINAL instant, so a verdict decided after the run died — the
// relaunch's, a fresh push's review — supersedes it no matter which one
// reaches the authority first.
func gateDecisionAt(t time.Time) gateDecision {
	return gateDecision{ID: rand.Text(), At: t.UTC().Truncate(time.Millisecond)}
}

// newerThan reports whether d was decided after o.
func (d gateDecision) newerThan(o gateDecision) bool {
	return d.At.After(o.At)
}

// gateDecisionTTL bounds a mark's life: the longest a grant — and a deferral
// on it, still able to replay a decision older than the mark — can live after
// that decision. A grant is minted at most forgePublishDefaultTTL before its
// run ends, and kept forgePublishGateGrace past that end.
const gateDecisionTTL = forgePublishDefaultTTL + forgePublishGateGrace

// gateMarkStatus is the status a decision posts.
type gateMarkStatus struct {
	State       string `json:"state"`
	Description string `json:"description"`
	TargetURL   string `json:"target_url,omitempty"`
}

// gateMark is what a key holds: the newest decision claimed there, and the
// status it posts — what an older post that landed after it puts back.
//
// A decision that will neither post nor wait is ABANDONED: its claim leads to
// nothing, and holding the key would supersede deferred verdicts older than
// it that nobody else will answer. Abandoned, the key stands for the newest
// LIVE decision its Prev chain names — the one that posted before it, or
// waits to (live).
type gateMark struct {
	Decision  gateDecision   `json:"decision"`
	Status    gateMarkStatus `json:"status"`
	Abandoned bool           `json:"abandoned,omitempty"`
	Prev      *gateMark      `json:"prev,omitempty"`
}

// live is the decision this key effectively stands for: the mark itself while
// it is held, else the newest live predecessor.
func (m gateMark) live() gateMark {
	for m.Abandoned && m.Prev != nil {
		m = *m.Prev
	}
	if m.Abandoned {
		return gateMark{}
	}
	return m
}

// gateDecisionStore holds the marks.
type gateDecisionStore interface {
	// claim records m on key unless the mark there names a newer decision, and
	// reports whether m's decision may post.
	claim(ctx context.Context, key string, m gateMark, ttl time.Duration) (bool, error)
	// newest reads the decision key effectively stands for; found is false
	// when there is none.
	newest(ctx context.Context, key string) (m gateMark, found bool, err error)
	// release abandons key's mark when it still names d: its decision will
	// neither post nor wait, and older deferred verdicts must be able to land.
	release(ctx context.Context, key string, d gateDecision) error
}

// gateDecisionKey names one check on one head of one repo of one forge: two
// forges with the same repo path and the same commit carry two status streams.
// Two connections to one forge share its stream, and so its order.
func gateDecisionKey(conn forge.Connection, repo, sha, check string) string {
	forgeID := strings.ToLower(strings.TrimRight(strings.TrimSpace(conn.BaseURL()), "/"))
	sum := sha256.Sum256([]byte(forgeID + "\x00" + strings.ToLower(strings.TrimSpace(repo)) + "\x00" +
		strings.ToLower(strings.TrimSpace(sha)) + "\x00" + strings.TrimSpace(check)))
	return hex.EncodeToString(sum[:16])
}

// claimGateDecision claims key for m's decision. The zero decision — a post
// that takes no part in the ordering — and a deployment without the store
// always may post.
func (s *Server) claimGateDecision(ctx context.Context, key string, m gateMark) (bool, error) {
	if s.gateDecisions == nil || m.Decision.ID == "" {
		return true, nil
	}
	return s.gateDecisions.claim(ctx, key, m, gateDecisionTTL)
}

// releaseGateDecision abandons the claim a decision holds once it is clear it
// will neither post nor wait — its post was refused for good, or keeping it on
// the grant failed — so an older deferred verdict can still land.
func (s *Server) releaseGateDecision(ctx context.Context, markKey string, d gateDecision) {
	if s.gateDecisions == nil || markKey == "" || d.ID == "" {
		return
	}
	if err := s.gateDecisions.release(ctx, markKey, d); err != nil {
		s.logWarn("forge gate: the verdict-order claim of a verdict that will not land could not be released: %v", err)
	}
}

// gateReassertMax bounds how many newer decisions one post puts back on top:
// each is a decision that claimed the head while the previous write was in
// flight.
const gateReassertMax = 3

// reassertNewerVerdict runs once a decision's status has landed. A newer
// decision that claimed the head while that post was in flight may have
// landed first — and be under it now. The mark names the newest decision and
// its status, so the post that landed late writes that status back on top.
func (s *Server) reassertNewerVerdict(ctx context.Context, gc forgeGateClient, key, repo, sha, check string, posted gateDecision) {
	if s.gateDecisions == nil || posted.ID == "" {
		return
	}
	cur := posted
loop:
	for i := 0; i < gateReassertMax; i++ {
		m, found, err := s.gateDecisions.newest(ctx, key)
		switch {
		case err != nil:
			s.logWarn("forge gate: %s on %s@%s posted, but its order could not be re-read (%v) — a newer verdict posted meanwhile may be under it", check, repo, shortSHA(sha), err)
			break loop
		case !found || m.Decision.ID == cur.ID || !m.Decision.newerThan(cur) || m.Status.State == "":
			break loop
		}
		// Never put a SYNTHETIC status back on top of what just landed. The
		// reconciler's failure is anchored at its run's terminal instant, so
		// it reads as "newer" than a verdict DECIDED before that death but
		// POSTED after it — re-asserting buries a legitimate verdict under a
		// marker (#1632 probe: R2's own re-assert covered its success with
		// "review refused to certify", and nothing healed it). The asymmetry
		// is deliberate: a synthetic marker is re-derivable (the reconciler's
		// live read posts it again on a later pass if the head is still
		// unanswered), a displaced real verdict is gone for good — the same
		// hierarchy the reconciler applies when it reads (never overwrite a
		// real verdict), applied to the write that re-assert performs.
		if isSyntheticGateInterruption(m.Status.Description) {
			break loop
		}
		if err := gc.SetCommitStatus(ctx, repo, sha, forge.CommitStatus{
			State:       forge.CommitState(m.Status.State),
			Context:     check,
			Description: m.Status.Description,
			TargetURL:   m.Status.TargetURL,
		}); err != nil {
			s.logWarn("forge gate: a newer verdict (%s) claimed %s on %s@%s while an older one was posted, and could not be put back on top: %v", m.Status.State, check, repo, shortSHA(sha), err)
			break loop
		}
		if s.logger != nil {
			s.logger.Info("forge gate: a newer verdict claimed %s on %s@%s while an older one was posted — put back on top (%s)", check, repo, shortSHA(sha), m.Status.State)
		}
		cur = m.Decision
	}
	// Bounded: a decision claiming inside the last write may still be under
	// it. Said here; healed by the next verdict posted on this head, whose own
	// re-assert puts it back.
	if m, found, err := s.gateDecisions.newest(ctx, key); err != nil {
		s.logWarn("forge gate: the verdict-order mark of %s on %s@%s could not be re-read (%v) — a newer verdict may be under the one just posted", check, repo, shortSHA(sha), err)
	} else if found && m.Decision.ID != cur.ID && m.Decision.newerThan(cur) {
		s.logWarn("forge gate: a newer verdict (%s) claimed %s on %s@%s during the re-assert and may still be under the one just posted — the next verdict posted on this head puts it back", m.Status.State, check, repo, shortSHA(sha))
	}
}

// --- Valkey ------------------------------------------------------------------

const gateDecisionKeyPrefix = "iterion:gate:decision:"

type valkeyGateDecisionStore struct {
	rdb redis.UniversalClient
}

func newValkeyGateDecisionStore(rdb redis.UniversalClient) *valkeyGateDecisionStore {
	return &valkeyGateDecisionStore{rdb: rdb}
}

func (v *valkeyGateDecisionStore) claim(ctx context.Context, key string, m gateMark, ttl time.Duration) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, valkeyOpTimeout)
	defer cancel()
	full := gateDecisionKeyPrefix + key
	granted := false
	txf := func(tx *redis.Tx) error {
		granted = false
		raw, err := tx.Get(ctx, full).Bytes()
		var cur gateMark
		switch {
		case errors.Is(err, redis.Nil):
		case err != nil:
			return err
		default:
			if json.Unmarshal(raw, &cur) != nil {
				break // an unreadable mark does not order anything
			}
			live := cur.live()
			if live.Decision.ID != "" && live.Decision.ID == m.Decision.ID {
				// This decision re-claiming its own mark (a replay): keep the
				// entry as it stands, refresh its life.
				_, err = tx.TxPipelined(ctx, func(p redis.Pipeliner) error {
					p.Set(ctx, full, raw, ttl)
					return nil
				})
				granted = err == nil
				return err
			}
			// Tie-break toward the INCUMBENT: only a strictly newer decision
			// displaces the live mark. Decisions truncate to the millisecond
			// and the reconciler's anchor clamps to the server's now, so a
			// tie is ordinary — and refusing it is what keeps a synthetic
			// failure claiming in the same millisecond from beating a
			// verdict (or an operator's approve) that claimed first.
			if live.Decision.ID != "" && !m.Decision.newerThan(live.Decision) {
				return nil
			}
			m.Prev = &live
		}
		b, err := json.Marshal(m)
		if err != nil {
			return err
		}
		_, err = tx.TxPipelined(ctx, func(p redis.Pipeliner) error {
			p.Set(ctx, full, b, ttl)
			return nil
		})
		granted = err == nil
		return err
	}
	for attempt := 0; attempt < 3; attempt++ {
		err := v.rdb.Watch(ctx, txf, full)
		if errors.Is(err, redis.TxFailedErr) {
			continue // another decision landed meanwhile: compare again
		}
		if err != nil {
			return false, fmt.Errorf("claim the gate decision: %w", err)
		}
		return granted, nil
	}
	return false, errors.New("claim the gate decision: still contended after 3 attempts")
}

func (v *valkeyGateDecisionStore) newest(ctx context.Context, key string) (gateMark, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, valkeyOpTimeout)
	defer cancel()
	raw, err := v.rdb.Get(ctx, gateDecisionKeyPrefix+key).Bytes()
	if errors.Is(err, redis.Nil) {
		return gateMark{}, false, nil
	}
	if err != nil {
		return gateMark{}, false, fmt.Errorf("read the gate decision: %w", err)
	}
	var m gateMark
	if err := json.Unmarshal(raw, &m); err != nil {
		return gateMark{}, false, fmt.Errorf("read the gate decision: %w", err)
	}
	if live := m.live(); live.Decision.ID != "" {
		return live, true, nil
	}
	return gateMark{}, false, nil
}

func (v *valkeyGateDecisionStore) release(ctx context.Context, key string, d gateDecision) error {
	ctx, cancel := context.WithTimeout(ctx, valkeyOpTimeout)
	defer cancel()
	full := gateDecisionKeyPrefix + key
	txf := func(tx *redis.Tx) error {
		raw, err := tx.Get(ctx, full).Bytes()
		if errors.Is(err, redis.Nil) {
			return nil
		}
		if err != nil {
			return err
		}
		var cur gateMark
		if json.Unmarshal(raw, &cur) != nil || cur.Abandoned || cur.Decision.ID != d.ID {
			return nil // not this decision's claim any more, or already let go
		}
		cur.Abandoned = true
		b, err := json.Marshal(cur)
		if err != nil {
			return err
		}
		_, err = tx.TxPipelined(ctx, func(p redis.Pipeliner) error {
			p.Set(ctx, full, b, gateDecisionTTL)
			return nil
		})
		return err
	}
	for attempt := 0; attempt < 3; attempt++ {
		err := v.rdb.Watch(ctx, txf, full)
		if errors.Is(err, redis.TxFailedErr) {
			continue
		}
		if err != nil {
			return fmt.Errorf("release the gate decision: %w", err)
		}
		return nil
	}
	return errors.New("release the gate decision: still contended after 3 attempts")
}

// --- memory ------------------------------------------------------------------

// gateDecisionMaxMarks bounds the in-memory twin. Full of live marks, a claim
// on a further head is granted unrecorded — that head goes unordered, which is
// said once per saturation.
const gateDecisionMaxMarks = 100_000

type memoryGateDecisionStore struct {
	mu     sync.Mutex
	marks  map[string]memoryGateMark
	now    func() time.Time
	logger *iterlog.Logger
	full   bool
}

type memoryGateMark struct {
	m       gateMark
	expires time.Time
}

func (s *memoryGateDecisionStore) release(_ context.Context, key string, d gateDecision) error {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.marks[key]; ok && !cur.m.Abandoned && cur.m.Decision.ID == d.ID {
		cur.m.Abandoned = true
		cur.expires = now.Add(gateDecisionTTL)
		s.marks[key] = cur
	}
	return nil
}

func newMemoryGateDecisionStore(logger *iterlog.Logger) *memoryGateDecisionStore {
	return &memoryGateDecisionStore{marks: map[string]memoryGateMark{}, now: time.Now, logger: logger}
}

func (s *memoryGateDecisionStore) claim(_ context.Context, key string, m gateMark, ttl time.Duration) (bool, error) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, held := s.marks[key]
	live := cur.m.live()
	if held && now.Before(cur.expires) {
		if live.Decision.ID != "" && live.Decision.ID == m.Decision.ID {
			cur.expires = now.Add(ttl) // this decision re-claiming its own mark
			s.marks[key] = cur
			return true, nil
		}
		// Tie-break toward the INCUMBENT: only a strictly newer decision
		// displaces the live mark. Decisions truncate to the millisecond and
		// the reconciler's anchor clamps to the server's now, so a tie is
		// ordinary — and refusing it keeps a synthetic failure claiming in
		// the same millisecond from beating a verdict that claimed first.
		if live.Decision.ID != "" && !m.Decision.newerThan(live.Decision) {
			return false, nil
		}
		m.Prev = &live
	}
	if !held && len(s.marks) >= gateDecisionMaxMarks {
		for k, mk := range s.marks {
			if !now.Before(mk.expires) {
				delete(s.marks, k)
			}
		}
		if len(s.marks) >= gateDecisionMaxMarks {
			if !s.full && s.logger != nil {
				s.logger.Warn("forge gate: the in-memory verdict-order marks are full (%d) — verdicts on further heads post unordered", gateDecisionMaxMarks)
			}
			s.full = true
			return true, nil
		}
		s.full = false
	}
	s.marks[key] = memoryGateMark{m: m, expires: now.Add(ttl)}
	return true, nil
}

func (s *memoryGateDecisionStore) newest(_ context.Context, key string) (gateMark, bool, error) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.marks[key]; ok && now.Before(cur.expires) {
		if live := cur.m.live(); live.Decision.ID != "" {
			return live, true, nil
		}
	}
	return gateMark{}, false, nil
}
