package cli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/SocialGouv/iterion/pkg/secrets"
)

// countingServer is a cloud that records every request it is asked, so a
// refusal that claims to send nothing can be held to it.
type countingServer struct {
	*httptest.Server
	seen atomic.Int64
	log  []string
}

func newCountingServer(t *testing.T, h http.HandlerFunc) *countingServer {
	t.Helper()
	c := &countingServer{}
	c.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.seen.Add(1)
		c.log = append(c.log, r.Method+" "+r.URL.Path)
		h(w, r)
	}))
	t.Cleanup(c.Close)
	return c
}

// patMintRefused is the instance's own 403 at a route that mints a credential
// from the one presented (pkg/auth: CheckDerivation / ErrNotDerivable).
func patMintRefused(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_, _ = io.WriteString(w, `{"error":"a personal access token cannot mint another credential: sign in to mint one"}`)
}

// TestRemoteMintCommands_RefuseAPersonalAccessTokenBeforeSendingAnything: the
// instance refuses a personal access token at every route that mints another
// credential, and a PAT is what the CLI stores. The commands that mint one
// therefore refuse first, send nothing, and name the login that does the job:
// `tokens create`, and `teams switch` when its caller wires no browser
// approval (a nil seam — the CLI command wires the /cli-auth approval, which
// is the mint that does work, #2674). A request sent anyway would come back
// 403 and read as the instance failing.
func TestRemoteMintCommands_RefuseAPersonalAccessTokenBeforeSendingAnything(t *testing.T) {
	ctx := context.Background()
	p := NewPrinter(OutputHuman)

	t.Run("teams switch", func(t *testing.T) {
		srv := newCountingServer(t, func(w http.ResponseWriter, r *http.Request) {
			t.Errorf("the refusal sent %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		})
		c := NewRemoteClientFor(RemoteConfig{BaseURL: srv.URL, Token: "iap_stored"})
		err := RemoteTeamsSwitch(ctx, c, p, "t-target", "iterion-cli (host)", nil)
		if err == nil {
			t.Fatal("a teams switch from a personal access token reported success")
		}
		msg := err.Error()
		for _, want := range []string{
			"a personal access token cannot mint another credential",
			"iterion remote login " + srv.URL + " --email <you> --team t-target",
			"switch to the team in the studio",
		} {
			if !strings.Contains(msg, want) {
				t.Errorf("the refusal does not carry %q: %v", want, msg)
			}
		}
		if n := srv.seen.Load(); n != 0 {
			t.Fatalf("the refusal sent %d request(s) (%v): it refuses before presenting the credential", n, srv.log)
		}
	})

	t.Run("tokens create", func(t *testing.T) {
		srv := newCountingServer(t, func(w http.ResponseWriter, r *http.Request) {
			t.Errorf("the refusal sent %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		})
		c := NewRemoteClientFor(RemoteConfig{BaseURL: srv.URL, Token: "iap_stored"})
		err := RemoteTokensCreate(ctx, c, p, "ci", "", 0)
		if err == nil {
			t.Fatal("a tokens create from a personal access token reported success")
		}
		msg := err.Error()
		for _, want := range []string{
			"a personal access token cannot mint another credential",
			"Account → Access tokens",
			"iterion remote login " + srv.URL + " --email <you>",
		} {
			if !strings.Contains(msg, want) {
				t.Errorf("the refusal does not carry %q: %v", want, msg)
			}
		}
		if n := srv.seen.Load(); n != 0 {
			t.Fatalf("the refusal sent %d request(s) (%v)", n, srv.log)
		}
	})

	t.Run("a credential that is not iap_-prefixed reaches the instance's 403", func(t *testing.T) {
		srv := newCountingServer(t, func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/auth/me":
				_, _ = io.WriteString(w, `{"user":{"id":"u1"},"orgs":[{"org_id":"o1","teams":[{"team_id":"11111111-1111-1111-1111-111111111111"}]}]}`)
			default:
				patMintRefused(w, r)
			}
		})
		// An access token pasted into ITERION_REMOTE_TOKEN: the local check
		// cannot see it coming, so the instance's own refusal carries the
		// advice.
		c := NewRemoteClientFor(RemoteConfig{BaseURL: srv.URL, Token: "eyJhbGciOiJIUzI1NiJ9.x.y"})
		err := RemoteTokensCreate(ctx, c, p, "ci", "", 0)
		if err == nil {
			t.Fatal("a tokens create the instance refused reported success")
		}
		if msg := err.Error(); !strings.Contains(msg, "cannot mint another credential") || !strings.Contains(msg, "Account → Access tokens") {
			t.Errorf("the instance's 403 was not rendered actionably: %v", msg)
		}
		if n := srv.seen.Load(); n == 0 {
			t.Fatal("nothing was sent: a credential that is not a PAT must be presented")
		}
	})

	t.Run("a session credential mints", func(t *testing.T) {
		srv := newCountingServer(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/me/tokens" {
				http.NotFound(w, r)
				return
			}
			_, _ = io.WriteString(w, `{"pat":{"id":"pat-1"},"token":"iap_minted"}`)
		})
		c := NewRemoteClientFor(RemoteConfig{BaseURL: srv.URL, Token: "eyJhbGciOiJIUzI1NiJ9.x.y"})
		if err := RemoteTokensCreate(ctx, c, p, "ci", "", 0); err != nil {
			t.Fatalf("a mint from a session credential was refused: %v", err)
		}
	})
}

// TestRemoteTeamsSwitch_TheStoredPATApprovesThroughTheBrowser is the #2674
// witness at the client seam: with a personal access token stored, the switch
// mints NOTHING from it — no POST /api/me/tokens leaves the CLI — and the new
// token is the one the browser approval (the approving live session's mint)
// hands back, pinned to the resolved team UUID; the previous CLI token is
// revoked by fingerprint.
func TestRemoteTeamsSwitch_TheStoredPATApprovesThroughTheBrowser(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ITERION_REMOTE_URL", "")

	const teamUUID = "87a2ee99-6b91-43ca-bb51-659a8ce7f705"
	oldToken := "iap_stored"
	oldFP := secrets.FingerprintSHA256(oldToken)
	var revoked string
	srv := newCountingServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/auth/me":
			_, _ = io.WriteString(w, `{"user":{"id":"u1"},"orgs":[{"org_id":"o1","teams":[{"team_id":"`+teamUUID+`","team_name":"Team B","team_slug":"team-b"}]}]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/me/tokens":
			_, _ = io.WriteString(w, `{"tokens":[{"id":"p-old","name":"cli","fingerprint":"`+oldFP+`"}]}`)
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/me/tokens/"):
			revoked = strings.TrimPrefix(r.URL.Path, "/api/me/tokens/")
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("the switch sent %s %s with the stored PAT: a PAT mints nothing, the mint is the approval's", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	})
	if err := SaveRemoteConfig(RemoteConfig{BaseURL: srv.URL, Token: oldToken}); err != nil {
		t.Fatal(err)
	}
	c := NewRemoteClientFor(RemoteConfig{BaseURL: srv.URL, Token: oldToken})

	var approvedTeam string
	approve := func(_ context.Context, teamID string) (string, string, error) {
		approvedTeam = teamID
		return "iap_browser_minted", "o1", nil
	}
	p := NewPrinter(OutputHuman)
	if err := RemoteTeamsSwitch(context.Background(), c, p, "team-b", "iterion-cli (host)", approve); err != nil {
		t.Fatalf("the switch through the browser approval: %v", err)
	}
	if approvedTeam != teamUUID {
		t.Fatalf("the approval was asked for team %q, want the UUID the slug resolves to", approvedTeam)
	}
	cfg, err := LoadRemoteConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Token != "iap_browser_minted" || cfg.TeamID != teamUUID || cfg.OrgID != "o1" {
		t.Fatalf("the stored credential is %+v, want the approval's token pinned to %s in o1", cfg, teamUUID)
	}
	if revoked != "p-old" {
		t.Fatalf("revoked = %q, want p-old (the fingerprint match)", revoked)
	}
	for _, line := range srv.log {
		if strings.HasPrefix(line, "POST /api/me/tokens") {
			t.Fatalf("the CLI minted from the stored credential: %v", srv.log)
		}
	}
}
