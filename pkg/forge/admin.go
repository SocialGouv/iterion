package forge

import "context"

// Admin is the per-provider outbound client the orchestrator drives. One
// implementation per (provider, auth-kind) — gitlab OAuth/PAT, github App,
// github OAuth, forgejo OAuth/PAT — all behind this single interface so the
// orchestrator is provider-agnostic.
//
// Event names in HookSpec.Events are PROVIDER-NATIVE (the orchestrator
// resolves normalized → native via event_map.go before calling). GitLab's
// implementation further translates those native names to its boolean
// request body.
type Admin interface {
	// Provider returns which forge this client talks to.
	Provider() Provider

	// WhoAmI returns the identity the credential represents. Called at
	// connect time to populate Connection.Account*, and to validate a
	// pasted PAT before persisting it. Returns ErrUnauthorized when the
	// credential is rejected.
	WhoAmI(ctx context.Context) (Identity, error)

	// ListRepos returns repos the credential can ADMIN (enough scope to
	// create a webhook). Paginated; the implementation collects up to
	// q.PerPage entries for the requested page.
	ListRepos(ctx context.Context, q RepoQuery) ([]RepoSummary, error)

	// GetHook finds an existing iterion-owned hook on repo by its delivery
	// URL. The idempotency primitive: the orchestrator calls this before
	// CreateHook so a re-run on an already-provisioned repo reuses the hook.
	// Returns (nil, nil) when no matching hook exists.
	GetHook(ctx context.Context, repo, deliveryURL string) (*HookHandle, error)

	// CreateHook registers a new webhook on repo. Returns the forge-side
	// hook handle. Returns ErrForbidden when the credential lacks hook-admin
	// scope.
	CreateHook(ctx context.Context, repo string, spec HookSpec) (HookHandle, error)

	// UpdateHook widens/narrows an existing hook's events in place (used
	// when a second bot is enabled on a repo, or on secret rotation). Must
	// be idempotent: an update to the same spec returns the same handle.
	UpdateHook(ctx context.Context, repo, hookID string, spec HookSpec) (HookHandle, error)

	// DeleteHook removes a hook. A missing hook (already deleted on the
	// forge) returns ErrHookNotFound, which deprovision treats as success.
	DeleteHook(ctx context.Context, repo, hookID string) error

	// ListHooks returns ALL webhooks currently registered on repo (not just
	// iterion-owned ones), for operator introspection/audit — surfacing
	// orphaned or divergent hooks. Returns ErrForbidden when the credential
	// lacks hook-admin scope.
	ListHooks(ctx context.Context, repo string) ([]HookHandle, error)
}

// OAuthExchanger drives a provider's OAuth authorization-code(+PKCE) flow
// at connect time: build the authorize URL the operator is redirected to,
// then trade the returned code for tokens. The per-provider OAuth clients
// (e.g. gitlab.OAuthApp) implement it; the server's connect/callback
// handlers consume it generically.
type OAuthExchanger interface {
	AuthorizeURL(redirectURI, state, codeChallenge string, scopes []string) string
	Exchange(ctx context.Context, code, redirectURI, codeVerifier string) (RefreshedToken, error)
}

// OAuthAppProvisioner creates an OAuth application on a forge instance using an
// operator-supplied admin credential, so iterion can auto-register the app it
// then drives the connect flow with — instead of the operator hand-creating it
// on the forge. Implemented by the per-provider AdminClient where the forge
// exposes a create-app API (GitLab /applications [admin], Forgejo
// /user/applications/oauth2 [user]). Not part of Admin: it's a one-time
// bootstrap, not a per-connection runtime op. Returns ErrForbidden when the
// credential lacks the rights (e.g. GitLab needs an instance admin).
type OAuthAppProvisioner interface {
	CreateOAuthApp(ctx context.Context, spec OAuthAppSpec) (OAuthAppCredentials, error)
}

// OAuthAppSpec describes the OAuth application to create on the forge.
type OAuthAppSpec struct {
	Name         string
	RedirectURI  string
	Scopes       []string
	Confidential bool
}

// OAuthAppCredentials is what a forge returns for a freshly created OAuth app.
// ProviderAppID is the forge-side id (retained for later deletion).
type OAuthAppCredentials struct {
	ClientID      string
	ClientSecret  string
	ProviderAppID string
}

// Identity is the account a credential authenticates as.
type Identity struct {
	Login string `json:"login"`
	ID    string `json:"id"`
	Email string `json:"email,omitempty"`
	// Kind is one of the AccountKind* constants: a GitHub-App connection
	// posts as the App ("installation"), an OAuth/PAT connection as the
	// person ("user") — or as the machine account the forge flags as one
	// ("bot": GitLab's access-token bot users and service accounts).
	Kind string `json:"kind,omitempty"`
	// Namespace is the user/org/group the account belongs to, surfaced in
	// the studio so the operator confirms the right account is connected.
	Namespace string `json:"namespace,omitempty"`
}

// RepoQuery filters a ListRepos call. Only repos the credential can admin
// (Permissions.Admin) are returned, so the repo picker never offers a repo
// where CreateHook would 403.
type RepoQuery struct {
	Search  string
	Page    int
	PerPage int
}

// RepoSummary is one repo offered in the studio repo picker.
type RepoSummary struct {
	// FullName is the canonical "owner/repo" (GitLab: namespace path /
	// project path) used as the integration + project-allowlist key.
	FullName      string `json:"full_name"`
	Description   string `json:"description,omitempty"`
	Private       bool   `json:"private"`
	DefaultBranch string `json:"default_branch,omitempty"`
	WebURL        string `json:"web_url,omitempty"`
	// CanAdmin reports whether the credential can create a webhook here.
	// The picker disables repos where it is false.
	CanAdmin bool `json:"can_admin"`
}

// HookSpec describes the webhook to register on the forge.
type HookSpec struct {
	// URL is the iterion inbound endpoint
	// (PublicURL + /api/webhooks/{provider}/{webhook_id}).
	URL string
	// Secret is the minted iwh_ plaintext — GitLab's "secret token" header
	// echo, or the HMAC signing key for github/forgejo. Held in memory only
	// for the duration of the CreateHook/UpdateHook call.
	Secret string
	// Events are PROVIDER-NATIVE event names (see event_map.go).
	Events []string
	Active bool
}

// HookHandle is a registered hook as the forge reports it.
type HookHandle struct {
	ID     string   `json:"id"` // forge-assigned (numeric on most forges; stored as string)
	URL    string   `json:"url"`
	Events []string `json:"events,omitempty"`
	Active bool     `json:"active"`
}

// AvatarSetter replaces the avatar of the ACCOUNT the credential authenticates
// as — the bot user behind a GitLab group/project access token, a dedicated
// Forgejo bot account — with the iterion-bot mascot, so every identity iterion
// operates shows one face. An optional capability, like RepoCreator: the
// GitHub clients deliberately do not implement it, because GitHub exposes no
// API for a user's avatar nor for an App's logo (the manifest flow cannot set
// one either) — those stay a documented manual upload. Callers gate on WHO the
// token is: an OAuth user token is a person's account and is never rebranded.
type AvatarSetter interface {
	// SetAvatar uploads png as the current account's avatar. avatarURL is
	// the URL the forge reports back, "" when it reports none (Forgejo).
	// Returns ErrAvatarUnsupported when the instance has no avatar endpoint.
	SetAvatar(ctx context.Context, png []byte) (avatarURL string, err error)
}
