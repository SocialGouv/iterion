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

const anthropicProfileURL = "https://api.anthropic.com/api/oauth/profile"
const accountFingerprintPrefix = "account:anthropic:"

// ErrAccountLookupUnavailable marks a profile lookup that could not be
// performed — a transport failure, a 429, a 5xx, a response no parser
// recognises. It says nothing about the credential, which is the whole
// point of naming it: only a DEFINITIVE answer (a 200 identifying a
// different account, or a bearer the provider refuses outright) is
// evidence that a previously verified identity no longer holds.
//
// Without the distinction, a five-second blip at api.anthropic.com during
// a routine refresh erases a verified account and re-keys the credential
// to SubscriptionFingerprint — the whole-blob hash of the just-rotated
// payload. That hash is per-credential and per-rotation, where the account
// fingerprint is shared and stable, so usagecap.Key stops resolving to the
// account scope, Latest returns no readings, and the closed-window skip
// silently stops protecting that forfait until some later lookup happens
// to succeed.
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, anthropicProfileURL, nil)
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
		// 401/403 is the provider refusing THIS bearer — the one answer that
		// disproves a previous identity claim. Every other status (429, any
		// 5xx, and an endpoint that moved) is the lookup being unavailable,
		// not the credential being wrong.
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
		// A captive portal or an error envelope served with 200 lands here.
		// Neither is the provider saying anything about this credential.
		return OAuthAccount{}, fmt.Errorf("anthropic profile: malformed response: %w", ErrAccountLookupUnavailable)
	}
	a := OAuthAccount{ID: profile.Account.UUID, OrganizationID: profile.Organization.UUID, Email: strings.TrimSpace(profile.Account.Email)}
	addr, err := mail.ParseAddress(a.Email)
	if a.Fingerprint() == "" || err != nil || addr.Address != a.Email || len(a.Email) > 320 || strings.ContainsAny(a.Email, "\r\n\t") {
		// Parsed, but it names no usable identity — a shape this code does
		// not recognise, not a statement that the old identity is wrong.
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

	if !slices.Contains(rec.Scopes, "user:profile") {
		// Definitive, and no number of retries changes it: this bearer
		// cannot be identified at all, so an identity it can no longer
		// prove has to go.
		disownAnthropicAccount(rec, payload, "Profile unavailable: this credential has no user:profile scope; reconnect through the browser to identify the account.")
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
		// Nothing was learned, so nothing is unlearned: the identity and its
		// fingerprint stay. AccountCheckedAt deliberately stays at the last
		// CONFIRMED check — advancing it here would date a verification that
		// did not happen. The error is still recorded, so a run of failures
		// is visible instead of silent.
		rec.AccountError = err.Error()
	default:
		disownAnthropicAccount(rec, payload, err.Error())
	}
}

// disownAnthropicAccount drops an identity this credential can no longer
// prove and returns it to a local, unverified meter. Reserved for a
// DEFINITIVE answer: it costs the account fingerprint, which is the shared,
// rotation-stable key every tier's usage readings are filed under, and
// SubscriptionFingerprint replaces it with the hash of one payload that the
// next refresh rotates away.
func disownAnthropicAccount(rec *OAuthRecord, payload []byte, reason string) {
	rec.AccountID, rec.AccountOrganizationID, rec.AccountEmail = "", "", ""
	rec.AccountCheckedAt = nil
	rec.AccountError = reason
	if IsAccountFingerprint(rec.Fingerprint) {
		rec.Fingerprint = SubscriptionFingerprint(rec.Kind, payload)
	}
}
