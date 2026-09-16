package secrets

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

const defaultAnthropicProfileURL = "https://api.anthropic.com/api/oauth/profile"
const accountFingerprintPrefix = "account:anthropic:"

// anthropicProfileURL is the last leg of the same override family as the
// authorize, token, redirect and scope endpoints — and the one that carries a
// BEARER outbound.
//
// Hardcoded, it contradicted anthropicTokenURL's own promise that an override
// "moves the whole flow, not three quarters of it": a deployment pointing the
// token endpoint at a gateway or a test double still shipped the token that
// gateway minted to api.anthropic.com, on every connect and every refresh —
// credential egress to a host the operator had deliberately steered away from,
// and an outbound call per record that offline and CI environments cannot mock.
func anthropicProfileURL() string {
	return envOr("ITERION_OAUTH_FORFAIT_ANTHROPIC_PROFILE_URL", defaultAnthropicProfileURL)
}

// ErrAccountLookupUnavailable marks a profile lookup that could not be
// PERFORMED — a transport failure, a 429, a 5xx, a body no parser
// recognises. It disproves nothing, which is the point of naming it: only
// a definitive answer (a bearer the provider refuses, or a 200 naming a
// different account) is evidence about whose subscription this is.
//
// It exists because a refreshed record carries two facts the failure path
// used to collapse into one. Whether the identity is CONFIRMED is a
// verification fact, and a rotated bearer whose profile could not be read
// has indeed not confirmed it. Which meter the spend belongs to is a
// CONTINUITY fact, and a 503 says nothing about the subscription having
// changed. usagecap.Key reads the fingerprint ALONE — never AccountID — so
// re-minting SubscriptionFingerprint on an unavailable lookup files the
// next readings under the hash of a payload the next refresh rotates away,
// and the closed-window skip stops protecting that forfait in silence.
//
// The text is terse because it composes into AccountError, which an
// operator reads: "anthropic profile: HTTP 503: lookup unavailable".
var ErrAccountLookupUnavailable = errors.New("lookup unavailable")

// OAuthAccount is identity returned by the provider, never inferred from an
// operator label or a reset time. An account can have seats in several provider
// organizations, so both UUIDs identify the subscription's meter.
type OAuthAccount struct {
	ID             string
	OrganizationID string
	Email          string
}

// Fingerprint is independent of bearer-token rotation and of the Iterion tier
// holding the credential. Full SHA-256 is used because this key crosses tiers.
func (a OAuthAccount) Fingerprint() string {
	account, aerr := uuid.Parse(a.ID)
	org, oerr := uuid.Parse(a.OrganizationID)
	if aerr != nil || oerr != nil || account == uuid.Nil || org == uuid.Nil {
		return ""
	}
	sum := sha256.Sum256([]byte("anthropic\x00" + account.String() + "\x00" + org.String()))
	return accountFingerprintPrefix + hex.EncodeToString(sum[:])
}

// IsAccountFingerprint distinguishes provider-verified account keys from
// historical blob hashes. Only the former may share a window across tenants.
// The key is an audit identity, not evidence of credential ownership.
func IsAccountFingerprint(fp string) bool {
	if !strings.HasPrefix(fp, accountFingerprintPrefix) {
		return false
	}
	h := strings.TrimPrefix(fp, accountFingerprintPrefix)
	if len(h) != sha256.Size*2 || h != strings.ToLower(h) {
		return false
	}
	_, err := hex.DecodeString(h)
	return err == nil
}

// DiscoverAnthropicAccount calls the same profile endpoint as Claude Code.
// It is a bounded read, with no redirect, refresh, or untrusted response body
// in its errors. The caller preserves the credential when lookup is unavailable.
func DiscoverAnthropicAccount(ctx context.Context, hc *http.Client, token string) (OAuthAccount, error) {
	if err := ValidateTokenShape("access token", token); err != nil || token == "" {
		return OAuthAccount{}, fmt.Errorf("anthropic profile: invalid access token")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if hc == nil {
		hc = http.DefaultClient
	}
	client := *hc
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, anthropicProfileURL(), nil)
	if err != nil {
		return OAuthAccount{}, fmt.Errorf("anthropic profile: request construction failed: %w", ErrAccountLookupUnavailable)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return OAuthAccount{}, fmt.Errorf("anthropic profile: request failed: %w", ErrAccountLookupUnavailable)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// 401/403 is the provider refusing THIS bearer — the only status
		// that says something about the credential. Every other one (429,
		// any 5xx, an endpoint that moved) is the lookup being unavailable.
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return OAuthAccount{}, fmt.Errorf("anthropic profile: HTTP %d", resp.StatusCode)
		}
		return OAuthAccount{}, fmt.Errorf("anthropic profile: HTTP %d: %w", resp.StatusCode, ErrAccountLookupUnavailable)
	}
	const maxProfileBytes = 64 * 1024
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxProfileBytes+1))
	if err != nil || len(body) > maxProfileBytes {
		return OAuthAccount{}, fmt.Errorf("anthropic profile: unreadable or oversized response: %w", ErrAccountLookupUnavailable)
	}
	var profile struct {
		Account struct {
			UUID  string `json:"uuid"`
			Email string `json:"email"`
		} `json:"account"`
		Organization struct {
			UUID string `json:"uuid"`
		} `json:"organization"`
	}
	if err := json.Unmarshal(body, &profile); err != nil {
		// A captive portal, or an error envelope served with 200. Neither is
		// the provider saying anything about this credential.
		return OAuthAccount{}, fmt.Errorf("anthropic profile: malformed response: %w", ErrAccountLookupUnavailable)
	}
	a := OAuthAccount{ID: profile.Account.UUID, OrganizationID: profile.Organization.UUID, Email: strings.TrimSpace(profile.Account.Email)}
	addr, err := mail.ParseAddress(a.Email)
	if a.Fingerprint() == "" || err != nil || addr.Address != a.Email || len(a.Email) > 320 || strings.ContainsAny(a.Email, "\r\n\t") {
		// Parsed, but naming no usable identity — a shape this code does not
		// recognise, not a statement that another account owns the bearer.
		return OAuthAccount{}, fmt.Errorf("anthropic profile: missing or invalid account identity: %w", ErrAccountLookupUnavailable)
	}
	return a, nil
}

