package server

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/auth/desktopsso"
	"github.com/SocialGouv/iterion/pkg/auth/oidc"
	"github.com/SocialGouv/iterion/pkg/auth/orgsso"
	"github.com/SocialGouv/iterion/pkg/auth/wsticket"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/pat"
)

// sentinelRoots are the packages behind the identity routes and requireAuth:
// the auth service — with the oidc, orgsso, desktopsso and wsticket packages
// under it — the identity store and the PAT store, as directories under pkg/.
// Each is walked with every package below it, so a sentinel a new
// sub-package declares is scanned like any other.
var sentinelRoots = []string{"auth", "identity", "pat"}

// sentinelStatus is the status mapAuthErrorStatus owes every sentinel error
// a route can hand it: a refusal below 500, a failure 500. A key names the
// declaring package by its directory under pkg/.
var sentinelStatus = map[string]struct {
	err    error
	status int
}{
	"auth.ErrInvalidCredentials":     {auth.ErrInvalidCredentials, http.StatusUnauthorized},
	"auth.ErrAccountDisabled":        {auth.ErrAccountDisabled, http.StatusUnauthorized},
	"auth.ErrSessionNotFound":        {auth.ErrSessionNotFound, http.StatusUnauthorized},
	"auth.ErrSessionRevoked":         {auth.ErrSessionRevoked, http.StatusUnauthorized},
	"auth.ErrSessionExpired":         {auth.ErrSessionExpired, http.StatusUnauthorized},
	"auth.ErrResetNotFound":          {auth.ErrResetNotFound, http.StatusUnauthorized},
	"auth.ErrTokenExpired":           {auth.ErrTokenExpired, http.StatusUnauthorized},
	"auth.ErrTokenInvalid":           {auth.ErrTokenInvalid, http.StatusUnauthorized},
	"auth.ErrTokenRevoked":           {auth.ErrTokenRevoked, http.StatusUnauthorized},
	"auth.ErrPasswordWeak":           {auth.ErrPasswordWeak, http.StatusBadRequest},
	"auth.ErrSignupClosed":           {auth.ErrSignupClosed, http.StatusBadRequest},
	"auth.ErrInvitationMismatch":     {auth.ErrInvitationMismatch, http.StatusBadRequest},
	"auth.ErrPasswordChangeRequired": {auth.ErrPasswordChangeRequired, http.StatusForbidden},
	"auth.ErrSSORestricted":          {auth.ErrSSORestricted, http.StatusForbidden},
	"auth.ErrNotAMember":             {auth.ErrNotAMember, http.StatusForbidden},
	"auth.ErrNotAnOrgMember":         {auth.ErrNotAnOrgMember, http.StatusForbidden},
	"auth.ErrNoTeamInOrg":            {auth.ErrNoTeamInOrg, http.StatusForbidden},
	"auth.ErrInvitationNotFound":     {auth.ErrInvitationNotFound, http.StatusNotFound},
	"auth.ErrTeamNotFound":           {auth.ErrTeamNotFound, http.StatusNotFound},
	"auth.ErrOrgNotFound":            {auth.ErrOrgNotFound, http.StatusNotFound},
	"auth.ErrLinkRequiresConsent":    {auth.ErrLinkRequiresConsent, http.StatusConflict},
	"auth.ErrLinkAlreadyOwned":       {auth.ErrLinkAlreadyOwned, http.StatusConflict},
	"auth.ErrNotDerivable":           {auth.ErrNotDerivable, http.StatusForbidden},
	// Failures: a token hash the store already holds is a collision Refresh
	// resolves itself; a stored password hash the server cannot parse is the
	// server's own state.
	"auth.ErrSessionExists":       {auth.ErrSessionExists, http.StatusInternalServerError},
	"auth.ErrInvalidPasswordHash": {auth.ErrInvalidPasswordHash, http.StatusInternalServerError},

	"identity.ErrNotFound":            {identity.ErrNotFound, http.StatusNotFound},
	"identity.ErrInvalidRole":         {identity.ErrInvalidRole, http.StatusBadRequest},
	"identity.ErrEmailAlreadyTaken":   {identity.ErrEmailAlreadyTaken, http.StatusConflict},
	"identity.ErrSlugAlreadyTaken":    {identity.ErrSlugAlreadyTaken, http.StatusConflict},
	"identity.ErrOrgSlugAlreadyTaken": {identity.ErrOrgSlugAlreadyTaken, http.StatusConflict},
	"identity.ErrRunnerPoolHeld":      {identity.ErrRunnerPoolHeld, http.StatusConflict},
	"identity.ErrUserStatusChanged":   {identity.ErrUserStatusChanged, http.StatusConflict},
	"identity.ErrInvitationUsed":      {identity.ErrInvitationUsed, http.StatusConflict},
	"identity.ErrInvitationExpired":   {identity.ErrInvitationExpired, http.StatusGone},
}

