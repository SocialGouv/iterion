package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/auth/oidc"
	"github.com/SocialGouv/iterion/pkg/auth/orgsso"
	"github.com/SocialGouv/iterion/pkg/identity"
)

// The class #2655 names at the read sites outside #1668's claims: a read that
// FAILED answers its failure — logged, generic body — and never a fact. Three
// surfaces, each with the MIRROR scenario where the fact is legitimate because
// the store ANSWERED: an isolated pair per site, since a guard that only ever
// sees the failing direction passes just as well on a route that refuses
// everything.

// The failure a reachable store answers is errStoreDown (declared by
// forge_app_degraded_read_test.go, the same class on the forge App read): not
// ErrNotFound, so nothing about it says the row is absent.

// failingProviderStore answers errStoreDown from Get and delegates the rest:
// the per-org SSO resolver's only read of a provider row.
type failingProviderStore struct {
	orgsso.Store
	err error
}

func (f failingProviderStore) Get(context.Context, string) (orgsso.OrgSSOProvider, error) {
	return orgsso.OrgSSOProvider{}, f.err
}

// The account reads use the house fail-the-Nth-call harness (storeGate,
// auth_views_test.go): it already gates GetUsersByIDs and GetUser BY NAME, so
// the roster's own read fails while the authorization read it runs first
// (GetOrgMembership) still answers — the difference a wrapper failing every
// read would hide behind a 403.

// seedEnabledOrgProvider writes an enabled per-org OIDC row and answers its
// slug, so a resolution that fails afterwards fails on the READ and not on a
// provider that was never configured.
func seedEnabledOrgProvider(t *testing.T, s *Server) string {
	t.Helper()
	sealed, err := orgsso.SealClientSecret(s.sealer, "p1", "s3cr3t")
	if err != nil {
		t.Fatal(err)
	}
	row := orgsso.OrgSSOProvider{
		ID: "p1", TenantID: "t1", Kind: orgsso.KindOIDC, Enabled: true,
		IssuerURL: "https://sso.acme.example/realms/main", ClientID: "iterion",
		SealedSecret: sealed, CreatedAt: time.Now(),
	}
	if err := s.orgSSO.Create(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	return row.OIDCSlug()
}

// TestSSOStart_anUnreadableProviderStoreIsNotAnUnknownProvider: /start used to
// answer 404 "unknown provider" for every resolver failure, so a tenant whose
// provider store was down read as a tenant whose SSO had been deleted — and
// nothing was logged.
func TestSSOStart_anUnreadableProviderStoreIsNotAnUnknownProvider(t *testing.T) {
	s := newOrgSSOTestServer(t)
	slug := seedEnabledOrgProvider(t, s)
	s.orgSSO = failingProviderStore{Store: s.orgSSO, err: errStoreDown}

	w := newServerErrorRecorder(s)
	r := httptest.NewRequest("GET", "/api/auth/oidc/"+slug+"/start", nil)
	r.SetPathValue("provider", slug)
	s.handleOIDCStart(w, r)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("code = %d body = %s, want 500: a failed read is not the fact that the provider is unknown", w.Code, w.Body.String())
	}
	logged := w.Diagnostic(t)
	if !strings.Contains(logged, "p1") || !strings.Contains(logged, errStoreDown.Error()) {
		t.Errorf("the cause did not survive in the log: %q, want the provider id and %q", logged, errStoreDown)
	}
}

// TestSSOStart_aProviderThatIsNotThereStillAnswers404 is the mirror: the
// store ANSWERED that the row is absent, which is a fact /start owes the
// caller as a 404 — the 500 above must not swallow it.
func TestSSOStart_aProviderThatIsNotThereStillAnswers404(t *testing.T) {
	s := newOrgSSOTestServer(t)

	w := newServerErrorRecorder(s)
	r := httptest.NewRequest("GET", "/api/auth/oidc/oidc-org-nope/start", nil)
	r.SetPathValue("provider", "oidc-org-nope")
	s.handleOIDCStart(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("code = %d body = %s, want 404 for a provider the store says is absent", w.Code, w.Body.String())
	}
}

