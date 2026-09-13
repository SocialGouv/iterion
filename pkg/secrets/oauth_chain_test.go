package secrets

import (
	"testing"
	"time"
)

// The ceiling this whole change removes: the store held exactly ONE record
// per (owner, kind), so an operator holding four Claude subscriptions could
// wire two — their org's and the deployment's — and had no way to say "try
// these in this order". On 2026-09-08 that stopped every claude_code run on
// a production deployment for three hours: the org forfait's five-hour
// window closed, the single tier behind it was already spent on its weekly
// one, and there was nowhere to put a third.
//
// Two records for one kind must therefore COEXIST, and come back in the
// order they are meant to be tried.
func TestOAuthChain_TwoCredentialsOfOneKindCoexist(t *testing.T) {
	s := NewMemoryOAuthStore()
	ctx := t.Context()

	primary := OAuthRecord{UserID: "org:acme", Kind: OAuthKindClaudeCode, Rank: 0,
		SealedPayload: []byte("primary"), AccountLabel: "devthejo@gmail.com"}
	fallback := OAuthRecord{UserID: "org:acme", Kind: OAuthKindClaudeCode, Rank: 1,
		SealedPayload: []byte("fallback"), AccountLabel: "jothedev@proton.me"}
	for _, rec := range []OAuthRecord{fallback, primary} { // inserted out of order on purpose
		if err := s.Upsert(ctx, rec); err != nil {
			t.Fatalf("upsert rank %d: %v", rec.Rank, err)
		}
	}

	recs, err := s.ListByUser(ctx, "org:acme")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("the chain holds %d record(s), want 2 — a second credential for one kind was swallowed, "+
			"which is exactly the ceiling that stopped a production fleet", len(recs))
	}
	// Order is the contract: a caller walks the slice and takes the first
	// that serves, without knowing ranks exist.
	if recs[0].Rank != 0 || recs[1].Rank != 1 {
		t.Fatalf("chain order = [%d %d], want [0 1] — the publisher would try the fallback first",
			recs[0].Rank, recs[1].Rank)
	}
	if string(recs[0].SealedPayload) != "primary" || string(recs[1].SealedPayload) != "fallback" {
		t.Fatalf("payloads swapped: %q then %q", recs[0].SealedPayload, recs[1].SealedPayload)
	}

	// Get keeps meaning "the primary" for every reader written before chains.
	got, err := s.Get(ctx, "org:acme", OAuthKindClaudeCode)
	if err != nil {
		t.Fatalf("get primary: %v", err)
	}
	if got.Rank != 0 || string(got.SealedPayload) != "primary" {
		t.Fatalf("Get returned rank %d (%q), want the primary", got.Rank, got.SealedPayload)
	}
}

// Each link is addressed on its own. A writer that still keyed on
// (owner, kind) would land on the primary whatever rank it was called for —
// deleting, renaming or refreshing the wrong credential while reporting
// success, which is worse than refusing.
func TestOAuthChain_WritesAddressOneLinkNotThePair(t *testing.T) {
	s := NewMemoryOAuthStore()
	ctx := t.Context()
	for rank, label := range map[int]string{0: "primary", 1: "fallback"} {
		if err := s.Upsert(ctx, OAuthRecord{UserID: "u", Kind: OAuthKindCodex, Rank: rank,
			SealedPayload: []byte(label), AccountLabel: label}); err != nil {
			t.Fatalf("seed rank %d: %v", rank, err)
		}
	}

	// Rename the FALLBACK only.
	if err := s.SetAccountLabel(ctx, OAuthRecordID("u", OAuthKindCodex, 1), "renamed"); err != nil {
		t.Fatalf("rename rank 1: %v", err)
	}
	primary, err := s.Get(ctx, "u", OAuthKindCodex)
	if err != nil {
		t.Fatalf("get primary: %v", err)
	}
	if primary.AccountLabel != "primary" {
		t.Fatalf("the primary was renamed to %q — the write landed on the wrong link", primary.AccountLabel)
	}

	// Delete the FALLBACK only.
	if err := s.Delete(ctx, OAuthRecordID("u", OAuthKindCodex, 1)); err != nil {
		t.Fatalf("delete rank 1: %v", err)
	}
	recs, err := s.ListByUser(ctx, "u")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(recs) != 1 || recs[0].Rank != 0 {
		t.Fatalf("after deleting rank 1 the chain is %+v, want the primary alone", recs)
	}
}

// A refresh claims ONE record. Two links of the same chain must be claimable
// independently — otherwise the sweep renewing the primary would fence the
// fallback out of its own refresh, and the fallback would expire quietly
// while every claim test stayed green.
func TestOAuthChain_ClaimsArePerLink(t *testing.T) {
	s := NewMemoryOAuthStore()
	ctx := t.Context()
	now := time.Now().UTC()
	for _, rank := range []int{0, 1} {
		if err := s.Upsert(ctx, OAuthRecord{UserID: "u", Kind: OAuthKindClaudeCode, Rank: rank,
			SealedPayload: []byte("x")}); err != nil {
			t.Fatalf("seed rank %d: %v", rank, err)
		}
	}
	okPrimary, err := s.ClaimRefresh(ctx, OAuthRecordID("u", OAuthKindClaudeCode, 0), "sweep-a", now, now.Add(time.Minute))
	if err != nil || !okPrimary {
		t.Fatalf("claim primary = %v, %v; want true", okPrimary, err)
	}
	okFallback, err := s.ClaimRefresh(ctx, OAuthRecordID("u", OAuthKindClaudeCode, 1), "sweep-b", now, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("claim fallback: %v", err)
	}
	if !okFallback {
		t.Fatal("claiming the fallback was refused while the primary was claimed — the fence is on the pair, " +
			"not on the record, so the fallback would never be refreshed")
	}
}
