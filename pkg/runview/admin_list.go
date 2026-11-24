package runview

// The super-admin cross-org runs read (#2148): one listing that spans
// every tenant, server-side paginated by a stable keyset cursor. The
// tenant guard is lifted HERE, inside the read model — a caller cannot
// forget to lift it (a plain unstamped ctx makes the mongo store panic
// fail-closed) and cannot half-lift it (the read is all-or-nothing;
// narrowing to some teams is a FILTER, never a scope). The gate that
// decides WHO may ask stays in pkg/server (requireSuperAdmin behind
// the audited predicate); this layer only answers the question.

import (
	"context"
	"encoding/base64"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// AdminListFilter scopes ListAcrossTenants. Empty fields mean no
// filter.
type AdminListFilter struct {
	Status store.RunStatus // exact match
	// TeamIDs narrows the answer to runs of these tenants. Empty =
	// every tenant. Narrowing is a filter on the result, not a scope
	// on the read: the caller's own team stamp can never leak into —
	// or out of — the answer.
	TeamIDs []string
	Since   time.Time // UpdatedAt >= Since
	// Limit caps the page; the server clamps it. 0 = no limit.
	Limit int
	// After is the keyset position: only runs strictly older than it
	// are returned. A zero cursor reads from the top.
	After RunCursor
}

// RunCursor is the keyset position of one run in the cross-tenant
// order: (CreatedAt descending, ID descending). Both fields travel in
// the opaque token, so a page boundary stays reproducible whatever the
// store's id ordering is.
type RunCursor struct {
	CreatedAt time.Time
	ID        string
}

// EncodeRunCursor renders the opaque cursor token the API returns. An
// empty token means "no position" and decodes back to the zero cursor.
func EncodeRunCursor(c RunCursor) string {
	if c.ID == "" || c.CreatedAt.IsZero() {
		return ""
	}
	payload := strconv.FormatInt(c.CreatedAt.UnixNano(), 10) + "|" + c.ID
	return base64.RawURLEncoding.EncodeToString([]byte(payload))
}

// DecodeRunCursor parses a token EncodeRunCursor produced. A malformed
// token is an error the caller turns into a 400 — never silently read
// as "from the top", which would replay page one forever.
func DecodeRunCursor(s string) (RunCursor, error) {
	if s == "" {
		return RunCursor{}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return RunCursor{}, fmt.Errorf("malformed cursor: %w", err)
	}
	nano, id, ok := strings.Cut(string(raw), "|")
	if !ok {
		return RunCursor{}, fmt.Errorf("malformed cursor: missing separator")
	}
	ns, err := strconv.ParseInt(nano, 10, 64)
	if err != nil {
		return RunCursor{}, fmt.Errorf("malformed cursor timestamp: %w", err)
	}
	if id == "" {
		return RunCursor{}, fmt.Errorf("malformed cursor: empty run id")
	}
	return RunCursor{CreatedAt: time.Unix(0, ns).UTC(), ID: id}, nil
}

// ListAcrossTenants returns every tenant's runs matching f, newest
// first, as one page plus the token the next page reads from. An empty
// next token means this page was the last.
//
// The read is tenant-blind BY CONSTRUCTION: the ctx this method hands
// the store is TeamBlind(ctx), so a caller cannot accidentally scope
// it to its own team (or leave it unstamped, which the mongo store
// refuses by panic). The context and cancellation of the CALLER are
// honoured — only the tenant stamp is replaced.
func (s *Service) ListAcrossTenants(ctx context.Context, f AdminListFilter) ([]RunSummary, string, error) {
	blind := store.TeamBlind(ctx)
	ids, err := s.store.ListRuns(blind)
	if err != nil {
		return nil, "", err
	}
	// The records a listing returns are LIGHT: LoadRunForListing never
	// loads the recorded workflow source (see ListRunRecordsCtx — the
	// same listing-store upgrade applies here).
	load := s.store.LoadRun
	if lister := store.AsRunListingStore(s.store); lister != nil {
		load = lister.LoadRunForListing
	}
	matched := make([]*store.Run, 0, len(ids))
	for _, id := range ids {
		r, err := load(blind, id)
		if err != nil {
			// A single unreadable run must not break the whole
			// listing — same contract as every other listing path.
			s.logSkippedRun(id, err)
			continue
		}
		if !matchesFilter(r, ListFilter{Status: f.Status, Since: f.Since}) {
			continue
		}
		if !teamAllowed(r.TenantID, f.TeamIDs) {
			continue
		}
		matched = append(matched, r)
	}
	// (CreatedAt, ID) descending. The ID tie-break makes the order
	// TOTAL, which is what lets one keyset token page deterministically
	// across tenants whose runs share a wall-clock instant — the sort
	// the per-team lists use is by CreatedAt alone, where ties are
	// reordered freely.
	sort.Slice(matched, func(i, j int) bool {
		if !matched[i].CreatedAt.Equal(matched[j].CreatedAt) {
			return matched[i].CreatedAt.After(matched[j].CreatedAt)
		}
		return matched[i].ID > matched[j].ID
	})
	if !f.After.CreatedAt.IsZero() {
		kept := matched[:0]
		for _, r := range matched {
			if r.CreatedAt.After(f.After.CreatedAt) {
				continue
			}
			if r.CreatedAt.Equal(f.After.CreatedAt) && r.ID >= f.After.ID {
				continue
			}
			kept = append(kept, r)
		}
		matched = kept
	}
	next := ""
	if f.Limit > 0 && len(matched) > f.Limit {
		last := matched[f.Limit-1]
		next = EncodeRunCursor(RunCursor{CreatedAt: last.CreatedAt, ID: last.ID})
		matched = matched[:f.Limit]
	}
	out := make([]RunSummary, 0, len(matched))
	for _, r := range matched {
		out = append(out, s.summarize(r))
	}
	return out, next, nil
}

// teamAllowed reports whether a run's tenant is in the filter's team
// set. An empty set admits every tenant; a local-mode run (TenantID
// empty) is therefore visible only in the all-tenants view, which is
// the honest answer — the filesystem store has no tenancy to name.
func teamAllowed(tenantID string, teamIDs []string) bool {
	if len(teamIDs) == 0 {
		return true
	}
	for _, id := range teamIDs {
		if id == tenantID {
			return true
		}
	}
	return false
}