// TestSSOCallback_anUnreadableProviderStoreBannersProviderUnavailable: the
// callback is a top-level navigation, so its answer is a banner code. It used
// to be "unknown_provider", whose SPA text tells the user to stop waiting and
// sign in with a password an SSO-only account does not have.
func TestSSOCallback_anUnreadableProviderStoreBannersProviderUnavailable(t *testing.T) {
	s := newOrgSSOTestServer(t)
	slug := seedEnabledOrgProvider(t, s)
	s.orgSSO = failingProviderStore{Store: s.orgSSO, err: errStoreDown}
	logs := captureFailureLogs(s)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/auth/oidc/"+slug+"/callback?state=x&code=y", nil)
	r.SetPathValue("provider", slug)
	s.handleOIDCCallback(w, r)

	if w.Code != http.StatusFound {
		t.Fatalf("code = %d, want 302", w.Code)
	}
	if want := "/login?sso_error=" + ssoErrProviderUnavailable; w.Header().Get("Location") != want {
		t.Fatalf("Location = %q, want %q", w.Header().Get("Location"), want)
	}
	if got := logs.String(); !strings.Contains(got, errStoreDown.Error()) {
		t.Errorf("the cause did not survive in the log: %q", got)
	}
}

// TestSSOCallback_aProviderThatIsNotThereStillBannersUnknownProvider is the
// mirror of the banner: the resolver's ErrUnknownProvider still reaches the
// SPA as unknown_provider.
func TestSSOCallback_aProviderThatIsNotThereStillBannersUnknownProvider(t *testing.T) {
	s := newOrgSSOTestServer(t)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/auth/oidc/oidc-org-nope/callback?state=x&code=y", nil)
	r.SetPathValue("provider", "oidc-org-nope")
	s.handleOIDCCallback(w, r)

	if want := "/login?sso_error=" + ssoErrUnknownProvider; w.Header().Get("Location") != want {
		t.Fatalf("Location = %q, want %q", w.Header().Get("Location"), want)
	}
}

