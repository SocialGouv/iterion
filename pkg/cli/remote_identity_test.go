package cli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
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
// credential, and a PAT is what the CLI stores. The two commands that mint
// one — `teams switch` (a team-pinned token) and `tokens create` — therefore
// refuse first, send nothing, and name the login that does the job. A request
// sent anyway would come back 403 and read as the instance failing.
func TestRemoteMintCommands_RefuseAPersonalAccessTokenBeforeSendingAnything(t *testing.T) {
	ctx := context.Background()
	p := NewPrinter(OutputHuman)

	t.Run("teams switch", func(t *testing.T) {
		srv := newCountingServer(t, func(w http.ResponseWriter, r *http.Request) {
			t.Errorf("the refusal sent %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		})
		c := NewRemoteClientFor(RemoteConfig{BaseURL: srv.URL, Token: "iap_stored"})
		err := RemoteTeamsSwitch(ctx, c, p, "t-target", "iterion-cli (host)")
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
