package secrets

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
		return OAuthAccount{}, fmt.Errorf("anthropic profile: request construction failed")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return OAuthAccount{}, fmt.Errorf("anthropic profile: request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return OAuthAccount{}, fmt.Errorf("anthropic profile: HTTP %d", resp.StatusCode)
	}
	const maxProfileBytes = 64 * 1024
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxProfileBytes+1))
	if err != nil || len(body) > maxProfileBytes {
		return OAuthAccount{}, fmt.Errorf("anthropic profile: unreadable or oversized response")
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
		return OAuthAccount{}, fmt.Errorf("anthropic profile: malformed response")
	}
	a := OAuthAccount{ID: profile.Account.UUID, OrganizationID: profile.Organization.UUID, Email: strings.TrimSpace(profile.Account.Email)}
	addr, err := mail.ParseAddress(a.Email)
	if a.Fingerprint() == "" || err != nil || addr.Address != a.Email || len(a.Email) > 320 || strings.ContainsAny(a.Email, "\r\n\t") {
		return OAuthAccount{}, fmt.Errorf("anthropic profile: missing or invalid account identity")
	}
	return a, nil
}

// identifyRefreshedAnthropicAccount checks the bearer actually returned by
// refresh. Pasted access/refresh tokens need not name the same account, and an
// endpoint override is not proof of that relationship either. A failed lookup
// must preserve rotated tokens but cannot assert the previous bearer identity.
func identifyRefreshedAnthropicAccount(ctx context.Context, hc *http.Client, rec *OAuthRecord, payload []byte, token string) {
	previousEmail := rec.AccountEmail
	rec.accountUpdate = &OAuthAccountUpdate{PreviousEmail: previousEmail}
	rec.AccountID, rec.AccountOrganizationID, rec.AccountEmail = "", "", ""
	rec.AccountCheckedAt = nil
	rec.AccountError = "Profile unavailable: this credential has no user:profile scope; reconnect through the browser to identify the account."
	if slices.Contains(rec.Scopes, "user:profile") {
		account, err := DiscoverAnthropicAccount(ctx, hc, token)
		if err == nil {
			now := time.Now().UTC()
			rec.AccountID, rec.AccountOrganizationID, rec.AccountEmail = account.ID, account.OrganizationID, account.Email
			rec.AccountCheckedAt, rec.AccountError = &now, ""
			rec.Fingerprint = account.Fingerprint()
		} else {
			rec.AccountError = err.Error()
		}
	}
	if rec.AccountID == "" && IsAccountFingerprint(rec.Fingerprint) {
		// Without proof, this bearer returns to a local, unverified meter.
		rec.Fingerprint = SubscriptionFingerprint(rec.Kind, payload)
	}
	rec.accountUpdate.ID, rec.accountUpdate.OrganizationID, rec.accountUpdate.Email = rec.AccountID, rec.AccountOrganizationID, rec.AccountEmail
	rec.accountUpdate.CheckedAt, rec.accountUpdate.Error = rec.AccountCheckedAt, rec.AccountError
}