// TestSSOResolver_anUnsealableSecretIsNotAnUnknownProvider: the row is there
// and enabled, so a client secret this deployment's sealer cannot open is ITS
// failure — the second arm the resolver used to collapse into "unknown".
func TestSSOResolver_anUnsealableSecretIsNotAnUnknownProvider(t *testing.T) {
	s := newOrgSSOTestServer(t)
	if err := s.orgSSO.Create(context.Background(), orgsso.OrgSSOProvider{
		ID: "p1", TenantID: "t1", Kind: orgsso.KindOIDC, Enabled: true,
		IssuerURL: "https://sso.acme.example/realms/main", ClientID: "iterion",
		// Sealed by nobody: the sealer refuses it.
		SealedSecret: []byte("not-a-sealed-secret"), CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	_, _, _, err := s.resolveConnector(context.Background(), "oidc-org-p1")
	if !errors.Is(err, oidc.ErrProviderUnavailable) {
		t.Fatalf("resolveConnector err = %v, want ErrProviderUnavailable", err)
	}
	if errors.Is(err, oidc.ErrUnknownProvider) {
		t.Errorf("a secret the sealer cannot open answered as an unknown provider: %v", err)
	}
}

// TestOrgRoster_aFailedAccountReadIsNotANamelessMember: the twin of the team
// roster #1668 fixed. A failed account read rendered every member as an
// account with no email and no name, answered 200 — an org admin reading it
// as "these accounts lost their identity".
func TestOrgRoster_aFailedAccountReadIsNotANamelessMember(t *testing.T) {
	f := newAuthRouteFixture(t)
	// The fixture's account is an ADMIN of org o1 (seedOrgTreeFor), so the
	// roster's authorization read answers from the row and only the account
	// read below is armed to fail.
	f.ids.fail("GetUsersByIDs", 1)

	w := newServerErrorRecorder(f.s)
	ctx := auth.WithIdentity(context.Background(), auth.Identity{UserID: f.user.User.ID})
	f.s.handleListOrgMembers(w, orgReq(ctx, "GET", "/api/orgs/o1/members", "", "o1"))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("code = %d body = %s, want 500: a failed account read is not a member with no name", w.Code, w.Body.String())
	}
	if body := w.Body.String(); strings.Contains(body, "\"user_id\"") {
		t.Errorf("the roster rendered a member from a failed read: %s", body)
	}
	logged := w.Diagnostic(t)
	if !strings.Contains(logged, "GetUsersByIDs") || !strings.Contains(logged, "o1") {
		t.Errorf("the cause did not survive in the log: %q, want the failed read and the org", logged)
	}
	// The store's own text names an internal host: it stays in the log.
	if strings.Contains(w.Body.String(), internalHost) {
		t.Errorf("the store's text reached the client: %s", w.Body.String())
	}
}

// TestOrgRoster_anAbsentAccountStillRendersItsRow is the mirror: a membership
// whose account the store ANSWERED is gone for still renders with its id and
// role — the fact the roster owes, and the one the 500 above must not eat.
func TestOrgRoster_anAbsentAccountStillRendersItsRow(t *testing.T) {
	f := newAuthRouteFixture(t)
	// A membership whose account row was never written: GetUsersByIDs
	// ANSWERS a map without it.
	if err := f.s.authStore().UpsertOrgMembership(context.Background(), identity.OrgMembership{
		UserID: "u-gone", OrgID: "o1", Role: identity.OrgRoleMember, JoinedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	ctx := auth.WithIdentity(context.Background(), auth.Identity{UserID: f.user.User.ID})
	f.s.handleListOrgMembers(w, orgReq(ctx, "GET", "/api/orgs/o1/members", "", "o1"))

	if w.Code != http.StatusOK {
		t.Fatalf("code = %d body = %s, want 200", w.Code, w.Body.String())
	}
	var got struct {
		Members []orgMemberView `json:"members"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	var gone *orgMemberView
	for i, m := range got.Members {
		if m.UserID == "u-gone" {
			gone = &got.Members[i]
		}
	}
	if gone == nil || gone.Role != string(identity.OrgRoleMember) || gone.Email != "" {
		t.Fatalf("members = %+v, want the row of u-gone with its role and no email", got.Members)
	}
}

// TestMyGitHubOrgs_aFailedReadIsNotAnEmptyList: the list used to answer 200
// with no orgs when its read failed, so the "Create a GitHub App" form offered
// to redo a connect flow that had already succeeded.
func TestMyGitHubOrgs_aFailedReadIsNotAnEmptyList(t *testing.T) {
	f := newAuthRouteFixture(t)
	f.ids.fail("GetUser", 1)

	w := newServerErrorRecorder(f.s)
	r := httptest.NewRequest("GET", "/api/me/github-orgs", nil)
	r = r.WithContext(auth.WithIdentity(context.Background(), auth.Identity{UserID: f.user.User.ID}))
	f.s.handleListMyGitHubOrgs(w, r)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("code = %d body = %s, want 500: a failed read is not an empty list of orgs", w.Code, w.Body.String())
	}
	if body := w.Body.String(); strings.Contains(body, "\"orgs\"") {
		t.Errorf("a failed read answered a list anyway: %s", body)
	}
	if logged := w.Diagnostic(t); !strings.Contains(logged, "GetUser") {
		t.Errorf("the cause did not survive in the log: %q", logged)
	}
}

// TestMyGitHubOrgs_anAnsweredReadStillAnswersItsList is the mirror: an account
// the store answers for — with orgs, or absent entirely — keeps its 200, since
// an empty list is the fact "nothing connected yet".
func TestMyGitHubOrgs_anAnsweredReadStillAnswersItsList(t *testing.T) {
	f := newAuthRouteFixture(t)
	s := f.s
	if _, err := s.authStore().PatchUser(context.Background(), f.user.User.ID, identity.UserPatch{
		GitHubOrgs: &[]string{"socialgouv"},
	}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name   string
		userID string
		want   string
	}{
		{"the orgs it connected", f.user.User.ID, `{"orgs":["socialgouv"]}`},
		{"an account the store says is absent", "u-gone", `{"orgs":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest("GET", "/api/me/github-orgs", nil)
			r = r.WithContext(auth.WithIdentity(context.Background(), auth.Identity{UserID: tc.userID}))
			s.handleListMyGitHubOrgs(w, r)
			if w.Code != http.StatusOK {
				t.Fatalf("code = %d body = %s, want 200", w.Code, w.Body.String())
			}
			if got := strings.TrimSpace(w.Body.String()); got != tc.want {
				t.Fatalf("body = %s, want %s", got, tc.want)
			}
		})
	}
}

// unauthorizableGitHubConn is the deployment's GitHub connector whose
// authorize step fails — a discovery or a template error whose text carries
// the URL it dialled and the upstream body.
type unauthorizableGitHubConn struct{ err error }

func (unauthorizableGitHubConn) Name() string    { return "github" }
func (unauthorizableGitHubConn) Display() string { return "GitHub" }

func (c unauthorizableGitHubConn) AuthorizeURL(context.Context, string, string, string) (string, error) {
	return "", c.err
}

func (unauthorizableGitHubConn) ExchangeCode(context.Context, string, string, string) (oidc.ExternalUser, error) {
	return oidc.ExternalUser{}, errors.New("not reached")
}

func (unauthorizableGitHubConn) SupportsPKCE() bool { return false }

// TestGitHubOrgsStart_aFailedAuthorizeKeepsItsTextInTheLog: the start route
// passed the connector's own error text as its 5xx message. The shared writer
// already floors every 5xx body to the generic one (internal/httpx
// WriteErrorJSON), so what the client reads was ALREADY safe — measured, by
// the mutant that puts the raw text back and changes nothing a client sees.
// What the route owed, and did not answer, is the LOG: the failed operation
// has to be named there, or the cause arrives as a bare connector string
// with nothing saying which call produced it.
func TestGitHubOrgsStart_aFailedAuthorizeKeepsItsTextInTheLog(t *testing.T) {
	f := newAuthRouteFixture(t)
	reg := oidc.NewRegistry()
	reg.Register(unauthorizableGitHubConn{
		err: fmt.Errorf("github: discovery at https://%s/.well-known: %w", internalHost, errStoreDown),
	})
	f.s.oidcRegistry = reg
	f.s.forgeStates = newForgeStateStore(time.Minute)

	w := newServerErrorRecorder(f.s)
	r := httptest.NewRequest("GET", "/api/forge/github/orgs/start", nil)
	r = r.WithContext(auth.WithIdentity(context.Background(), auth.Identity{UserID: f.user.User.ID}))
	f.s.handleStartGitHubOrgs(w, r)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("code = %d body = %s, want 500", w.Code, w.Body.String())
	}
	// The floor the shared writer already guarantees: no connector text in
	// the body. It is asserted, not credited to this change.
	if body := w.Body.String(); strings.Contains(body, internalHost) {
		t.Errorf("the connector's text reached the client: %s", body)
	}
	logged := w.Diagnostic(t)
	if !strings.Contains(logged, internalHost) {
		t.Errorf("the cause did not survive in the log: %q", logged)
	}
	// What this change adds: the log names the call that failed.
	if !strings.Contains(logged, "build the github orgs authorize URL") {
		t.Errorf("the log does not name the failed operation: %q", logged)
	}
}

// The sentinel's remaining consumers (#2655 R4b). resolveConnector is shared
// by four routes, and only the two SSO ones classified its failure: the
// GitHub-orgs routes answered every failure with "github (sign-in) is not
// configured" — a CONFIGURATION fact — and logged nothing. The classification
// now lives in ONE place for every JSON consumer (refuseConnector), the
// callback keeping its own three-way banner split.
//
// Reachability, stated: with the fixed "github" slug those two routes cannot
// reach ErrProviderUnavailable today — the per-org branch of the resolver
// needs an "oidc-org-" slug (orgsso.ParseOIDCSlug), and the global registry
// answers only ErrUnknownProvider. So the sentinel below is produced by the
// REAL resolver on the slug that can carry it, and handed to the classifier
// with each forge route's own refusal word.

func TestGitHubOrgsRoutes_anUnreadableResolutionIsNotANotConfiguredAnswer(t *testing.T) {
	s := newOrgSSOTestServer(t)
	slug := seedEnabledOrgProvider(t, s)
	s.orgSSO = failingProviderStore{Store: s.orgSSO, err: errStoreDown}
	_, _, _, resolveErr := s.resolveConnector(context.Background(), slug)
	if !errors.Is(resolveErr, oidc.ErrProviderUnavailable) {
		t.Fatalf("the resolver answered %v, want the ErrProviderUnavailable the routes must classify", resolveErr)
	}

	for _, tc := range []struct{ name, msg string }{
		{"the start route's word", "github sign-in is not configured on this server"},
		{"the callback's word", "github not configured"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newServerErrorRecorder(s)
			r := httptest.NewRequest("GET", "/api/forge/github/orgs/start", nil)
			s.refuseConnector(w, r, "github", resolveErr, http.StatusBadRequest, tc.msg)

			if w.Code != http.StatusInternalServerError {
				t.Fatalf("code = %d body = %s, want 500: a resolution that FAILED is not the fact that github is not configured", w.Code, w.Body.String())
			}
			if body := w.Body.String(); strings.Contains(body, tc.msg) || strings.Contains(body, "p1") {
				t.Errorf("the client read a configuration answer or the cause: %s", body)
			}
			logged := w.Diagnostic(t)
			if !strings.Contains(logged, "p1") || !strings.Contains(logged, errStoreDown.Error()) {
				t.Errorf("the cause did not survive in the log: %q, want the provider id and %q", logged, errStoreDown)
			}
		})
	}
}

