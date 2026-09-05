package forge

import (
	"context"
	"net/http"
	"strconv"
)

// AdminHTTP is the shared HTTP core every provider's AdminClient
// drives. It captures the four things that differ across providers —
// API base URL, error/log prefix, header strategy, HTTP client — so
// the do/statusErr/WhoAmI methods on each AdminClient collapse to
// one-liners that forward through this struct.
//
// Providers don't store an AdminHTTP on their client: each AdminClient
// keeps its existing exported fields (HTTP, APIBase/BaseURL, Token)
// for callers that build it as a struct literal (tests,
// github/app_client.go), and builds an AdminHTTP per call via
// NewAdminHTTP. The result is the same allocation pattern as the
// previous inline closures, with zero new fields on the per-provider
// struct.
type AdminHTTP struct {
	client     *http.Client
	apiBase    string
	provider   string
	setHeaders func(*http.Request)
}

// NewAdminHTTP builds the shared core with provider-specific bits:
// the HTTP client (nil → http.DefaultClient via DoJSON's fallback),
// the full API base ("https://api.github.com" or
// "https://<host>/api/v4"), the provider tag used in errors
// ("github" / "gitlab" / "forgejo"), and the header-setter callback
// applying that provider's Authorization + Accept + version headers.
func NewAdminHTTP(client *http.Client, apiBase, provider string, setHeaders func(*http.Request)) AdminHTTP {
	return AdminHTTP{client: client, apiBase: apiBase, provider: provider, setHeaders: setHeaders}
}

// Do performs one JSON-over-HTTP API call by delegating to DoJSON.
// path is appended to apiBase as-is so callers control query strings.
func (h AdminHTTP) Do(ctx context.Context, method, path string, body, out any) (int, error) {
	return DoJSON(ctx, h.client, method, h.apiBase+path, h.provider, h.setHeaders, body, out)
}

// DoMultipartFile uploads one file part by delegating to DoMultipartFile,
// with this client's base URL, header strategy and error prefix.
func (h AdminHTTP) DoMultipartFile(ctx context.Context, method, path, field, filename, contentType string, data []byte, out any) (int, []byte, error) {
	return DoMultipartFile(ctx, h.client, method, h.apiBase+path, h.provider, h.setHeaders, field, filename, contentType, data, out)
}

// StatusErr maps a non-2xx status to the right forge sentinel /
// generic wrapped error, using this client's provider prefix.
func (h AdminHTTP) StatusErr(op string, code int) error {
	return StatusErr(h.provider, op, code)
}

// loginIDEmail is the lowest-common-denominator /user response shape:
// github and forgejo both return `{id int64, login string, email string}`
// (gitlab returns `username` instead and is handled separately). Used by
// the github + forgejo AdminClient.WhoAmI implementations.
type loginIDEmail struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
	Email string `json:"email"`
}

// FetchWhoAmI is the shared WhoAmI implementation for providers whose
// /user endpoint returns the github/forgejo-shape {id, login, email}
// response. The result Identity is normalised (Kind="user",
// Namespace=Login). Callers pass the URL path because the API base is
// already baked into the client's setHeaders/apiBase.
func (h AdminHTTP) FetchWhoAmI(ctx context.Context, path string) (Identity, error) {
	var u loginIDEmail
	code, err := h.Do(ctx, http.MethodGet, path, nil, &u)
	if err != nil {
		return Identity{}, err
	}
	if code != http.StatusOK {
		return Identity{}, h.StatusErr("GET "+path, code)
	}
	return Identity{
		Login:     u.Login,
		ID:        strconv.FormatInt(u.ID, 10),
		Email:     u.Email,
		Kind:      AccountKindUser,
		Namespace: u.Login,
	}, nil
}
