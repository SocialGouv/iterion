package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// tokenAnswer is the create/rotate answer GitLab documents for a project
// access token (docs/api/project_access_tokens): the token rides `token`,
// the expiry is a bare date.
func tokenAnswer(id int64, token string, expires any) map[string]any {
	return map[string]any{
		"id": id, "user_id": 166, "name": "iterion-bot", "token": token,
		"scopes": []string{"api"}, "access_level": 40, "active": true, "revoked": false,
		"created_at": "2026-10-21T10:00:00.000Z", "expires_at": expires,
	}
}

func TestMintProjectBot_RequestShapeAndCredential(t *testing.T) {
	var gotBody map[string]any
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.EscapedPath(), r.Header.Get("Authorization")
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(tokenAnswer(58, "glpat-minted-token", "2027-10-21"))
	}))
	defer srv.Close()

	cred, err := New(srv.Client(), srv.URL, "oauth-grant").MintProjectBot(context.Background(), "group/sub/api", forge.ProjectBotSpec{
		Name: forge.ProjectBotName, AccessLevel: forge.ProjectBotAccessLevel, Scopes: forge.ProjectBotScopes,
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/v4/projects/group%2Fsub%2Fapi/access_tokens" {
		t.Errorf("path = %q, want the namespaced project id", gotPath)
	}
	if gotAuth != "Bearer oauth-grant" {
		t.Errorf("the mint must ride the connection's grant, got Authorization %q", gotAuth)
	}
	if gotBody["name"] != "iterion-bot" {
		t.Errorf("name = %v", gotBody["name"])
	}
	if gotBody["access_level"] != float64(40) {
		t.Errorf("access_level = %v, want 40 (Maintainer)", gotBody["access_level"])
	}
	if scopes, _ := gotBody["scopes"].([]any); len(scopes) != 1 || scopes[0] != "api" {
		t.Errorf("scopes = %v, want [api]", gotBody["scopes"])
	}
	if want := time.Now().UTC().AddDate(0, 0, 365).Format(gitlabDate); gotBody["expires_at"] != want {
		t.Errorf("expires_at = %v, want %s: the mint always names an expiry (a GitLab that enforces expiry refuses a request without one), GitLab's ceiling first", gotBody["expires_at"], want)
	}
	if cred.TokenID != "58" || cred.Token != "glpat-minted-token" || cred.UserID != "166" {
		t.Errorf("credential = %+v", cred)
	}
	if want := time.Date(2027, 10, 21, 0, 0, 0, 0, time.UTC); !cred.ExpiresAt.Equal(want) {
		t.Errorf("expires_at = %v, want %v", cred.ExpiresAt, want)
	}
}

func TestMintProjectBot_ExplicitExpiryTravelsAsDate(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(tokenAnswer(1, "glpat-x", nil))
	}))
	defer srv.Close()
	cred, err := New(srv.Client(), srv.URL, "tok").MintProjectBot(context.Background(), "g/p", forge.ProjectBotSpec{
		Name: "b", AccessLevel: 40, Scopes: []string{"api"}, ExpiresAt: time.Date(2027, 1, 2, 15, 4, 5, 0, time.FixedZone("x", 3600)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotBody["expires_at"] != "2027-01-02" {
		t.Errorf("expires_at = %v, want the UTC date", gotBody["expires_at"])
	}
	if !cred.ExpiresAt.IsZero() {
		t.Errorf("a null expires_at must read as no expiry, got %v", cred.ExpiresAt)
	}
}

func TestMintProjectBot_RefusalsAreTyped(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   error
		detail string
	}{
		{"below maintainer or tokens disabled", 400, `{"message":"400 Bad request - User does not have permission to create project access token"}`, forge.ErrProjectBotNotPermitted, "User does not have permission"},
		{"access level above the caller's", 400, `{"message":"Access level of the token contains permissions not held by the creating user"}`, forge.ErrProjectBotNotPermitted, "permissions not held"},
		{"grant rejected", 401, `{"message":"401 Unauthorized"}`, forge.ErrUnauthorized, ""},
		{"project unknown to the grant", 404, `{"message":"404 Project Not Found"}`, forge.ErrNotFound, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			_, err := New(srv.Client(), srv.URL, "tok").MintProjectBot(context.Background(), "g/p", forge.ProjectBotSpec{Name: "b", AccessLevel: 40, Scopes: []string{"api"}})
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if tc.detail != "" && (err == nil || !contains(err.Error(), tc.detail)) {
				t.Errorf("err = %v, must carry GitLab's own reason %q", err, tc.detail)
			}
		})
	}
}

func TestMintProjectBot_AnswerWithoutTokenIsRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 5, "user_id": 9})
	}))
	defer srv.Close()
	if _, err := New(srv.Client(), srv.URL, "tok").MintProjectBot(context.Background(), "g/p", forge.ProjectBotSpec{Name: "b", AccessLevel: 40, Scopes: []string{"api"}}); err == nil {
		t.Fatal("a 2xx without a token must not yield a credential")
	}
}

