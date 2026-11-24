package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth/desktopsso"
	"github.com/SocialGouv/iterion/pkg/auth/oidc"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// otherStubSSO is a SECOND registered provider, so a callback can arrive on
// a provider other than the one whose /start issued the state.
type otherStubSSO struct{ stubSSO }

func (otherStubSSO) Name() string    { return "othersso" }
func (otherStubSSO) Display() string { return "Other SSO" }

// TestDesktopSSO_AProviderErrorReachesTheClientTheStateNames: the refusal
// path that routes an IdP error to the desktop's loopback reads the flow
// from the state the callback carries, and that flow is the client WAITING —
// whichever provider's callback the error arrives on. It is the rule the
// accepted path applies to the same disagreement (pending.Provider != name
// is answered through redirectCallbackError with that pending), and the
// alternative is the timeout this branch exists to prevent: the state is
// consumed either way, so withholding the refusal protects nothing and
// strands the app. The disagreement is logged.
func TestDesktopSSO_AProviderErrorReachesTheClientTheStateNames(t *testing.T) {
	f := newDerivationFixture(t)
	f.s.desktopTickets = &recordingTickets{Store: desktopsso.NewMemoryStore(time.Minute)}
	reg := oidc.NewRegistry()
	reg.Register(stubSSO{ext: oidc.ExternalUser{Provider: "testsso", Subject: "desk-1", Email: "desk@example.org"}})
	reg.Register(otherStubSSO{})
	f.s.oidcRegistry = reg
	f.s.oidcStates = oidc.NewMemoryStateStore(10 * time.Minute)
	// The fixture's logger is built at LevelError, and the disagreement is
	// a Warn: witnessed at that level the assertion below could never
	// fail, whatever the branch did.
	f.s.logger = iterlog.New(iterlog.LevelWarn, f.logs)

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	start, err := client.Get(f.srv.URL + "/api/auth/oidc/testsso/start?desktop=1&format=json&next=" + url.QueryEscape("http://127.0.0.1:41234/desktop-callback"))
	if err != nil {
		t.Fatal(err)
	}
	defer start.Body.Close()
	var started struct {
		AuthorizeURL string `json:"authorize_url"`
	}
	if err := json.NewDecoder(start.Body).Decode(&started); err != nil {
		t.Fatalf("decode the start: %v", err)
	}
	authURL, err := url.Parse(started.AuthorizeURL)
	if err != nil {
		t.Fatal(err)
	}
	state := authURL.Query().Get("state")
	if state == "" {
		t.Fatal("the start issued no state")
	}

	// The OTHER provider's callback presents testsso's state.
	cb, err := client.Get(f.srv.URL + "/api/auth/oidc/othersso/callback?error=access_denied&state=" + url.QueryEscape(state))
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Body.Close()
	loc := cb.Header.Get("Location")
	if !strings.Contains(loc, "127.0.0.1:41234") {
		t.Fatalf("the refusal redirected to %q, want the loopback of the flow the state names — anything else leaves the app waiting out its timeout", loc)
	}
	if logs := f.logs.String(); !strings.Contains(logs, "issued by provider") {
		t.Fatalf("the provider disagreement left no trace in the log: %s", logs)
	}

	// The provider's OWN flow reaches its loopback too: the state decides,
	// and the ordinary case is not a disagreement.
	start2, err := client.Get(f.srv.URL + "/api/auth/oidc/testsso/start?desktop=1&format=json&next=" + url.QueryEscape("http://127.0.0.1:41235/desktop-callback"))
	if err != nil {
		t.Fatal(err)
	}
	defer start2.Body.Close()
	if err := json.NewDecoder(start2.Body).Decode(&started); err != nil {
		t.Fatalf("decode the second start: %v", err)
	}
	authURL2, err := url.Parse(started.AuthorizeURL)
	if err != nil {
		t.Fatal(err)
	}
	own, err := client.Get(f.srv.URL + "/api/auth/oidc/testsso/callback?error=access_denied&state=" + url.QueryEscape(authURL2.Query().Get("state")))
	if err != nil {
		t.Fatal(err)
	}
	defer own.Body.Close()
	if !strings.Contains(own.Header.Get("Location"), "127.0.0.1:41235") {
		t.Fatalf("the provider's own desktop refusal redirected to %q, want its loopback", own.Header.Get("Location"))
	}
}

// failingStates is a state store whose read does not complete — a store
// outage, not a state that is absent.
type failingStates struct {
	oidc.StateStore
	err error
}

func (f failingStates) Take(context.Context, string) (oidc.PendingAuth, error) {
	return oidc.PendingAuth{}, f.err
}

// TestDesktopSSO_AStateReadThatFailsIsNotAConsumedState: the refusal path
// claims the flow's state is consumed rather than left replayable. A read
// that did not complete consumes nothing, so it is reported — an error
// swallowed there would leave that claim standing on a replayable state,
// and would send the refusal to the SPA for a desktop client with no word
// of why.
func TestDesktopSSO_AStateReadThatFailsIsNotAConsumedState(t *testing.T) {
	f := newDerivationFixture(t)
	f.s.desktopTickets = &recordingTickets{Store: desktopsso.NewMemoryStore(time.Minute)}
	f.withStubSSO(oidc.ExternalUser{Provider: "testsso", Subject: "desk-1", Email: "desk@example.org"})
	f.s.oidcStates = failingStates{StateStore: f.s.oidcStates, err: context.DeadlineExceeded}

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	cb, err := client.Get(f.srv.URL + "/api/auth/oidc/testsso/callback?error=access_denied&state=whatever")
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Body.Close()
	if loc := cb.Header.Get("Location"); !strings.HasPrefix(loc, "/login?") {
		t.Fatalf("the refusal redirected to %q, want the SPA banner when no flow could be read", loc)
	}
	logs := f.logs.String()
	if !strings.Contains(logs, "NOT consumed") {
		t.Fatalf("a state read that failed left no trace in the log: %s", logs)
	}
}
