package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/secrets"
)

type accountProfileTransport func(*http.Request) (*http.Response, error)

func (f accountProfileTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func accountBlob(token string) string {
	return fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"expiresAt":%d,"scopes":["user:profile","user:inference"]}}`, token, time.Now().Add(time.Hour).UnixMilli())
}

func TestOAuthAccountCorrelationStaysWithinVisibleOwner(t *testing.T) {
	_, hs, signer, st := oauthTestServer(t)
	fp := (secrets.OAuthAccount{ID: "c7f30bce-7e8b-4bb7-8199-afc172f3d580", OrganizationID: "0d05ce64-6b67-4368-909c-0d26f5a5870a"}).Fingerprint()
	for _, seed := range []struct {
		owner string
		kind  secrets.OAuthKind
		rank  int
	}{
		{"viewer", secrets.OAuthKindClaudeCode, 0},
		{"viewer", secrets.OAuthKindClaudeCode, 2},
		{"other-owner", secrets.OAuthKindClaudeCode, 7},
		{"viewer", secrets.OAuthKindCodex, 5},
	} {
		if err := st.Upsert(t.Context(), secrets.OAuthRecord{
			ID: secrets.OAuthRecordID(seed.owner, seed.kind, seed.rank), UserID: seed.owner,
			Kind: seed.kind, Rank: seed.rank, Fingerprint: fp,
		}); err != nil {
			t.Fatal(err)
		}
	}
	status, body := oauthCall(t, hs, "GET", "/api/me/oauth/connections", oauthJWT(t, signer, "viewer"), "")
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, body)
	}
	var result struct {
		Connections []oauthConnectionView `json:"connections"`
	}
	if err := json.Unmarshal([]byte(body), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Connections) != 3 {
		t.Fatalf("visible connections=%d", len(result.Connections))
	}
	for _, view := range result.Connections {
		var want []int
		if view.Kind == string(secrets.OAuthKindClaudeCode) {
			want = []int{2}
			if view.Rank == 2 {
				want = []int{0}
			}
		}
		if !reflect.DeepEqual(view.SameAccountRanks, want) {
			t.Fatalf("kind=%s rank=%d correlations=%v want=%v", view.Kind, view.Rank, view.SameAccountRanks, want)
		}
	}
}

func TestOAuthVerifiedAccountReconnectAndSwap(t *testing.T) {
	s, hs, signer, st := oauthTestServer(t)
	status := 200
	account := "c7f30bce-7e8b-4bb7-8199-afc172f3d580"
	email := "verified@example.org"
	s.httpClient = &http.Client{Transport: accountProfileTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://api.anthropic.com/api/oauth/profile" {
			t.Fatalf("unexpected URL %s", r.URL)
		}
		body := fmt.Sprintf(`{"account":{"uuid":%q,"email":%q},"organization":{"uuid":"0d05ce64-6b67-4368-909c-0d26f5a5870a"}}`, account, email)
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	connect := func(owner, token string, rank int) oauthConnectionView {
		t.Helper()
		code, body := oauthCall(t, hs, "POST", fmt.Sprintf("/api/me/oauth/claude_code/credentials?rank=%d", rank), oauthJWT(t, signer, owner), accountBlob(token))
		if code != 200 {
			t.Fatalf("connect status=%d body=%s", code, body)
		}
		var view oauthConnectionView
		if err := json.Unmarshal([]byte(body), &view); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(body, account) || strings.Contains(body, token) {
			t.Fatal("public view exposed provider UUID or token")
		}
		return view
	}
	first := connect("owner-a", "token-one", 0)
	if !first.AccountVerified || first.AccountLabel != "verified@example.org" || first.AccountEmail != first.AccountLabel || first.AccountCheckedAt == nil {
		t.Fatalf("first=%+v", first)
	}
	email = "renamed@example.org"
	updated := connect("owner-a", "token-new-email", 0)
	if updated.Fingerprint != first.Fingerprint || updated.AccountLabel != email || updated.AccountEmail != email {
		t.Fatalf("derived label did not follow verified email: %+v", updated)
	}
	if err := st.SetAccountLabel(t.Context(), secrets.OAuthRecordID("owner-a", secrets.OAuthKindClaudeCode, 0), "my name"); err != nil {
		t.Fatal(err)
	}
	rotated := connect("owner-a", "token-two", 0)
	otherOwner := connect("owner-b", "token-three", 2)
	if rotated.Fingerprint != first.Fingerprint || otherOwner.Fingerprint != first.Fingerprint || rotated.AccountLabel != "my name" {
		t.Fatalf("rotation/cross-owner identity changed: first=%+v rotated=%+v other=%+v", first, rotated, otherOwner)
	}
	status = 403
	sameToken := connect("owner-a", "token-two", 0)
	if !sameToken.AccountVerified || sameToken.Fingerprint != first.Fingerprint || sameToken.AccountError == "" {
		t.Fatalf("same bearer lost known identity: %+v", sameToken)
	}
	unknown := connect("owner-a", "different-unidentified-token", 0)
	if unknown.AccountVerified || unknown.AccountEmail != "" || unknown.AccountLabel != "" || unknown.AccountError == "" || unknown.Fingerprint == first.Fingerprint {
		t.Fatalf("unknown replacement inherited prior identity: %+v", unknown)
	}
	stored, _ := st.Get(t.Context(), "owner-a", secrets.OAuthKindClaudeCode)
	if stored.AccountID != "" || stored.AccountOrganizationID != "" || stored.AccountCheckedAt != nil {
		t.Fatal("account swap retained private profile metadata")
	}
}

func TestOAuthRefreshIdentifiesTheReturnedBearer(t *testing.T) {
	const accountA = "c7f30bce-7e8b-4bb7-8199-afc172f3d580"
	const accountB = "ce6b5bc9-11a8-477b-bd39-9c934d8c65f4"
	const org = "0d05ce64-6b67-4368-909c-0d26f5a5870a"
	const tokenA = "sk-ant-oat01-fixture-account-a-abcdefghijklmnop"
	const nextToken = "sk-ant-oat01-fixture-returned-bearer-abcdefghijklmnop"
	for _, tc := range []struct {
		name                             string
		sameAccount, unavailable, rename bool
	}{
		{name: "different account"}, {name: "same account rotation", sameAccount: true},
		{name: "profile unavailable", unavailable: true}, {name: "different account with concurrent rename", rename: true},
		{name: "profile unavailable with concurrent rename", unavailable: true, rename: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, hs, signer, st := oauthTestServer(t)
			s.cfg.AnthropicOAuthClientID = "fixture-client"
			s.httpClient = &http.Client{Transport: accountProfileTransport(func(r *http.Request) (*http.Response, error) {
				var body string
				status := 200
				switch r.URL.Path {
				case "/api/oauth/profile":
					a, email := accountA, "account-a@example.invalid"
					if r.Header.Get("Authorization") == "Bearer "+nextToken {
						if tc.unavailable {
							status = 503
						} else if !tc.sameAccount {
							a, email = accountB, "account-b@example.invalid"
						}
					}
					body = fmt.Sprintf(`{"account":{"uuid":%q,"email":%q},"organization":{"uuid":%q}}`, a, email, org)
				case "/v1/oauth/token":
					if err := r.ParseForm(); err != nil {
						t.Fatal(err)
					}
					if r.Form.Get("refresh_token") != "refresh-initial" {
						t.Fatal("wrong refresh fixture")
					}
					if tc.rename {
						if err := st.SetAccountLabel(t.Context(), secrets.OAuthRecordID("owner", secrets.OAuthKindClaudeCode, 0), "operator renamed"); err != nil {
							t.Fatal(err)
						}
					}
					body = fmt.Sprintf(`{"access_token":%q,"refresh_token":"refresh-rotated","expires_in":3600,"scope":"user:profile user:inference"}`, nextToken)
				default:
					t.Fatalf("unexpected URL %s", r.URL)
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			blob := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"refreshToken":"refresh-initial","expiresAt":%d,"scopes":["user:profile","user:inference"]}}`, tokenA, time.Now().Add(time.Hour).UnixMilli())
			jwt := oauthJWT(t, signer, "owner")
			if code, body := oauthCall(t, hs, "POST", "/api/me/oauth/claude_code/credentials", jwt, blob); code != 200 {
				t.Fatalf("connect=%d %s", code, body)
			}
			if code, body := oauthCall(t, hs, "POST", "/api/me/oauth/claude_code/refresh", jwt, ""); code != 200 {
				t.Fatalf("refresh=%d %s", code, body)
			}
			rec, err := st.Get(t.Context(), "owner", secrets.OAuthKindClaudeCode)
			if err != nil {
				t.Fatal(err)
			}
			plain, err := secrets.OpenOAuthPayload(s.sealer, rec.UserID, rec.Kind, rec.SealedPayload)
			if err != nil {
				t.Fatal(err)
			}
			view, err := secrets.ParseAnthropicView(plain)
			if err != nil {
				t.Fatal(err)
			}
			if view.ClaudeAIOauth.AccessToken != nextToken || view.ClaudeAIOauth.RefreshToken != "refresh-rotated" {
				t.Fatal("rotated tokens were lost")
			}
			wantAccount, wantEmail := accountB, "account-b@example.invalid"
			if tc.sameAccount {
				wantAccount, wantEmail = accountA, "account-a@example.invalid"
			}
			if tc.unavailable {
				if secrets.IsAccountFingerprint(rec.Fingerprint) || rec.AccountID != "" || rec.AccountOrganizationID != "" || rec.AccountEmail != "" || rec.AccountCheckedAt != nil || rec.AccountError == "" {
					t.Fatal("failed profile lookup retained a verified identity")
				}
				wantEmail = ""
			} else {
				wantFP := (secrets.OAuthAccount{ID: wantAccount, OrganizationID: org}).Fingerprint()
				if rec.AccountID != wantAccount || rec.Fingerprint != wantFP || rec.AccountEmail != wantEmail || rec.AccountCheckedAt == nil || rec.AccountError != "" {
					t.Fatal("stored identity does not match the returned bearer")
				}
			}
			wantLabel := wantEmail
			if tc.rename {
				wantLabel = "operator renamed"
			}
			if rec.AccountLabel != wantLabel {
				t.Fatalf("label=%q want=%q", rec.AccountLabel, wantLabel)
			}
		})
	}
}