func TestRotate_SelfAndAdminPaths(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.EscapedPath(), r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(tokenAnswer(59, "glpat-rotated", "2027-11-04"))
	}))
	defer srv.Close()
	exp := time.Date(2027, 11, 4, 0, 0, 0, 0, time.UTC)

	cred, err := New(srv.Client(), srv.URL, "bot-token").RotateSelf(context.Background(), "g/p", exp)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/v4/projects/g%2Fp/access_tokens/self/rotate" {
		t.Errorf("self rotate path = %q", gotPath)
	}
	if gotAuth != "Bearer bot-token" {
		t.Errorf("self rotation must authenticate AS the bot, got %q", gotAuth)
	}
	if gotBody["expires_at"] != "2027-11-04" {
		t.Errorf("expires_at = %v: without it GitLab issues a one-week token", gotBody["expires_at"])
	}
	if cred.TokenID != "59" || cred.Token != "glpat-rotated" || !cred.ExpiresAt.Equal(exp) {
		t.Errorf("credential = %+v", cred)
	}

	if _, err := New(srv.Client(), srv.URL, "oauth-grant").RotateProjectBot(context.Background(), "g/p", "58", exp); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/v4/projects/g%2Fp/access_tokens/58/rotate" {
		t.Errorf("admin rotate path = %q", gotPath)
	}
}

func TestRotate_DeadTokenIsUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"401 Unauthorized"}`))
	}))
	defer srv.Close()
	_, err := New(srv.Client(), srv.URL, "expired").RotateSelf(context.Background(), "g/p", time.Time{})
	if !errors.Is(err, forge.ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized (the worker falls back to the admin grant on it)", err)
	}
}

func TestRotate_RefusedExpiryKeepsGitLabsReason(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"Expiration date must be before the maximum allowed lifetime"}`))
	}))
	defer srv.Close()
	_, err := New(srv.Client(), srv.URL, "bot").RotateSelf(context.Background(), "g/p", time.Now().Add(400*24*time.Hour))
	if err == nil || errors.Is(err, forge.ErrProjectBotNotPermitted) {
		t.Fatalf("a rotation 400 is not a mint refusal: %v", err)
	}
	if !contains(err.Error(), "maximum allowed lifetime") {
		t.Errorf("err = %v, must carry GitLab's reason", err)
	}
}

