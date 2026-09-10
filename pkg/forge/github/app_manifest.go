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

// AppManifestOptions tunes the App's requested grant set beyond the
// least-privilege runtime baseline.
type AppManifestOptions struct {
	// AllowRepoCreation adds administration:write to the App's requested
	// permissions so iterion can CREATE repositories in the installed
	// org (the "new app → new repo" launch journey). Broad grant, hence
	// opt-in at App-creation time and surfaced in the connect UI; at run
	// time it is minted per CreateRepo call only
	// (RepoAdminInstallationPermissions) — the cached runtime token stays
	// on RuntimeInstallationPermissions.
	AllowRepoCreation bool
	// AllowAppDelivery adds workflows:write + packages:write so a bot can
	// publish the CI that builds an app and the image it produces — the
	// second half of the "new app → new repo → deployed" journey. Opt-in
	// because `workflows: write` lets the holder rewrite CI, i.e. run
	// arbitrary code in it.
	AllowAppDelivery bool
	// AllowSecurityRead adds vulnerability_alerts:read so iterion can list
	// the installation's Dependabot alerts org-wide (the vuln-watch bot's
	// Lane A). Opt-in because alert data names every vulnerable dependency
	// of every repo in the installation; at run time it is minted only into
	// the dedicated security-read token (SecurityReadInstallationPermissions),
	// never into the runtime forge token.
	AllowSecurityRead bool
	// AllowProjectBoard adds organization_projects:write so iterion can read
	// an org's Projects v2 board and reflect native card transitions onto its
	// Status field (ADR-097). Opt-in because it is an ORG-level grant — it
	// spans every project the org owns, not the installed repositories — and
	// at run time it is minted per board call only
	// (ProjectsInstallationPermissions); the cached runtime token never
	// carries it.
	AllowProjectBoard bool
	// SecurityReadOnly builds a WATCH-ONLY App: metadata:read plus
	// vulnerability_alerts:read, and nothing else. It REPLACES the runtime
	// baseline rather than adding to it, so the App can be installed org-wide
	// (which the org-wide alerts endpoint requires — it returns only what the
	// installation can see) without granting write on every repository.
	//
	// Its connection carries forge.PurposeSecurityRead and is excluded from
	// all runtime paths. When set, the other options are ignored: they only
	// add write grants, which is exactly what this shape exists to avoid.
	SecurityReadOnly bool
}

// BuildAppManifest assembles the manifest for an iterion forge GitHub App. The
// baseline permissions are the LEAST-PRIVILEGE set iterion's forge layer
// actually needs: push/read code (contents), open + comment on PRs
// (pull_requests), the mandatory metadata baseline, and manage the per-repo
// inbound webhook (repository_hooks — the App-level webhook is disabled,
// iterion creates per-repo hooks itself). `administration` (repo settings /
// deletion / branch-protection) is never part of the baseline — webhooks
// require `repository_hooks:write`, not `administration` — and only joins the
// grant when opts.AllowRepoCreation asks for the create-repo capability.
func BuildAppManifest(name, homeURL, redirectURL string, opts ...AppManifestOptions) AppManifest {
	for _, o := range opts {
		if o.SecurityReadOnly {
			// Whole-set replacement, evaluated before anything can widen it:
			// a watch-only App that also carried the runtime baseline would
			// defeat its own purpose.
			return newAppManifest(name, homeURL, redirectURL, SecurityReadInstallationPermissions())
		}
	}
	perms := RuntimeInstallationPermissions()
	// statuses:write lets Revi post its revi/review merge-gate commit status.
	// Optional at runtime (the mint falls back without it — see AppClient.rest),
	// but requested at App creation so new installations grant it up front.
	perms[PermissionStatuses] = "write"
	// checks:read lets the board card's CI panel list a ref's check-runs
	// through an App connection. Minted only into the CI profiles
	// (CIStatusInstallationPermissions), never into the runtime baseline;
	// requested here so a new installation grants it up front. An
	// installation approved before it was requested has to approve the
	// pending request, and the panel names that gap until it does.
	perms[PermissionChecks] = "read"
	for _, o := range opts {
		if o.AllowRepoCreation {
			perms["administration"] = "write"
		}
		if o.AllowAppDelivery {
			for name, level := range DeliveryInstallationPermissions() {
				perms[name] = level
			}
		}
		if o.AllowSecurityRead {
			perms["vulnerability_alerts"] = "read"
		}
		if o.AllowProjectBoard {
			perms["organization_projects"] = "write"
		}
	}
	return newAppManifest(name, homeURL, redirectURL, perms)
}

// newAppManifest assembles the manifest envelope around an already-decided
// permission set. Every App iterion creates shares the same callback/setup
// wiring; only DefaultPermissions differ between the runtime and the
// watch-only shapes.
func newAppManifest(name, homeURL, redirectURL string, perms map[string]string) AppManifest {
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
		// The App's granted permissions are iterion's least-privilege
		// runtime set (contents: clone + diff + push; pull_requests:
		// open + comment; metadata: baseline; repository_hooks: per-repo
		// inbound webhook), the two per-call read/verdict grants
		// (statuses: the merge-gate status; checks: the card CI panel's
		// check-runs read), plus administration:write ONLY when the
		// create-repo capability was opted in — minted tokens always
		// narrow to the subset a call needs. A SecurityReadOnly App instead
		// carries metadata:read + vulnerability_alerts:read and nothing else.
		DefaultPermissions: perms,
		HookAttributes:     map[string]any{"url": homeURL, "active": false},
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
	// Permissions is what the App GitHub actually created carries. iterion
	// POSTs the manifest through the operator's BROWSER, so what came back is
	// the only evidence of what was registered — the request iterion built is
	// not. A watch-only App is the one shape whose whole value rests on its
	// permissions, and an org owner is asked to install it on every repository:
	// the claim has to be confronted with this before it is recorded.
	Permissions map[string]string `json:"permissions"`
}

// IsSecurityReadOnly reports whether the created App carries exactly the
// watch-only permission profile and nothing else.
func (c ManifestConversion) IsSecurityReadOnly() bool {
	want := SecurityReadInstallationPermissions()
	if len(c.Permissions) != len(want) {
		return false
	}
	for name, level := range want {
		if c.Permissions[name] != level {
			return false
		}
	}
	return true
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