// TestGitHubOrgsStart_aConnectorThatIsNotThereKeepsItsRefusal is the mirror,
// through the real route: a deployment with no github connector at all is the
// fact the 400 exists for, and it must keep its word — and log nothing.
func TestGitHubOrgsStart_aConnectorThatIsNotThereKeepsItsRefusal(t *testing.T) {
	f := newAuthRouteFixture(t)
	f.s.oidcRegistry = oidc.NewRegistry()
	f.s.forgeStates = newForgeStateStore(time.Minute)
	logs := captureFailureLogs(f.s)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/forge/github/orgs/start", nil)
	r = r.WithContext(auth.WithIdentity(context.Background(), auth.Identity{UserID: f.user.User.ID}))
	f.s.handleStartGitHubOrgs(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d body = %s, want 400 for a github connector this deployment does not have", w.Code, w.Body.String())
	}
	if body := w.Body.String(); !strings.Contains(body, "not configured") {
		t.Errorf("body = %s, want the configuration refusal", body)
	}
	if logged := logs.String(); strings.Contains(logged, "resolve the SSO provider") {
		t.Errorf("a provider nobody configured was logged as a failure: %q", logged)
	}
}

// resolveConnectorConsumers pins the WIRING R4b closes, which the two
// witnesses above cannot: they drive the classifier and the absent-connector
// route, so a consumer that stops crossing refuseConnector — the exact defect
// R4b removed from the two GitHub-orgs routes — lands with the suite green.
// The sentinel is unreachable from those routes' fixed "github" slug (the
// resolver's per-org branch needs an "oidc-org-" slug), so no end-to-end
// request can witness their branch; what certifies it is the source they are
// built from.
//
// Keyed by the enclosing function, valued by what its error branch must name:
//
//   - "refuseConnector" — the JSON consumers, which hand the branch to the one
//     classifier: the sentinel owes the identity contract, anything else keeps
//     the route's own word.
//   - "ErrProviderUnavailable" — handleOIDCCallback, which answers a banner
//     redirect and splits a third case (ErrProviderDisabled) the API routes
//     have no word for, so it names the sentinel itself.
var resolveConnectorConsumers = map[string]string{
	"beginOIDCFlow":            "refuseConnector",
	"handleStartGitHubOrgs":    "refuseConnector",
	"handleGitHubOrgsCallback": "refuseConnector",
	"handleOIDCCallback":       "ErrProviderUnavailable",
}

