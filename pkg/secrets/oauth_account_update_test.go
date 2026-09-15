package secrets

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Run against memory and Mongo: the observable postimage is the contract,
// including the derived-label condition evaluated by the database itself.
func oauthAccountUpdateConformance(t *testing.T, store OAuthStore, ctx context.Context) {
	t.Helper()
	for _, tc := range []struct {
		name                     string
		clear, rename, reconnect bool
	}{
		{name: "identified"}, {name: "unverified", clear: true},
		{name: "renamed", rename: true}, {name: "unverified renamed", clear: true, rename: true},
		{name: "reconnected", reconnect: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checked := time.Now().UTC().Truncate(time.Millisecond)
			initial := OAuthRecord{UserID: OrgOwnerKey("identity-" + tc.name), Kind: OAuthKindClaudeCode, Rank: 2,
				SealedPayload: []byte("initial sealed token"), Fingerprint: (OAuthAccount{ID: accountA, OrganizationID: organizationA}).Fingerprint(),
				AccountID: accountA, AccountOrganizationID: organizationA, AccountEmail: "$old@example.org", AccountLabel: "$old@example.org", AccountCheckedAt: &checked}
			initial.ID = OAuthRecordID(initial.UserID, initial.Kind, initial.Rank)
			if err := store.Upsert(ctx, initial); err != nil {
				t.Fatal(err)
			}
			const owner = "identity-update"
			ok, err := ClaimRefreshRecord(ctx, store, initial, owner, checked, checked.Add(time.Minute))
			if err != nil || !ok {
				t.Fatalf("claim=%v %v", ok, err)
			}
			if tc.rename {
				if err := store.SetAccountLabel(ctx, initial.ID, "operator name"); err != nil {
					t.Fatal(err)
				}
			}
			account := &OAuthAccountUpdate{ID: accountB, OrganizationID: organizationA, Email: "$new@example.org", CheckedAt: &checked, PreviousEmail: initial.AccountEmail}
			fp := (OAuthAccount{ID: accountB, OrganizationID: organizationA}).Fingerprint()
			if tc.clear {
				account = &OAuthAccountUpdate{Error: "profile unavailable", PreviousEmail: initial.AccountEmail}
				fp = "unverified-fingerprint"
			}
			upd := OAuthTokenUpdate{SealedPayload: []byte("fresh sealed token"), Fingerprint: fp, Account: account}.WithClaim(owner)
			if tc.reconnect {
				if err := store.Upsert(ctx, initial); err != nil {
					t.Fatal(err)
				}
			}
			err = store.UpdateTokens(ctx, initial.ID, upd)
			if tc.reconnect {
				if !errors.Is(err, ErrRefreshClaimLost) {
					t.Fatalf("stale update=%v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			rows, err := store.ListByUser(ctx, initial.UserID)
			if err != nil || len(rows) != 1 {
				t.Fatalf("read=%v rows=%d", err, len(rows))
			}
			got := rows[0]
			if tc.reconnect {
				if string(got.SealedPayload) != string(initial.SealedPayload) || got.Fingerprint != initial.Fingerprint || got.AccountEmail != initial.AccountEmail {
					t.Fatal("stale refresh changed a replacement")
				}
				return
			}
			if string(got.SealedPayload) != "fresh sealed token" || got.Fingerprint != fp || got.AccountID != account.ID || got.AccountOrganizationID != account.OrganizationID || got.AccountEmail != account.Email || got.AccountError != account.Error || got.RefreshClaimOwner != "" {
				t.Fatalf("token and identity postimage disagree: %+v", got)
			}
			if tc.clear {
				if got.AccountCheckedAt != nil {
					t.Fatal("old verification timestamp survived")
				}
			} else if got.AccountCheckedAt == nil || !got.AccountCheckedAt.Equal(checked) {
				t.Fatal("verification timestamp missing")
			}
			wantLabel := account.Email
			if tc.rename {
				wantLabel = "operator name"
			}
			if got.AccountLabel != wantLabel {
				t.Fatalf("label=%q want=%q", got.AccountLabel, wantLabel)
			}
		})
	}
}

func TestMemoryOAuth_RefreshAccountIdentityAtomic(t *testing.T) {
	oauthAccountUpdateConformance(t, NewMemoryOAuthStore(), t.Context())
}

type reconnectAtClaimStore struct {
	OAuthStore
	before func(context.Context) error
}

func (s *reconnectAtClaimStore) ClaimRefresh(ctx context.Context, id, owner string, now, until time.Time) (bool, error) {
	if s.before != nil {
		f := s.before
		s.before = nil
		if err := f(ctx); err != nil {
			return false, err
		}
	}
	return s.OAuthStore.ClaimRefresh(ctx, id, owner, now, until)
}

func oauthSnapshotClaimConformance(t *testing.T, store OAuthStore, ctx context.Context) {
	t.Helper()
	for _, kind := range []OAuthKind{OAuthKindClaudeCode, OAuthKindCodex} {
		t.Run(string(kind), func(t *testing.T) {
			rec := OAuthRecord{UserID: OrgOwnerKey("claim-snapshot"), Kind: kind, Rank: 3, SealedPayload: []byte("snapshot A")}
			rec.ID = OAuthRecordID(rec.UserID, kind, rec.Rank)
			if err := store.Upsert(ctx, rec); err != nil {
				t.Fatal(err)
			}
			next := rec
			next.SealedPayload = []byte("replacement B")
			interleaved := &reconnectAtClaimStore{OAuthStore: store, before: func(ctx context.Context) error { return store.Upsert(ctx, next) }}
			now := time.Now().UTC()
			ok, err := ClaimRefreshRecord(ctx, interleaved, rec, "stale", now, now.Add(time.Minute))
			if err != nil || ok {
				t.Fatalf("replaced snapshot was claimed: %v %v", ok, err)
			}
			ok, err = ClaimRefreshRecord(ctx, store, next, "fresh", now, now.Add(time.Minute))
			if err != nil || !ok {
				t.Fatalf("replacement not immediately refreshable: %v %v", ok, err)
			}
		})
	}
}

func TestMemoryOAuth_ClaimRejectsReplacedSnapshot(t *testing.T) {
	oauthSnapshotClaimConformance(t, NewMemoryOAuthStore(), t.Context())
}