// identifyRefreshedAnthropicAccount checks the bearer actually returned by
// refresh. Pasted access/refresh tokens need not name the same account, and an
// endpoint override is not proof of that relationship either. A failed lookup
// must preserve rotated tokens but cannot assert the previous bearer identity.
func identifyRefreshedAnthropicAccount(ctx context.Context, hc *http.Client, rec *OAuthRecord, payload []byte, token string) {
	rec.accountUpdate = &OAuthAccountUpdate{PreviousEmail: rec.AccountEmail}
	defer func() {
		rec.accountUpdate.ID, rec.accountUpdate.OrganizationID, rec.accountUpdate.Email = rec.AccountID, rec.AccountOrganizationID, rec.AccountEmail
		rec.accountUpdate.CheckedAt, rec.accountUpdate.Error = rec.AccountCheckedAt, rec.AccountError
	}()

	// The identity claim always drops: this bearer is not the one that was
	// verified, and nothing below re-asserts it without proof.
	rec.AccountID, rec.AccountOrganizationID, rec.AccountEmail = "", "", ""
	rec.AccountCheckedAt = nil

	if !slices.Contains(rec.Scopes, "user:profile") {
		// Definitive, and no retry changes it: this bearer cannot be
		// identified at all, so its meter returns to a local one too.
		rec.AccountError = "Profile unavailable: this credential has no user:profile scope; reconnect through the browser to identify the account."
		demoteToLocalMeter(rec, payload)
		return
	}

	account, err := DiscoverAnthropicAccount(ctx, hc, token)
	switch {
	case err == nil:
		now := time.Now().UTC()
		rec.AccountID, rec.AccountOrganizationID, rec.AccountEmail = account.ID, account.OrganizationID, account.Email
		rec.AccountCheckedAt, rec.AccountError = &now, ""
		rec.Fingerprint = account.Fingerprint()
	case errors.Is(err, ErrAccountLookupUnavailable):
		// The claim is dropped above, as the posture requires — but the
		// METER is a different fact, and this outcome disproved nothing
		// about it. Keeping the fingerprint is what lets the readings keep
		// accumulating under the key they were filed under; minting a new
		// SubscriptionFingerprint here would move them to the hash of a
		// payload the next refresh rotates away, which no reader can find.
		rec.AccountError = err.Error()
		// The operator-visible LABEL is a third fact, and it must not follow
		// either. PreviousEmail exists to rewrite an email-derived label when
		// the address genuinely changed; left set here it rewrites the label
		// to the empty Email this branch just cleared — and the next
		// successful refresh carries PreviousEmail == "", so the condition
		// never fires again and the name never comes back. One unreachable
		// provider would leave the connection permanently unnamed in Studio.
		//
		// Clearing PreviousEmail rather than restoring the old address into
		// the update is what keeps this scoped: putting the address back
		// would re-assert the identity the posture above just dropped.
		rec.accountUpdate.PreviousEmail = ""
	default:
		// The provider answered about THIS bearer: refused it, or named
		// another account. Either way the meter must not follow.
		rec.AccountError = err.Error()
		demoteToLocalMeter(rec, payload)
	}
}

// demoteToLocalMeter returns a credential whose account was DEFINITIVELY
// disproved to a local, unverified meter. Reserved for that case: it costs
// the account fingerprint, which is the shared, rotation-stable key every
// tier's usage readings are filed under (usagecap.Key promotes the scope to
// "account" on it alone), and replaces it with the hash of one payload.
func demoteToLocalMeter(rec *OAuthRecord, payload []byte) {
	if IsAccountFingerprint(rec.Fingerprint) {
		rec.Fingerprint = SubscriptionFingerprint(rec.Kind, payload)
	}
}