func TestOAuthViewHidesProfileFromLegacyWriterSwap(t *testing.T) {
	// During rollout the old Mongo Upsert $sets its known fields only. New
	// account_* fields therefore survive when an old server replaces the token.
	checked := time.Now()
	rec := secrets.OAuthRecord{Kind: secrets.OAuthKindClaudeCode, Fingerprint: "legacy-new-token-fingerprint", AccountID: "old-account", AccountEmail: "previous-owner@example.invalid", AccountLabel: "previous-owner@example.invalid", AccountCheckedAt: &checked}
	view := toOAuthView(rec)
	if view.AccountVerified || view.AccountEmail != "" || view.AccountCheckedAt != nil || view.AccountLabel != "" {
		t.Fatalf("unverified replacement displays old profile email=%s and checked_at=%v", view.AccountEmail, view.AccountCheckedAt)
	}
}

type reconnectBeforeClaimStore struct {
	secrets.OAuthStore
	before func(context.Context) error
}

func (s *reconnectBeforeClaimStore) ClaimRefresh(ctx context.Context, id, owner string, now, until time.Time) (bool, error) {
	if s.before != nil {
		f := s.before
		s.before = nil
		if err := f(ctx); err != nil {
			return false, err
		}
	}
	return s.OAuthStore.ClaimRefresh(ctx, id, owner, now, until)
}

