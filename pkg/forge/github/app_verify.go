package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// ErrInstallationNotOwned is returned by VerifyInstallationOwnership when the
// authenticated user provably does NOT have access to the installation — the
// signal the install callback maps to a 403 (an attacker substituting another
// org's installation_id).
var ErrInstallationNotOwned = fmt.Errorf("github: installation not accessible to the authorizing user")

// VerifyInstallationOwnership proves the user completing a GitHub-App install
// callback actually has access to the installation they claim, closing the
// IDOR where installation_id (an enumerable integer taken verbatim from the
// callback URL) is trusted without a check. It:
//
//  1. exchanges the user-authorization `code` for a user-to-server token
//     (the App must have "Request user authorization (OAuth) during
//     installation" enabled, so GitHub appends `code` to the setup redirect);
//  2. lists the installations that token can see (GET /user/installations);
//  3. returns nil only when installationID is among them.
//
// A user cannot mint a code for an installation they don't control, and
// /user/installations only returns installations the user can access, so a
// forged installation_id fails at step 3. Returns ErrInstallationNotOwned when
// the installation is absent, or a wrapped error on exchange/API failure
// (callers fail closed on any error).
func VerifyInstallationOwnership(ctx context.Context, httpClient *http.Client, webBase string, cfg AppConfig, code string, installationID int64) error {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if code == "" {
		return fmt.Errorf("github: missing user-authorization code (enable 'Request user authorization during installation' on the App)")
	}
	if !cfg.UserAuthConfigured() {
		return fmt.Errorf("github: app has no client_id/client_secret configured for user-auth verification")
	}
	token, err := exchangeUserAuthCode(ctx, httpClient, webBase, cfg, code)
	if err != nil {
		return err
	}
	return userCanAccessInstallation(ctx, httpClient, APIBaseFor(webBase), token, installationID)
}

// exchangeUserAuthCode trades the user-authorization code for a user-to-server
// access token. No redirect_uri is sent: GitHub-App user-auth token exchange
// does not require it, and omitting it avoids a mismatch with the App's
// configured callback URL.
func exchangeUserAuthCode(ctx context.Context, httpClient *http.Client, webBase string, cfg AppConfig, code string) (string, error) {
	v := url.Values{}
	v.Set("client_id", cfg.ClientID)
	v.Set("client_secret", cfg.ClientSecret)
	v.Set("code", code)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(webBase, "/")+"/login/oauth/access_token", strings.NewReader(v.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))

	var tr struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	uErr := json.Unmarshal(raw, &tr)
	// GitHub returns 200 with an `error` field on a failed exchange.
	if tr.Error != "" {
		return "", fmt.Errorf("github: user-auth token exchange: %s", tr.Error)
	}
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("github: user-auth token endpoint: HTTP %d", resp.StatusCode)
	}
	if uErr != nil {
		return "", fmt.Errorf("github: user-auth token endpoint returned a non-JSON body (HTTP %d): %w", resp.StatusCode, uErr)
	}
	if tr.AccessToken == "" {
		return "", fmt.Errorf("github: user-auth token endpoint returned no access_token")
	}
	return tr.AccessToken, nil
}

// userCanAccessInstallation pages GET /user/installations with the user token
// and returns nil when installationID appears, ErrInstallationNotOwned when it
// does not, or a wrapped error on an API failure. Pagination is bounded so a
// pathological account can't spin the request forever; a real user has a
// handful of app installations.
func userCanAccessInstallation(ctx context.Context, httpClient *http.Client, apiBase, token string, installationID int64) error {
	const perPage = 100
	const maxPages = 20 // 2000 installations — far beyond any real account
	for page := 1; page <= maxPages; page++ {
		u := fmt.Sprintf("%s/user/installations?per_page=%d&page=%d", strings.TrimRight(apiBase, "/"), perPage, page)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		resp, err := httpClient.Do(req)
		if err != nil {
			return err
		}
		var out struct {
			TotalCount    int `json:"total_count"`
			Installations []struct {
				ID int64 `json:"id"`
			} `json:"installations"`
		}
		dErr := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out)
		resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			return fmt.Errorf("github: GET /user/installations: HTTP %d", resp.StatusCode)
		}
		if dErr != nil {
			return fmt.Errorf("github: decode /user/installations: %w", dErr)
		}
		for _, inst := range out.Installations {
			if inst.ID == installationID {
				return nil
			}
		}
		// Stop when the last page has been consumed.
		if len(out.Installations) < perPage || page*perPage >= out.TotalCount {
			break
		}
	}
	return ErrInstallationNotOwned
}
