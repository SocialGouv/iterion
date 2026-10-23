package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth/oidc"
)

func TestGitHubOrgsFromGroups(t *testing.T) {
	got := githubOrgsFromGroups([]string{
		"socialgouv/*",
		"socialgouv/iterion",
		"dnum-socialgouv/*",
		"socialgouv/another-team", // dup org
		"",                        // ignored
		"/weird",                  // empty org → ignored
	})
	want := []string{"socialgouv", "dnum-socialgouv"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if len(githubOrgsFromGroups(nil)) != 0 {
		t.Fatal("nil groups should yield no orgs")
	}
}

// stubGitHubOrgsConn is the deployment's GitHub connector for the read:org
// flow: the exchange answers one account's org group keys, or the failure a
// test arms.
type stubGitHubOrgsConn struct{ exchangeErr error }

func (stubGitHubOrgsConn) Name() string    { return "github" }
func (stubGitHubOrgsConn) Display() string { return "GitHub" }

func (stubGitHubOrgsConn) AuthorizeURL(context.Context, string, string, string) (string, error) {
	return "https://github.example/authorize", nil
}

func (c stubGitHubOrgsConn) ExchangeCode(context.Context, string, string, string) (oidc.ExternalUser, error) {
	if c.exchangeErr != nil {
		return oidc.ExternalUser{}, c.exchangeErr
	}
	return oidc.ExternalUser{Provider: "github", Subject: "s1", Email: fixtureEmail, Groups: []string{"acme/*"}}, nil
}

func (stubGitHubOrgsConn) SupportsPKCE() bool { return false }

// armGitHubOrgsCallback wires the fixture for GET
// /api/auth/oidc/github/callback/orgs: the deployment's GitHub connector and
// one pending state issued to the fixture's account. It returns the request
// that callback expects.
func armGitHubOrgsCallback(t *testing.T, f *authRouteFixture, conn oidc.Connector) *http.Request {
	t.Helper()
	reg := oidc.NewRegistry()
	reg.Register(conn)
	f.s.oidcRegistry = reg
	states := newForgeStateStore(time.Minute)
	f.s.forgeStates = states
	if err := states.put(forgePending{State: "st1", UserID: f.user.User.ID, IssuedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("issue the pending state: %v", err)
	}
	return httptest.NewRequest("GET", "/api/auth/oidc/github/callback/orgs?state=st1&code=c", nil)
}

// TestHandleGitHubOrgsCallback_AFailureAnswersTheGenericBody: the GitHub-orgs
// callback lives under /api/auth — a browser's top-level navigation — so
// every 5xx it answers owes the identity contract: the generic body, the
// cause in the server log. Its three failures are the account read, the
// write of the org list, and the upstream exchange.
func TestHandleGitHubOrgsCallback_AFailureAnswersTheGenericBody(t *testing.T) {
	for _, tc := range []struct {
		name string
		// arm returns the connector to wire, after arming the store.
		arm func(f *authRouteFixture) oidc.Connector
	}{
		{"the account read", func(f *authRouteFixture) oidc.Connector {
			f.ids.fail("GetUser", 0)
			return stubGitHubOrgsConn{}
		}},
		{"the write of the org list", func(f *authRouteFixture) oidc.Connector {
			f.ids.fail("PatchUser", 0)
			return stubGitHubOrgsConn{}
		}},
		{"the upstream exchange", func(f *authRouteFixture) oidc.Connector {
			return stubGitHubOrgsConn{exchangeErr: fmt.Errorf("github: token endpoint: %w", errClusterUnreachable)}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAuthRouteFixture(t)
			f.svc.SetStoreForTest(&storeGatePlus{storeGate: f.ids})
			r := armGitHubOrgsCallback(t, f, tc.arm(f))
			w := httptest.NewRecorder()
			f.s.handleGitHubOrgsCallback(w, r)
			assertIdentityUnavailable(t, w, f.logs, "the github-orgs callback whose "+tc.name+" failed")
			if loc := w.Header().Get("Location"); loc != "" {
				t.Fatalf("a failed %s redirected as a success to %q", tc.name, loc)
			}
		})
	}
}

// TestHandleGitHubOrgsCallback_ASuccessStoresTheOrgsAndRedirects keeps the
// happy path next to the failures above: the org logins land on the account
// and the browser is sent back to the app.
func TestHandleGitHubOrgsCallback_ASuccessStoresTheOrgsAndRedirects(t *testing.T) {
	f := newAuthRouteFixture(t)
	f.svc.SetStoreForTest(&storeGatePlus{storeGate: f.ids})
	r := armGitHubOrgsCallback(t, f, stubGitHubOrgsConn{})
	w := httptest.NewRecorder()
	f.s.handleGitHubOrgsCallback(w, r)
	if w.Code != http.StatusFound || w.Header().Get("Location") == "" {
		t.Fatalf("the callback answered %d %s, want a 302 back to the app", w.Code, strings.TrimSpace(w.Body.String()))
	}
	u, err := f.s.authStore().GetUser(context.Background(), f.user.User.ID)
	if err != nil {
		t.Fatalf("read the account back: %v", err)
	}
	if !reflect.DeepEqual(u.GitHubOrgs, []string{"acme"}) {
		t.Fatalf("the account's github orgs are %v, want [acme]", u.GitHubOrgs)
	}
}