func TestOAuthRefreshReconnectBetweenReadAndClaimKeepsNewAccount(t *testing.T) {
	s, hs, signer, st := oauthTestServer(t)
	s.cfg.AnthropicOAuthClientID = "fixture-client"
	const accountA = "c7f30bce-7e8b-4bb7-8199-afc172f3d580"
	const accountB = "ce6b5bc9-11a8-477b-bd39-9c934d8c65f4"
	const org = "0d05ce64-6b67-4368-909c-0d26f5a5870a"
	const tokenA = "sk-ant-oat01-fixture-account-a-abcdefghijklmnop"
	const tokenB = "sk-ant-oat01-fixture-account-b-abcdefghijklmnop"
	const tokenANext = "sk-ant-oat01-fixture-account-a-next-abcdefghijklmnop"
	exchanges := 0
	s.httpClient = &http.Client{Transport: accountProfileTransport(func(r *http.Request) (*http.Response, error) {
		var body string
		switch r.URL.Path {
		case "/api/oauth/profile":
			a, email := accountA, "account-a@example.invalid"
			if r.Header.Get("Authorization") == "Bearer "+tokenB {
				a, email = accountB, "account-b@example.invalid"
			}
			body = fmt.Sprintf(`{"account":{"uuid":%q,"email":%q},"organization":{"uuid":%q}}`, a, email, org)
		case "/v1/oauth/token":
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if r.Form.Get("refresh_token") != "refresh-a" {
				t.Fatal("wrong refresh fixture")
			}
			exchanges++
			body = fmt.Sprintf(`{"access_token":%q,"refresh_token":"refresh-a-next","expires_in":3600,"scope":"user:profile user:inference"}`, tokenANext)
		default:
			t.Fatalf("unexpected URL %s", r.URL)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	blob := func(token, refresh string) string {
		return fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"refreshToken":%q,"expiresAt":%d,"scopes":["user:profile","user:inference"]}}`, token, refresh, time.Now().Add(time.Hour).UnixMilli())
	}
	jwt := oauthJWT(t, signer, "owner")
	if code, body := oauthCall(t, hs, "POST", "/api/me/oauth/claude_code/credentials", jwt, blob(tokenA, "refresh-a")); code != 200 {
		t.Fatalf("connect A=%d %s", code, body)
	}
	s.oauthStore = &reconnectBeforeClaimStore{OAuthStore: st, before: func(ctx context.Context) error {
		// This goes through the ordinary connect endpoint: two valid pairs,
		// with B installed after the refresher read A but before it takes its claim.
		if code, body := oauthCall(t, hs, "POST", "/api/me/oauth/claude_code/credentials", jwt, blob(tokenB, "refresh-b")); code != 200 {
			return fmt.Errorf("connect B=%d %s", code, body)
		}
		return nil
	}}
	code, body := oauthCall(t, hs, "POST", "/api/me/oauth/claude_code/refresh", jwt, "")
	if code != 409 || exchanges != 0 {
		t.Fatalf("stale snapshot must not exchange: status=%d exchanges=%d body=%s", code, exchanges, body)
	}
	rec, err := st.Get(t.Context(), "owner", secrets.OAuthKindClaudeCode)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := secrets.OpenOAuthPayload(s.sealer, rec.UserID, rec.Kind, rec.SealedPayload)
	if err != nil {
		t.Fatal(err)
	}
	view, err := secrets.ParseAnthropicView(plain)
	if err != nil {
		t.Fatal(err)
	}
	if view.ClaudeAIOauth.AccessToken != tokenB {
		fpA := (secrets.OAuthAccount{ID: accountA, OrganizationID: org}).Fingerprint()
		t.Fatalf("reconnected B overwritten by stale refresh A: status=%d exchanges=%d payload_is_A=%v fingerprint_is_A=%v metadata_is_B=%v email=%s", code, exchanges, view.ClaudeAIOauth.AccessToken == tokenANext, rec.Fingerprint == fpA, rec.AccountID == accountB, rec.AccountEmail)
	}
}
