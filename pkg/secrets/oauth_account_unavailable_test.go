package secrets

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// verifiedAnthropicRecord is a credential whose account has been CONFIRMED:
// it carries the shared, rotation-stable account fingerprint that every
// tier's usage readings are filed under.
func verifiedAnthropicRecord(t *testing.T) (*OAuthRecord, time.Time) {
	t.Helper()
	checked := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	account := OAuthAccount{ID: accountA, OrganizationID: organizationA, Email: "person@example.org"}
	if !IsAccountFingerprint(account.Fingerprint()) {
		t.Fatal("fixture is not a verified account fingerprint")
	}
	return &OAuthRecord{
		UserID: "alice", Kind: OAuthKindClaudeCode,
		Scopes:                []string{"user:inference", "user:profile"},
		AccountID:             account.ID,
		AccountOrganizationID: account.OrganizationID,
		AccountEmail:          account.Email,
		AccountCheckedAt:      &checked,
		Fingerprint:           account.Fingerprint(),
	}, checked
}

func profileClient(fn func(*http.Request) (*http.Response, error)) *http.Client {
	return &http.Client{Transport: profileTransport(fn)}
}

func status(code int) func(*http.Request) (*http.Response, error) {
	return func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	}
}

// An UNAVAILABLE lookup says nothing about the credential, so it must unlearn
// nothing. Before this distinction existed, a five-second blip at
// api.anthropic.com during a routine refresh erased the verified account and
// re-keyed the credential to SubscriptionFingerprint — the hash of the
// just-rotated payload. That key is per-credential and per-rotation where the
// account fingerprint is shared and stable, so usagecap.Key stopped resolving
// to the account scope, Latest returned no readings, and the closed-window
// skip silently stopped protecting the forfait.
func TestUnavailableProfileLookupKeepsTheVerifiedAccount(t *testing.T) {
	for _, tc := range []struct {
		name string
		fn   func(*http.Request) (*http.Response, error)
	}{
		{"transport failure", func(*http.Request) (*http.Response, error) { return nil, errors.New("dial tcp: i/o timeout") }},
		{"rate limited", status(http.StatusTooManyRequests)},
		{"provider 5xx", status(http.StatusServiceUnavailable)},
		{"captive portal serves HTML with 200", func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("<html>sign in</html>"))}, nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, checked := verifiedAnthropicRecord(t)
			want := rec.Fingerprint

			identifyRefreshedAnthropicAccount(t.Context(), profileClient(tc.fn), rec, []byte("rotated-payload"), "opaque-token")

			if rec.Fingerprint != want {
				t.Errorf("fingerprint = %q, want the account key %q kept — the credential's usage ledger is now un-keyed", rec.Fingerprint, want)
			}
			if rec.AccountID != accountA || rec.AccountEmail != "person@example.org" {
				t.Errorf("identity dropped: id=%q email=%q", rec.AccountID, rec.AccountEmail)
			}
			if rec.AccountCheckedAt == nil || !rec.AccountCheckedAt.Equal(checked) {
				t.Errorf("checked_at = %v, want the last CONFIRMED check %v — a failed pass must not date a verification that did not happen", rec.AccountCheckedAt, checked)
			}
			if rec.AccountError == "" {
				t.Error("the failed pass left no trace: a run of these must be visible, not silent")
			}
			// The atomic update the store commits has to carry the same
			// preserved identity, or the record is only right in memory.
			if rec.accountUpdate == nil || rec.accountUpdate.ID != accountA || rec.accountUpdate.CheckedAt == nil {
				t.Errorf("accountUpdate = %+v, want the preserved identity", rec.accountUpdate)
			}
		})
	}
}

// The other direction, so the distinction is not one-way: a DEFINITIVE answer
// still disowns. Without both halves this guard would pass by never dropping
// anything.
func TestDefinitiveProfileAnswerStillDisownsTheAccount(t *testing.T) {
	for _, tc := range []struct {
		name string
		fn   func(*http.Request) (*http.Response, error)
	}{
		{"bearer refused", status(http.StatusUnauthorized)},
		{"bearer not entitled", status(http.StatusForbidden)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, _ := verifiedAnthropicRecord(t)
			was := rec.Fingerprint

			identifyRefreshedAnthropicAccount(t.Context(), profileClient(tc.fn), rec, []byte("rotated-payload"), "opaque-token")

			if rec.AccountID != "" || rec.AccountEmail != "" || rec.AccountCheckedAt != nil {
				t.Errorf("a refused bearer kept an identity it cannot prove: %+v", rec)
			}
			if rec.Fingerprint == was || IsAccountFingerprint(rec.Fingerprint) {
				t.Errorf("fingerprint = %q, want a local unverified meter", rec.Fingerprint)
			}
			if rec.Fingerprint != SubscriptionFingerprint(OAuthKindClaudeCode, []byte("rotated-payload")) {
				t.Errorf("fingerprint = %q, want the subscription key for the rotated payload", rec.Fingerprint)
			}
		})
	}
}

// And a 200 naming a DIFFERENT account re-stamps rather than preserving —
// that is the answer the whole check exists for.
func TestProfileNamingAnotherAccountRestamps(t *testing.T) {
	rec, _ := verifiedAnthropicRecord(t)
	other := OAuthAccount{ID: accountB, OrganizationID: organizationA, Email: "other@example.org"}

	identifyRefreshedAnthropicAccount(t.Context(), profileClient(func(*http.Request) (*http.Response, error) {
		body := fmt.Sprintf(`{"account":{"uuid":%q,"email":%q},"organization":{"uuid":%q}}`, other.ID, other.Email, other.OrganizationID)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	}), rec, []byte("rotated-payload"), "opaque-token")

	if rec.AccountID != accountB || rec.Fingerprint != other.Fingerprint() {
		t.Fatalf("a bearer that names another account must re-stamp: id=%q fp=%q", rec.AccountID, rec.Fingerprint)
	}
	if rec.AccountError != "" || rec.AccountCheckedAt == nil {
		t.Fatalf("a confirmed check must clear the error and date itself: err=%q checked=%v", rec.AccountError, rec.AccountCheckedAt)
	}
}

// A credential with no user:profile scope is definitive too: no retry can
// identify it, so it must not keep an identity it can no longer prove.
func TestMissingProfileScopeDisowns(t *testing.T) {
	rec, _ := verifiedAnthropicRecord(t)
	rec.Scopes = []string{"user:inference"}

	identifyRefreshedAnthropicAccount(t.Context(), profileClient(status(http.StatusInternalServerError)), rec, []byte("rotated-payload"), "opaque-token")

	if rec.AccountID != "" || IsAccountFingerprint(rec.Fingerprint) {
		t.Fatalf("a scopeless credential kept a verified identity: id=%q fp=%q", rec.AccountID, rec.Fingerprint)
	}
	if !strings.Contains(rec.AccountError, "user:profile") {
		t.Fatalf("the reason must name the missing scope, got %q", rec.AccountError)
	}
}