func TestRevoke_AdminAndSelf(t *testing.T) {
	var gotPath, gotMethod string
	var status int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.EscapedPath(), r.Method
		w.WriteHeader(status)
	}))
	defer srv.Close()
	c := New(srv.Client(), srv.URL, "tok")

	status = http.StatusNoContent
	if err := c.RevokeProjectBot(context.Background(), "g/p", "58"); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/api/v4/projects/g%2Fp/access_tokens/58" {
		t.Errorf("admin revoke = %s %s", gotMethod, gotPath)
	}
	status = http.StatusNotFound
	if err := c.RevokeProjectBot(context.Background(), "g/p", "58"); !errors.Is(err, forge.ErrNotFound) {
		t.Errorf("a token already gone must answer the ErrNotFound class, got %v", err)
	}

	status = http.StatusNoContent
	if err := c.RevokeSelf(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/api/v4/personal_access_tokens/self" {
		t.Errorf("self revoke = %s %s", gotMethod, gotPath)
	}
	status = http.StatusUnauthorized
	if err := c.RevokeSelf(context.Background()); !errors.Is(err, forge.ErrUnauthorized) {
		t.Errorf("a dead token revoking itself answers 401, got %v", err)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// A GitLab that enforces token expiry refuses a mint without `expires_at`
// before any default applies (the PIC, v19.4.2: "expires_at is missing"),
// and its policy caps the lifetime — 365 days by default, lower per instance
// or group, refused as "must be before <date>". The client never guesses the
// cap: it names GitLab's ceiling, then shorter lifetimes, and keeps the first
// token minted — here a 90-day policy, found on the fourth request.
func TestMintProjectBot_LifetimeLadderIsJudgedByGitLab(t *testing.T) {
	today := time.Now().UTC().Truncate(24 * time.Hour)
	policy := today.AddDate(0, 0, 90)
	var mu sync.Mutex
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		raw, _ := body["expires_at"].(string)
		if raw == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"message":"400 Bad request - expires_at is missing"}`))
			return
		}
		mu.Lock()
		asked = append(asked, raw)
		mu.Unlock()
		if d, err := time.Parse(gitlabDate, raw); err != nil || d.After(policy) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"message":"Expires at must be before ` + policy.Format(gitlabDate) + `"}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(tokenAnswer(58, "glpat-minted", raw))
	}))
	defer srv.Close()

	cred, err := New(srv.Client(), srv.URL, "tok").MintProjectBot(context.Background(), "g/p", forge.ProjectBotSpec{Name: "b", AccessLevel: 40, Scopes: []string{"api"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{today.AddDate(0, 0, 365).Format(gitlabDate), today.AddDate(0, 0, 364).Format(gitlabDate), today.AddDate(0, 0, 180).Format(gitlabDate), policy.Format(gitlabDate)}
	mu.Lock()
	got := append([]string(nil), asked...)
	mu.Unlock()
	if len(got) != len(want) {
		t.Fatalf("asked %v, want the ladder walked down to the policy: %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("asked %v, want the ladder walked down to the policy: %v", got, want)
		}
	}
	if !cred.ExpiresAt.Equal(policy) {
		t.Errorf("expires_at = %v, want the lifetime GitLab granted %v", cred.ExpiresAt, policy)
	}
}

// A refusal of the caller's rights reads the same at every lifetime: the
// ladder ends, bounded by its length, and the typed error carries GitLab's
// own reason — while a status other than 400 (the grant itself refused)
// returns on the first answer.
func TestMintProjectBot_RefusalsEndTheLadder(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   error
		calls  int
	}{
		{"rights refused at every lifetime", 400, `{"message":"400 Bad request - User does not have permission to create project access token"}`, forge.ErrProjectBotNotPermitted, len(lifetimeLadder)},
		{"grant rejected: no second request", 401, `{"message":"401 Unauthorized"}`, forge.ErrUnauthorized, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				calls++
				mu.Unlock()
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			_, err := New(srv.Client(), srv.URL, "tok").MintProjectBot(context.Background(), "g/p", forge.ProjectBotSpec{Name: "b", AccessLevel: 40, Scopes: []string{"api"}})
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if tc.status == http.StatusBadRequest && !contains(err.Error(), "User does not have permission") {
				t.Errorf("err = %v, must carry GitLab's own reason", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if calls != tc.calls {
				t.Errorf("calls = %d, want %d", calls, tc.calls)
			}
		})
	}
}

// An answer that may have minted ends the ladder on the spot, whatever it
// says: a 2xx without a token, or a transport failure, is one request and
// one error — a retry on a shorter lifetime would create a second token
// nothing records.
func TestMintProjectBot_AnAnswerThatMayHaveMintedEndsTheLadder(t *testing.T) {
	t.Run("2xx without a token", func(t *testing.T) {
		var mu sync.Mutex
		calls := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			calls++
			mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 5, "user_id": 9})
		}))
		defer srv.Close()
		if _, err := New(srv.Client(), srv.URL, "tok").MintProjectBot(context.Background(), "g/p", forge.ProjectBotSpec{Name: "b", AccessLevel: 40, Scopes: []string{"api"}}); err == nil {
			t.Fatal("a 2xx without a token must not yield a credential")
		}
		mu.Lock()
		defer mu.Unlock()
		if calls != 1 {
			t.Errorf("calls = %d, want 1: the ladder must not retry an answer that may have minted", calls)
		}
	})
	t.Run("transport failure", func(t *testing.T) {
		var mu sync.Mutex
		dials := 0
		client := &http.Client{Transport: &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
			mu.Lock()
			dials++
			mu.Unlock()
			return nil, errors.New("forge unreachable")
		}}}
		if _, err := New(client, "http://forge.invalid", "tok").MintProjectBot(context.Background(), "g/p", forge.ProjectBotSpec{Name: "b", AccessLevel: 40, Scopes: []string{"api"}}); err == nil {
			t.Fatal("a transport failure must surface")
		}
		mu.Lock()
		defer mu.Unlock()
		if dials != 1 {
			t.Errorf("dials = %d, want 1: a request that may have reached GitLab is never repeated on a shorter lifetime", dials)
		}
	})
}

// A policy lowered after the mint refuses the lifetime the bot was granted:
// the rotation walks the ladder down like the mint did (GitLab rolls the
// revoke back when it refuses the new token, so every step is safe), and the
// bot lives on with the lifetime the policy now allows — here 30 days.
func TestRotate_WalksTheLadderWhenThePolicyShrank(t *testing.T) {
	today := time.Now().UTC().Truncate(24 * time.Hour)
	policy := today.AddDate(0, 0, 30)
	var mu sync.Mutex
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		raw, _ := body["expires_at"].(string)
		mu.Lock()
		asked = append(asked, raw)
		mu.Unlock()
		if d, err := time.Parse(gitlabDate, raw); err != nil || d.After(policy) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"message":"Expires at must be before ` + policy.Format(gitlabDate) + `"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(tokenAnswer(59, "glpat-rotated", raw))
	}))
	defer srv.Close()

	granted := today.AddDate(0, 0, 365)
	cred, err := New(srv.Client(), srv.URL, "bot-token").RotateSelf(context.Background(), "g/p", granted)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{granted.Format(gitlabDate), today.AddDate(0, 0, 364).Format(gitlabDate), today.AddDate(0, 0, 180).Format(gitlabDate), today.AddDate(0, 0, 90).Format(gitlabDate), policy.Format(gitlabDate)}
	mu.Lock()
	got := append([]string(nil), asked...)
	mu.Unlock()
	if len(got) != len(want) {
		t.Fatalf("asked %v, want the granted lifetime then the ladder down to the policy: %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("asked %v, want the granted lifetime then the ladder down to the policy: %v", got, want)
		}
	}
	if !cred.ExpiresAt.Equal(policy) {
		t.Errorf("expires_at = %v, want the lifetime the policy now allows %v", cred.ExpiresAt, policy)
	}
}
