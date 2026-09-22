package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	stdpath "path"
	"path/filepath"
	"strings"
	"time"
)

// RemoteConfig is the stored credential for talking to a remote iterion
// instance over its HTTP API. The token is an `iap_` personal access token sent
// as a Bearer header; long-lived, so the CLI never juggles refresh tokens.
type RemoteConfig struct {
	BaseURL string `json:"base_url"`
	Token   string `json:"token"`
	Email   string `json:"email,omitempty"`
	// TeamID / OrgID are the default tenant scope for team-/org-scoped
	// commands. Persisted by `iterion remote teams|orgs switch`; a
	// per-command --team/--org flag or ITERION_REMOTE_TEAM/_ORG wins.
	TeamID string `json:"team_id,omitempty"`
	OrgID  string `json:"org_id,omitempty"`
}

// ErrNotLoggedIn reports that no remote credential is stored.
var ErrNotLoggedIn = errors.New("not logged in — run `iterion remote login <url>` first")

// RemoteConfigPath is where the CLI stores the remote credential.
func RemoteConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".iterion", "cli-auth.json"), nil
}

func SaveRemoteConfig(c RemoteConfig) error {
	p, err := RemoteConfigPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o600)
}

func LoadRemoteConfig() (RemoteConfig, error) {
	p, err := RemoteConfigPath()
	if err != nil {
		return RemoteConfig{}, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return RemoteConfig{}, ErrNotLoggedIn
		}
		return RemoteConfig{}, err
	}
	var c RemoteConfig
	if err := json.Unmarshal(b, &c); err != nil {
		return RemoteConfig{}, fmt.Errorf("read %s: %w", p, err)
	}
	return c, nil
}

// ClearRemoteConfig removes the stored credential, returning its path.
func ClearRemoteConfig() (string, error) {
	p, err := RemoteConfigPath()
	if err != nil {
		return "", err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return p, err
	}
	return p, nil
}

// RemoteClient does authenticated requests to a configured instance.
type RemoteClient struct {
	cfg  RemoteConfig
	http *http.Client
}

func NewRemoteClient() (*RemoteClient, error) {
	cfg, err := ResolveRemoteConfig()
	if err != nil {
		return nil, err
	}
	return NewRemoteClientFor(cfg), nil
}

// ResolveRemoteConfig resolves the remote credential for a command.
// ITERION_REMOTE_URL switches to a pure-environment config (token from
// ITERION_REMOTE_TOKEN, falling back to ITERION_TOKEN) so CI can drive
// an instance with zero config file — and so a stored token is never
// sent to a different host than the one it was minted for. Without the
// env URL the stored ~/.iterion/cli-auth.json is used. ITERION_REMOTE_TEAM
// / ITERION_REMOTE_ORG override the persisted default scope either way.
func ResolveRemoteConfig() (RemoteConfig, error) {
	var cfg RemoteConfig
	if envURL := os.Getenv("ITERION_REMOTE_URL"); envURL != "" {
		tok := os.Getenv("ITERION_REMOTE_TOKEN")
		if tok == "" {
			tok = os.Getenv("ITERION_TOKEN")
		}
		cfg = RemoteConfig{BaseURL: strings.TrimRight(envURL, "/"), Token: tok}
	} else {
		loaded, err := LoadRemoteConfig()
		if err != nil {
			if errors.Is(err, ErrNotLoggedIn) {
				return RemoteConfig{}, fmt.Errorf("%w (or set ITERION_REMOTE_URL + ITERION_REMOTE_TOKEN for env-only use)", ErrNotLoggedIn)
			}
			return RemoteConfig{}, err
		}
		cfg = loaded
	}
	if t := os.Getenv("ITERION_REMOTE_TEAM"); t != "" {
		cfg.TeamID = t
	}
	if o := os.Getenv("ITERION_REMOTE_ORG"); o != "" {
		cfg.OrgID = o
	}
	return cfg, nil
}

func NewRemoteClientFor(cfg RemoteConfig) *RemoteClient {
	return &RemoteClient{cfg: cfg, http: &http.Client{Timeout: 120 * time.Second}}
}

func (c *RemoteClient) BaseURL() string { return c.cfg.BaseURL }

// do performs a request; bearer overrides the stored token when non-empty.
// Deliberately sends no Origin/Sec-Fetch headers so the server treats the CLI as
// a non-browser client (login then returns the access_token in the body).
func (c *RemoteClient) do(ctx context.Context, method, path string, body []byte, bearer string) (int, []byte, error) {
	contentType := ""
	if body != nil {
		contentType = "application/json"
	}
	return c.doRequest(ctx, method, path, body, bearer, contentType)
}

// doWithContentType is do with an explicit Content-Type (multipart uploads).
func (c *RemoteClient) doWithContentType(ctx context.Context, method, path string, body []byte, contentType string) (int, []byte, error) {
	return c.doRequest(ctx, method, path, body, "", contentType)
}

// errPathRetarget names a request whose path the server would rewrite.
var errPathRetarget = errors.New("remote: request path would be rewritten by the server")

