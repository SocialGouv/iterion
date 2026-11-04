package audit

import (
	"context"
	"errors"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
)

// MemoryStore is the in-process audit log for tests and local mode.
// Keep semantics in lock-step with MongoStore. It owns the Meta map of every
// event it holds, as a database would: Insert stores a copy, a read hands
// out copies, and ScrubActor replaces a map rather than deleting from it —
// an audit response still encoding a row it read must not see the scrub
// land under it.
type MemoryStore struct {
	mu     sync.RWMutex
	events []Event
}

func NewMemoryStore() *MemoryStore { return &MemoryStore{} }

func (s *MemoryStore) Insert(_ context.Context, e Event) error {
	e.Meta = maps.Clone(e.Meta)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
	return nil
}

func (s *MemoryStore) ListByTenant(_ context.Context, tenantID string, p Page) ([]Event, error) {
	return s.list(func(e Event) bool {
		return e.Scope == ScopeTenant && e.TenantID == tenantID
	}, p), nil
}

func (s *MemoryStore) ListPlatform(_ context.Context, p Page) ([]Event, error) {
	return s.list(func(e Event) bool { return e.Scope == ScopePlatform }, p), nil
}

func (s *MemoryStore) ScrubActor(_ context.Context, userID, email string, digests []string) (int64, error) {
	if userID == "" {
		return 0, errors.New("audit: scrub actor: no user id")
	}
	addr := normalizeAddress(email)
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	for i := range s.events {
		e := &s.events[i]
		switch {
		case e.ActorID == userID,
			e.ActorID == "" && e.TargetID == userID,
			digestedAs(*e, digests):
			if !carriesIdentifier(*e, true) {
				continue
			}
			e.IP, e.UserAgent = "", ""
		case e.TargetID == userID || (addr != "" && addressedTo(*e, addr)):
			if !carriesIdentifier(*e, false) {
				continue
			}
		default:
			continue
		}
		meta := maps.Clone(e.Meta)
		for _, k := range ScrubbedMetaKeys {
			delete(meta, k)
		}
		if len(meta) == 0 {
			meta = nil
		}
		e.Meta = meta
		n++
	}
	return n, nil
}

// digestedAs reports whether e's meta.subject_hash is one of digests.
func digestedAs(e Event, digests []string) bool {
	v, ok := e.Meta["subject_hash"].(string)
	return ok && v != "" && slices.Contains(digests, v)
}

// addressedTo reports whether e's meta.email is addr, read under
// normalizeAddress.
func addressedTo(e Event, addr string) bool {
	v, ok := e.Meta["email"].(string)
	return ok && normalizeAddress(v) == addr
}

// normalizeAddress is the comparison ScrubActor reads an address under:
// lower-cased, surrounding blanks trimmed — the Mongo twin's
// case-insensitive, blank-tolerant anchored match.
func normalizeAddress(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// carriesIdentifier reports whether e still carries something ScrubActor
// removes from it — the IP and user agent only when they are the person's
// own (actor) — the memory twin's reading of the Mongo twin's filters, so
// both answer the same count.
func carriesIdentifier(e Event, actor bool) bool {
	if actor && (e.IP != "" || e.UserAgent != "") {
		return true
	}
	for _, k := range ScrubbedMetaKeys {
		if _, ok := e.Meta[k]; ok {
			return true
		}
	}
	return false
}

func (s *MemoryStore) list(match func(Event) bool, p Page) []Event {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Event
	for _, e := range s.events {
		if !match(e) {
			continue
		}
		if p.Action != "" && e.Action != p.Action {
			continue
		}
		if p.ActorID != "" && e.ActorID != p.ActorID {
			continue
		}
		if !p.From.IsZero() && e.CreatedAt.Before(p.From) {
			continue
		}
		if !p.To.IsZero() && e.CreatedAt.After(p.To) {
			continue
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if p.Offset > 0 {
		if p.Offset >= len(out) {
			return nil
		}
		out = out[p.Offset:]
	}
	if lim := ClampLimit(p.Limit); len(out) > lim {
		out = out[:lim]
	}
	for i := range out {
		out[i].Meta = maps.Clone(out[i].Meta)
	}
	return out
}