// sentinelAnsweredElsewhere lists the sentinels of the scanned packages that
// no caller hands to authError or to any other mapAuthErrorStatus call: each
// is answered by the sites named beside it. A sentinel that gains a path to
// one of those calls moves to sentinelStatus with the status it owes.
var sentinelAnsweredElsewhere = map[string]struct {
	err   error
	where string
}{
	"auth/desktopsso.ErrTicketNotFound": {desktopsso.ErrTicketNotFound, "handleDesktopExchange: 401 invalid or expired ticket"},
	"auth/wsticket.ErrTicketNotFound":   {wsticket.ErrTicketNotFound, "wsTicketIdentity: errWSTicketRefused, the 401 of requireAuth's WS-ticket branch"},
	"auth/oidc.ErrUnknownProvider":      {oidc.ErrUnknownProvider, "resolveConnector and LoginWithExternalForOrg: 404 at /start, an sso_error redirect at the callback, 400 on the GitHub-orgs routes"},
	"auth/oidc.ErrProviderDisabled":     {oidc.ErrProviderDisabled, "resolveConnector: 404 at /start, sso_error=disabled at the callback, 400 on the GitHub-orgs routes"},
	"auth/oidc.ErrStateNotFound":        {oidc.ErrStateNotFound, "the OIDC state stores' Take: sso_error=state_expired at the callback"},
	"auth/oidc.ErrEmailMissing":         {oidc.ErrEmailMissing, "the connectors' ExchangeCode and the SSO logins: an sso_error redirect at the callback, identityFailure on the GitHub-orgs callback"},
	"auth/oidc.ErrEmailNotVerified":     {oidc.ErrEmailNotVerified, "the Google and generic connectors' ExchangeCode: sso_error=exchange_failed at the callback"},
	"auth/orgsso.ErrNotFound":           {orgsso.ErrNotFound, "the org SSO provider routes (loadTenantOrgSSORow: 404) and the SSO logins and resolver, which turn it into oidc.ErrUnknownProvider"},
	"auth/orgsso.ErrExists":             {orgsso.ErrExists, "writeOrgSSOError: 409"},
	"auth/orgsso.ErrInvalid":            {orgsso.ErrInvalid, "writeOrgSSOError: 400"},
	"auth/orgsso.ErrOwnerNotGrant":      {orgsso.ErrOwnerNotGrant, "writeOrgSSOError: 400"},
	"auth/orgsso.ErrDomainNotFound":     {orgsso.ErrDomainNotFound, "the org SSO domain routes: 404 domain not found"},
	"auth/orgsso.ErrDomainExists":       {orgsso.ErrDomainExists, "handleCreateOrgSSODomain: 409"},
	"auth.ErrLastSignInMethod":          {auth.ErrLastSignInMethod, "handleUnlinkMySSO: 409 last_sign_in_method"},
	"auth/orgsso.ErrDomainInvalid":      {orgsso.ErrDomainInvalid, "the domain stores' Create, behind handleCreateOrgSSODomain's own 400 for an empty domain"},
	"pat.ErrNotFound":                   {pat.ErrNotFound, "identityFromPAT (patReadFailure: a 401 refusal) and handleRevokePAT (204)"},
	"auth.ErrPasswordChangedSignIn":     {auth.ErrPasswordChangedSignIn, "credentialChangeError: 409 password_changed_sign_in, the cookies cleared"},
	"auth.ErrCredentialChangeBusy":      {auth.ErrCredentialChangeBusy, "credentialChangeError and handleAdminResetUserPassword: 409 credential_change_busy"},
	"identity.ErrUserPasswordChanged":   {identity.ErrUserPasswordChanged, "the password sign-in's stamp and the credential flows' commits: auth.ErrInvalidCredentials"},
}

// isSentinelName reports whether name is a sentinel error's name: Err or
// Error followed by an upper-case word (ErrNotFound, ErrorUnknownKind). The
// prefix alone is not the tell — Errand and Errors are no sentinels — so the
// rune after the prefix decides.
func isSentinelName(name string) bool {
	for _, prefix := range []string{"Error", "Err"} {
		if rest, ok := strings.CutPrefix(name, prefix); ok && rest != "" && unicode.IsUpper([]rune(rest)[0]) {
			return true
		}
	}
	return false
}