// resolveConnectorSite is one call of the resolver found in the package's
// source, with what its error branch names.
type resolveConnectorSite struct {
	fn      string
	file    string
	line    int
	guarded bool     // the call is followed by `if err != nil { … }`
	names   []string // the identifiers that branch mentions
}

func TestResolveConnector_everyConsumerClassifiesAnUnavailableResolution(t *testing.T) {
	sites, total := parseResolveConnectorSites(t)

	// A consumer written in a shape the statement matcher below does not read
	// (the call inside an if-init, or in a return) would be invisible to this
	// guard while answering whatever it likes: the counts have to agree.
	if len(sites) != total {
		t.Fatalf("the package calls resolveConnector %d time(s) and this guard reads the error branch of %d of them: "+
			"a call in a shape parseResolveConnectorSites does not match (an if-init, a return) is a consumer nothing "+
			"certifies — teach the matcher that shape.", total, len(sites))
	}
	if len(sites) == 0 {
		t.Fatal("no resolveConnector call found in the package source: the resolver was renamed or this guard is " +
			"reading the wrong directory")
	}

	seen := map[string]bool{}
	for _, st := range sites {
		want, pinned := resolveConnectorConsumers[st.fn]
		if !pinned {
			t.Errorf("%s:%d: %s resolves a connector and is not in resolveConnectorConsumers — a new consumer owes the "+
				"classification (refuseConnector for a JSON route) and a row here saying which.", st.file, st.line, st.fn)
			continue
		}
		seen[st.fn] = true
		if !st.guarded {
			t.Errorf("%s:%d: %s does not test the resolver's error before using the connector", st.file, st.line, st.fn)
			continue
		}
		if !slices.Contains(st.names, want) {
			t.Errorf("%s:%d: the error branch of %s names %v, want %s — a resolution that FAILED is not the fact that "+
				"the provider is absent, and this branch answers its route's word for \"absent\" to an unreachable "+
				"provider store, with nothing logged for the operator.", st.file, st.line, st.fn, st.names, want)
		}
	}
	for fn := range resolveConnectorConsumers {
		if !seen[fn] {
			t.Errorf("%s is pinned in resolveConnectorConsumers but resolves no connector in the package source: "+
				"the row outlived its consumer.", fn)
		}
	}
}