// checkPathIsServed refuses a path the server would not serve as written.
//
// Every CLI command builds its URL by concatenating operator arguments into a
// path, and Go's ServeMux CLEANS the path and answers 307 — which net/http
// follows preserving method AND body. So `admin orgs delete
// '../users/u-victim'` deleted a USER and the CLI exited 0. The server
// re-authorises against the post-redirect path, so nothing is granted that
// the caller did not already hold; what is lost is the operator's ability to
// trust the target they typed.
//
// It lives here because this is the one function every request traverses:
// ~100 mutating call sites build their own paths, and a per-site escape is a
// guard that the next command added will not have. `url.PathEscape` at a call
// site is still worth having — it keeps an id with a slash inside its own
// segment — but it does NOT cover an id that IS `..`, since a dot is
// unreserved and travels unescaped.
//
// The predicate asks the SERVER's own cleaner which RESOURCE the path names,
// rather than listing the spellings that move one. Enumerating does not
// converge: the first version of this guard listed `.` and `..` and got both
// arms wrong — it skipped an EMPTY segment by construction
// (`/api/v1/bots//overlay` → `PUT /api/v1/bots/{name}`, a different route and
// a mutating one) and it refused `./report.md`, which addresses exactly the
// resource it spells.
//
// So: `.` is transparent — cleaning removes it and the resource is unchanged
// — while `..` and an empty segment SHIFT the segment list and land on
// another route. Dropping the transparent ones and comparing the rest against
// Clean separates the two without a list to widen next time.
//
// A segment merely CONTAINING a dot (`main.bot`, an email, a digest, a
// version) is untouched by cleaning and keeps working.
func checkPathIsServed(rawPath string) error {
	// Only the path is compared: cleaning a query string is meaningless, and
	// every typed command escapes a `/` inside a query value anyway.
	p, _, _ := strings.Cut(rawPath, "?")
	// Percent-decoded too: `%2e%2e` is the same segment spelled to slip past
	// a raw comparison, and net/url decodes it before the mux routes it.
	decoded := p
	if d, err := url.PathUnescape(p); err == nil {
		decoded = d
	}

	// The path as the operator meant it, with the transparent `.` segments
	// removed. Empty and `..` segments are KEPT, so Clean still has work to
	// do on them and the comparison below reddens.
	segs := strings.Split(decoded, "/")
	kept := make([]string, 0, len(segs))
	for i, seg := range segs {
		// The leading "" (from the root slash) and a single trailing "" (a
		// subtree pattern the mux treats as meaningful) are structural.
		if seg == "." && i != 0 && i != len(segs)-1 {
			continue
		}
		kept = append(kept, seg)
	}
	intended := strings.Join(kept, "/")

	cleaned := stdpath.Clean(intended)
	// Clean drops a trailing slash; the mux does not.
	if strings.HasSuffix(intended, "/") && !strings.HasSuffix(cleaned, "/") {
		cleaned += "/"
	}
	if cleaned != intended {
		return fmt.Errorf("%w: wrote %q, the server would serve %q — "+
			"an argument escaped its position in the URL", errPathRetarget, decoded, cleaned)
	}
	return nil
}

func (c *RemoteClient) doRequest(ctx context.Context, method, path string, body []byte, bearer, contentType string) (int, []byte, error) {
	u := strings.TrimRight(c.cfg.BaseURL, "/")
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if err := checkPathIsServed(path); err != nil {
		return 0, nil, err
	}
	u += path
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, r)
	if err != nil {
		return 0, nil, err
	}
	tok := bearer
	if tok == "" {
		tok = c.cfg.Token
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	return resp.StatusCode, b, nil
}

// API performs an authenticated request with the stored token.
func (c *RemoteClient) API(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	return c.do(ctx, method, path, body, "")
}

// LoginWithPassword exchanges email+password for an access JWT, then mints a
// long-lived PAT and returns it (the credential to store).
func (c *RemoteClient) LoginWithPassword(ctx context.Context, email, password, patName string) (string, error) {
	loginBody, _ := json.Marshal(map[string]string{"email": email, "password": password})
	code, body, err := c.do(ctx, "POST", "/api/auth/login", loginBody, "")
	if err != nil {
		return "", err
	}
	if code/100 != 2 {
		return "", fmt.Errorf("login failed (HTTP %d): %s", code, firstLine(body))
	}
	var lr struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &lr); err != nil || lr.AccessToken == "" {
		return "", fmt.Errorf("login succeeded but no access token was returned (cloud-mode instance required)")
	}
	patBody, _ := json.Marshal(map[string]any{"name": patName, "expires_in_days": 0})
	code, body, err = c.do(ctx, "POST", "/api/me/tokens", patBody, lr.AccessToken)
	if err != nil {
		return "", err
	}
	if code/100 != 2 {
		return "", fmt.Errorf("could not mint a token (HTTP %d): %s", code, firstLine(body))
	}
	var pr struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &pr); err != nil || pr.Token == "" {
		return "", fmt.Errorf("token endpoint returned no token")
	}
	return pr.Token, nil
}

func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}