// holdsError reports whether the i-th name of a package-level var spec holds
// an error whatever that name: its declared type is `error`, or its value is
// an errors.New or fmt.Errorf call through whatever name the file imports
// the package under.
func holdsError(vs *ast.ValueSpec, i int, imports map[string]string) bool {
	if typ, ok := vs.Type.(*ast.Ident); ok && typ.Name == "error" {
		return true
	}
	if len(vs.Values) != len(vs.Names) {
		return false
	}
	call, ok := vs.Values[i].(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	path := imports[pkg.Name]
	return (path == "errors" && sel.Sel.Name == "New") || (path == "fmt" && sel.Sel.Name == "Errorf")
}

// fileImports maps the names a file refers to its imports by to their paths.
func fileImports(file *ast.File) map[string]string {
	out := map[string]string{}
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		name := path[strings.LastIndex(path, "/")+1:]
		if imp.Name != nil {
			name = imp.Name.Name
		}
		out[name] = path
	}
	return out
}

// declaredSentinels lists, per package, the exported sentinel errors the
// non-test sources of root and of every package below it declare, keyed by
// the package's directory under pkg/ ("auth", "auth/oidc"). A sentinel is a
// package-level var with a sentinel's name, WHATEVER its value expression, or
// one that holds an error whatever its name.
//
// The name is the main tell, deliberately. A scan keyed on the value alone —
// an `errors.New` call reached through an import spelled `errors` — is green
// for every other way a sentinel is written: fmt.Errorf (a sentinel with a
// format-free message), an aliased import (stderrors.New), a value of the
// package's own error type (`var ErrX error = &xErr{}`). The type and value
// tells add what the name misses: a sentinel named outside the convention.
// Each form is one this test must see, because a sentinel nobody classified
// is answered as a store outage (500 "identity unavailable", an ERROR log
// line) instead of as the refusal it is.
func declaredSentinels(t *testing.T, root string) map[string][]string {
	t.Helper()
	base := filepath.Join("..", root)
	fset := token.NewFileSet()
	out := map[string][]string{}
	err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			// The go tool builds no package from these directories.
			if path != base && (name == "testdata" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		dir, err := filepath.Rel("..", filepath.Dir(path))
		if err != nil {
			return err
		}
		pkg := filepath.ToSlash(dir)
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		imports := fileImports(file)
		for _, decl := range file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, id := range vs.Names {
					if id.IsExported() && (isSentinelName(id.Name) || holdsError(vs, i, imports)) {
						out[pkg] = append(out[pkg], id.Name)
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan pkg/%s: %v", root, err)
	}
	return out
}

// TestMapAuthErrorStatus_EverySentinelIsClassified: every sentinel error the
// scanned packages declare is either classified — it maps to the status it
// owes, a refusal to its 4xx with its own message, a failure to 500, bare or
// wrapped — or exempted with the site that answers it instead. A sentinel
// declared and left out of both tables fails here: an unmapped refusal is
// answered as a store outage ("identity unavailable", an ERROR log line)
// until someone classifies it.
func TestMapAuthErrorStatus_EverySentinelIsClassified(t *testing.T) {
	found := map[string]bool{}
	for _, root := range sentinelRoots {
		pkgs := declaredSentinels(t, root)
		if len(pkgs) == 0 {
			t.Fatalf("found no sentinel declared under pkg/%s: the scan reads nothing", root)
		}
		for pkg, names := range pkgs {
			for _, n := range names {
				key := pkg + "." + n
				found[key] = true
				_, classified := sentinelStatus[key]
				_, exempt := sentinelAnsweredElsewhere[key]
				switch {
				case classified && exempt:
					t.Errorf("%s is both classified and exempted: keep the one that holds", key)
				case !classified && !exempt:
					t.Errorf("%s is declared but not classified: add it to sentinelStatus with the status it owes, or to sentinelAnsweredElsewhere with the site that answers it", key)
				}
			}
		}
	}
	// The per-package guard: every package a table names must yield, under
	// the name the table gives, each sentinel it lists — a package the walk
	// skips or a name the scan cannot see fails here instead of passing as
	// "nothing to classify". Two keys holding one value would leave a
	// declared sentinel's own value untested. A value of a type that cannot
	// be compared is no sentinel at all: errors.Is never matches it by
	// identity.
	seen := map[error]string{}
	check := func(key string, err error) {
		if !found[key] {
			t.Errorf("%s is in a table but the scan did not find it: the scan is blind to its package or its name", key)
		}
		if !reflect.TypeOf(err).Comparable() {
			t.Errorf("%s holds a %T, which errors.Is cannot match by identity: a sentinel needs a comparable value", key, err)
			return
		}
		if other, dup := seen[err]; dup {
			t.Errorf("%s and %s hold the same error value: a key names the wrong sentinel", key, other)
		}
		seen[err] = key
	}
	for key, tc := range sentinelStatus {
		check(key, tc.err)
	}
	for key, tc := range sentinelAnsweredElsewhere {
		check(key, tc.err)
		if tc.where == "" {
			t.Errorf("%s is exempted without the site that answers it", key)
		}
	}
	for name, tc := range sentinelStatus {
		if got := mapAuthErrorStatus(tc.err); got != tc.status {
			t.Errorf("%s maps to %d, want %d", name, got, tc.status)
		}
		if got := mapAuthErrorStatus(fmt.Errorf("a caller's context: %w", tc.err)); got != tc.status {
			t.Errorf("%s wrapped maps to %d, want %d", name, got, tc.status)
		}
	}
}

// TestHandleSwitchOrg_RefusalsAreRefusals drives the org switch's two
// refusals through the route: a user who is not a member of the org, and a
// super-admin naming an org that does not exist. Each is answered with its
// own status and message — and logs no ERROR, which an operator reads as a
// store outage.
func TestHandleSwitchOrg_RefusalsAreRefusals(t *testing.T) {
	f := newAuthRouteFixture(t)
	seedOrg(t, f.s, "o2", "org-two")
	for _, tc := range []struct {
		name   string
		id     auth.Identity
		org    string
		status int
		want   error
	}{
		{"a non-member", auth.Identity{UserID: f.user.User.ID, Credential: auth.CredentialAccessToken}, "o2", http.StatusForbidden, auth.ErrNotAnOrgMember},
		{"a super-admin naming no org", auth.Identity{UserID: f.user.User.ID, IsSuperAdmin: true, Credential: auth.CredentialAccessToken}, "o-missing", http.StatusNotFound, auth.ErrOrgNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.id.IsSuperAdmin {
				if _, err := f.s.authStore().PatchUser(context.Background(), f.user.User.ID, identity.UserPatch{IsSuperAdmin: ptrTo(true)}); err != nil {
					t.Fatalf("promote: %v", err)
				}
			}
			before := f.logs.String()
			r := httptest.NewRequest("POST", "/api/auth/me/org/"+tc.org, nil)
			r.SetPathValue("org_id", tc.org)
			r = r.WithContext(auth.WithIdentity(context.Background(), tc.id))
			w := httptest.NewRecorder()
			f.s.handleSwitchOrg(w, r)
			if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.want.Error()) {
				t.Fatalf("the switch answered %d %s, want %d with %q", w.Code, w.Body.String(), tc.status, tc.want)
			}
			if logged := strings.TrimPrefix(f.logs.String(), before); logged != "" {
				t.Fatalf("a refusal wrote to the server log: %q", logged)
			}
		})
	}
}