// parseResolveConnectorSites reads every non-test file of the package once and
// parses THAT text, so what it certifies is the source the package is built
// from. It answers the sites it could read the error branch of, and the total
// number of resolveConnector calls it saw.
func parseResolveConnectorSites(t *testing.T) (sites []resolveConnectorSite, total int) {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok && calleeName(c) == "resolveConnector" {
				total++
			}
			return true
		})
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				blk, ok := n.(*ast.BlockStmt)
				if !ok {
					return true
				}
				for i, st := range blk.List {
					call := resolveConnectorAssign(st)
					if call == nil {
						continue
					}
					site := resolveConnectorSite{fn: fd.Name.Name, file: name, line: fset.Position(call.Pos()).Line}
					if i+1 < len(blk.List) {
						if ifs, ok := blk.List[i+1].(*ast.IfStmt); ok && isErrNotNil(ifs.Cond) {
							site.guarded = true
							site.names = identsIn(ifs.Body)
						}
					}
					sites = append(sites, site)
				}
				return true
			})
		}
	}
	return sites, total
}

// resolveConnectorAssign answers the resolver call of a DIRECT assignment in a
// statement list, so one call is read by its innermost block only.
func resolveConnectorAssign(st ast.Stmt) *ast.CallExpr {
	as, ok := st.(*ast.AssignStmt)
	if !ok {
		return nil
	}
	for _, rhs := range as.Rhs {
		if c, ok := rhs.(*ast.CallExpr); ok && calleeName(c) == "resolveConnector" {
			return c
		}
	}
	return nil
}

func calleeName(c *ast.CallExpr) string {
	switch fn := c.Fun.(type) {
	case *ast.SelectorExpr:
		return fn.Sel.Name
	case *ast.Ident:
		return fn.Name
	}
	return ""
}

func isErrNotNil(cond ast.Expr) bool {
	be, ok := cond.(*ast.BinaryExpr)
	if !ok || be.Op != token.NEQ {
		return false
	}
	x, xok := be.X.(*ast.Ident)
	y, yok := be.Y.(*ast.Ident)
	return xok && yok && x.Name == "err" && y.Name == "nil"
}

// identsIn answers every identifier a branch mentions, selector tails
// included, so a call (refuseConnector) and a sentinel reference
// (oidc.ErrProviderUnavailable) are both found by name.
func identsIn(n ast.Node) []string {
	var out []string
	ast.Inspect(n, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok {
			out = append(out, id.Name)
		}
		return true
	})
	sort.Strings(out)
	return slices.Compact(out)
}
