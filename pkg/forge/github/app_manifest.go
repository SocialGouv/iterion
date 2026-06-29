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
	CallbackURLs []string `json:"callback_urls"`
	// SetupURL is where GitHub redirects AFTER the user installs the App on
	// repos — without it GitHub stays put and iterion never sees the
	// installation, so the github_app connection is never created. It points at
	// the install callback, which the install flow's state flows through to.
	SetupURL           string            `json:"setup_url"`
	SetupOnUpdate      bool              `json:"setup_on_update"`
	Public             bool              `json:"public"`
	DefaultEvents      []string          `json:"default_events"`
	DefaultPermissions map[string]string `json:"default_permissions"`
	HookAttributes     map[string]any    `json:"hook_attributes"`
}

// BuildAppManifest assembles the manifest for an iterion forge GitHub App. The
// permissions are the LEAST-PRIVILEGE set iterion's forge layer actually needs:
// push/read code (contents), open + comment on PRs (pull_requests), the
// mandatory metadata baseline, and manage the per-repo inbound webhook
// (repository_hooks — the App-level webhook is disabled, iterion creates per-repo
// hooks itself). It deliberately does NOT request `administration` (repo
// deletion / settings / teams / branch-protection): that grant is dangerous AND
// wrong for a GitHub App installation token — per GitHub docs repo webhooks
// require `repository_hooks:write`, not `administration`.
func BuildAppManifest(name, homeURL, redirectURL string) AppManifest {
	return AppManifest{
		Name:        name,
		URL:         homeURL,
		RedirectURL: redirectURL,
		// The connect flow authorizes the user against this App at
		// {home}/api/forge/oauth/callback (see GET /api/forge/oauth/callback).
		CallbackURLs: []string{homeURL + "/api/forge/oauth/callback"},
		// After install, GitHub redirects here with installation_id (+ the
		// install flow's state), so iterion creates the github_app connection.
		SetupURL:      homeURL + "/api/forge/github/app/callback",
		SetupOnUpdate: true,
		Public:        false,
		DefaultEvents: []string{},
		DefaultPermissions: map[string]string{
			"contents":         "write", // clone + read the diff AND push branches/commits
			"pull_requests":    "write", // open PRs + post review comments
			"metadata":         "read",  // mandatory baseline
			"repository_hooks": "write", // auto-provision the per-repo inbound webhook
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
	// PEM is the App's private key — the credential the least-privilege
	// github_app (installation-token) path needs. WebhookSecret is the App's
	// generated webhook secret. Both are returned once by the conversion and
	// must be captured here (GitHub won't re-issue the private key).
	PEM           string `json:"pem"`
	WebhookSecret string `json:"webhook_secret"`
	Owner         struct {
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