// TestHandleCreateTeam_ARefusalIsNotAFailure: creating a team in an org
// that does not exist is refused (404); the same read FAILING is a failure
// (500, the generic body, the cause logged) — not an org that is not there.
func TestHandleCreateTeam_ARefusalIsNotAFailure(t *testing.T) {
	create := func(f *authRouteFixture, org string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/teams", strings.NewReader(fmt.Sprintf(`{"name":"New team","org_id":%q}`, org)))
		r = r.WithContext(auth.WithIdentity(context.Background(), auth.Identity{UserID: f.user.User.ID, IsSuperAdmin: true, Credential: auth.CredentialAccessToken}))
		w := httptest.NewRecorder()
		f.s.handleCreateTeam(w, r)
		return w
	}
	f := newAuthRouteFixture(t)
	if _, err := f.s.authStore().PatchUser(context.Background(), f.user.User.ID, identity.UserPatch{IsSuperAdmin: ptrTo(true)}); err != nil {
		t.Fatalf("promote: %v", err)
	}
	if w := create(f, "o-missing"); w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), auth.ErrOrgNotFound.Error()) {
		t.Fatalf("a team in a missing org answered %d %s, want 404 %q", w.Code, w.Body.String(), auth.ErrOrgNotFound)
	}
	f.ids.fail("GetOrg", 0)
	assertIdentityUnavailable(t, create(f, "o1"), f.logs, "a team creation whose org read fails")
}

func ptrTo[T any](v T) *T { return &v }

// manualClock is a clock a test moves by hand.
type manualClock struct {
	mu  sync.Mutex
	now time.Time
}

func newManualClock(at time.Time) *manualClock { return &manualClock{now: at} }

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manualClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}
