package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// AppManifest is the GitHub App manifest iterion POSTs to
// <web>/settings/apps/new — the only programmatic path to create a GitHub app
// (there is no create-OAuth-app REST endpoint). The created App's
// client_id/client_secret then drive the existing OAuth user-to-server connect
// flow (OAuthApp).
type AppManifest struct {
	Name        string `json:"name"`
	URL         string `json:"url"`
	RedirectURL string `json:"redirect_url"`
	// CallbackURLs are the user-authorization (OAuth) callback URLs baked into
	// the created App. WITHOUT this the subsequent "connect via OAuth" step fails
	// with GitHub's "This GitHub App must be configured with a callback URL" —
	// RedirectURL above only covers the one-shot manifest-conversion redirect,
	// not the recurring user-to-server OAuth the connect flow uses.
	CallbackURLs       []string          `json:"callback_urls"`
	Public             bool              `json:"public"`
	DefaultEvents      []string          `json:"default_events"`
	DefaultPermissions map[string]string `json:"default_permissions"`
	HookAttributes     map[string]any    `json:"hook_attributes"`
}

// BuildAppManifest assembles the manifest for an iterion forge OAuth app. The
// permissions are what a user-to-server token needs to manage repo webhooks
// (administration) and run a PR-review bot (pull_requests, contents); the
// App-level webhook is disabled (iterion creates per-repo hooks itself).
func BuildAppManifest(name, homeURL, redirectURL string) AppManifest {
	return AppManifest{
		Name:        name,
		URL:         homeURL,
		RedirectURL: redirectURL,
		// The connect flow authorizes the user against this App at
		// {home}/api/forge/oauth/callback (see GET /api/forge/oauth/callback).
		CallbackURLs:  []string{homeURL + "/api/forge/oauth/callback"},
		Public:        false,
		DefaultEvents: []string{},
		DefaultPermissions: map[string]string{
			"administration": "write", // create repo webhooks
			"contents":       "read",  // clone + read the diff
			"pull_requests":  "write", // post the review
			"metadata":       "read",  // mandatory baseline
		},
		HookAttributes: map[string]any{"url": homeURL, "active": false},
	}
}

// ManifestConversion is the subset of GitHub's app-manifest conversion
// response iterion keeps: the App id + slug + owner (to deep-link its settings)
// and the OAuth client credentials.
type ManifestConversion struct {
	ID           int64  `json:"id"`
	Slug         string `json:"slug"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	Owner        struct {
		Login string `json:"login"`
		Type  string `json:"type"` // "Organization" | "User"
	} `json:"owner"`
}

// AppManageURL is the GitHub settings page (Advanced tab, with the Delete
// button) for a created App, so the operator can remove it on the forge —
// GitHub exposes no API to delete an App. Empty when the slug is unknown.
func AppManageURL(webBase, ownerLogin, ownerType, slug string) string {
	if slug == "" {
		return ""
	}
	base := strings.TrimRight(webBase, "/")
	if base == "" {
		base = "https://github.com"
	}
	if strings.EqualFold(ownerType, "Organization") && ownerLogin != "" {
		return base + "/organizations/" + ownerLogin + "/settings/apps/" + slug + "/advanced"
	}
	return base + "/settings/apps/" + slug + "/advanced"
}

// ConvertManifest exchanges the temporary code GitHub returns after the
// operator confirms the manifest for the created App's credentials, via
// POST {apiBase}/app-manifests/{code}/conversions. The code is single-use and
// expires in ~1h; no auth header is needed (the code is the credential).
func ConvertManifest(ctx context.Context, httpClient *http.Client, webBase, code string) (ManifestConversion, error) {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	apiBase := APIBaseFor(webBase)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiBase+"/app-manifests/"+code+"/conversions", nil)
	if err != nil {
		return ManifestConversion{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := httpClient.Do(req)
	if err != nil {
		return ManifestConversion{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		if resp.StatusCode == http.StatusUnprocessableEntity || resp.StatusCode == http.StatusNotFound {
			return ManifestConversion{}, fmt.Errorf("github: manifest code invalid or expired (HTTP %d)", resp.StatusCode)
		}
		return ManifestConversion{}, fmt.Errorf("github: convert manifest: HTTP %d", resp.StatusCode)
	}
	var out ManifestConversion
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return ManifestConversion{}, fmt.Errorf("github: decode manifest conversion: %w", err)
	}
	if out.ClientID == "" || out.ClientSecret == "" {
		return ManifestConversion{}, fmt.Errorf("github: manifest conversion returned no client credentials")
	}
	return out, nil
}
