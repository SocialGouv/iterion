package server

import (
	"sync"
	"time"
)

// gatesMergesTTL is how long a (tenant, bot) merge-gate posture is reused.
// Resolving one bot is a bot-source store round-trip — a GetBySlug plus, for
// a catalog bot, an unprojected ListByTenant of the team's sources — and the
// repo listing that needs it is polled every 30 s by every open studio tab,
// over rows that share their bots. A window of one poll interval removes
// nearly all of those reads; a bot whose declaration changes arrives through
// a push, so answering from a window-old read is invisible to the operator.
const gatesMergesTTL = 30 * time.Second

// gatesMergesSweepAt is the entry count at which an insert drops the expired
// entries. The map is keyed by (tenant, bot), so it is bounded by the pairs a
// deployment polls within one window; the sweep is what keeps a departed
// team's bots out of it for the rest of the process's life.
const gatesMergesSweepAt = 256

// gatesMergesAnswer is one bot's resolved posture: it gates merges, or it
// does not resolve at all. A row binding an unreadable bot reports NO
// posture, which is a different fact from "nothing gates here", so the
// failure travels with the answer — but only within the request that observed
// it: it is never memoised (see answer).
type gatesMergesAnswer struct {
	gates bool
	err   error
}

type gatesMergesEntry struct {
	answer   gatesMergesAnswer
	storedAt time.Time
}

// gatesMergesMemo holds those answers for a short window, across requests.
// The zero value is ready; it is read by concurrent requests.
type gatesMergesMemo struct {
	mu      sync.Mutex
	entries map[string]gatesMergesEntry
	// clock overrides the wall clock the window is measured on (test seam — a
	// test cannot wait 30 s for an entry to expire). nil → time.Now.
	clock func() time.Time
	// ttl overrides gatesMergesTTL (test seam). Zero → gatesMergesTTL.
	ttl time.Duration
}

// answer returns the posture of (tenant, bot), calling resolve on a miss or
// an expired entry. resolve runs OUTSIDE the lock: it is a store round-trip,
// and holding the mutex across it would serialise every poller behind one
// bot. Two requests racing the same miss therefore both resolve it, and the
// later write wins — the answer is the same either way.
//
// Only a RESOLUTION is stored. A failure is answered to the request that
// observed it and nothing else: a cancelled request context, a store blip or
// a bot read on a tier that was unreachable says nothing about the bot, and
// an entry would outlive the condition that produced it — the next request,
// healthy, would answer from it and hide the repo's Merge gate card for the
// rest of the window. The next request resolves again, and warns again.
func (m *gatesMergesMemo) answer(tenantID, botID string, resolve func() gatesMergesAnswer) gatesMergesAnswer {
	key := tenantID + "\x00" + botID
	m.mu.Lock()
	e, ok := m.entries[key]
	fresh := ok && m.now().Sub(e.storedAt) < m.window()
	m.mu.Unlock()
	if fresh {
		return e.answer
	}

	a := resolve()
	if a.err != nil {
		return a
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.entries == nil {
		m.entries = map[string]gatesMergesEntry{}
	}
	now := m.now()
	if len(m.entries) >= gatesMergesSweepAt {
		m.sweepLocked(now)
	}
	m.entries[key] = gatesMergesEntry{answer: a, storedAt: now}
	return a
}

// sweepLocked drops every entry past the window. Called with the mutex held.
func (m *gatesMergesMemo) sweepLocked(now time.Time) {
	window := m.window()
	for k, e := range m.entries {
		if now.Sub(e.storedAt) >= window {
			delete(m.entries, k)
		}
	}
}

func (m *gatesMergesMemo) now() time.Time {
	if m.clock != nil {
		return m.clock()
	}
	return time.Now()
}

func (m *gatesMergesMemo) window() time.Duration {
	if m.ttl > 0 {
		return m.ttl
	}
	return gatesMergesTTL
}
