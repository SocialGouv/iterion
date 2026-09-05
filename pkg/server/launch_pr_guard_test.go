package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/forge"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

type launchPullClient struct {
	pr  forge.PullRef
	err error
}

func (c launchPullClient) GetPullRequest(context.Context, string, int) (forge.PullRef, error) {
	return c.pr, c.err
}
func (c launchPullClient) SetCommitStatus(context.Context, string, string, forge.CommitStatus) error {
	panic("launch must not publish a status")
}

func TestPRLaunchRejectsUnprovenHead(t *testing.T) {
	for _, tc := range []struct {
		name, head string
		err        error
	}{
		{name: "fork", head: "contributor/r"},
		{name: "withheld"},
		{name: "forge-error", err: errors.New("forge unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newForgePublishTestServer(t)
			s.cfg.PublicURL = "https://iterion.example"
			s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) {
				return launchPullClient{pr: forge.PullRef{State: "open", HeadRepoFullName: tc.head, SourceBranch: "main"}, err: tc.err}, nil
			}
			vars := map[string]string{"pr_url": "https://github.com/o/r/pull/7"}
			_, err := s.applyPRLaunchContext(context.Background(), "team1", "", "fixer", vars, nil)
			if err == nil {
				t.Fatal("unverified PR launch was admitted")
			}
			// The registry, not the returned map: indexing the map proves
			// nothing when a refusal can hand back one that was never written.
			if n := len(s.forgePublishTokens.(*ForgePublishTokenRegistry).tokens); n != 0 {
				t.Fatalf("unverified PR received a launch grant (%d in the registry)", n)
			}
			// A failure to ASK is retryable; a decision about the PR is not.
			if got := prLaunchUnavailable(err); got != (tc.err != nil) {
				t.Fatalf("retryable = %v for %q, want %v", got, tc.name, tc.err != nil)
			}
		})
	}
}

func allowSameRepoLaunch(s *Server) {
	s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) {
		return launchPullClient{pr: forge.PullRef{State: "open", HeadRepoFullName: "o/r", SourceBranch: "topic"}}, nil
	}
}

func TestHTTPPRLaunchRejectsForkBeforeCreatingRun(t *testing.T) {
	s := newQueueOutageHTTPTestServer(t)
	installQueueOutageTestPublisher(t, s, errors.New("launch must not reach the publisher"))
	s.forgeConnections = forge.NewMemoryConnectionStore()
	if err := s.forgeConnections.Create(context.Background(), forge.Connection{
		ID: "conn1", TenantID: "team1", Provider: forge.ProviderGitHub,
	}); err != nil {
		t.Fatal(err)
	}
	lookups := 0
	s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) {
		lookups++
		return launchPullClient{pr: forge.PullRef{HeadRepoFullName: "contributor/r", SourceBranch: "topic"}}, nil
	}
	req := httptest.NewRequest(http.MethodPost, "/api/runs", strings.NewReader(`{
		"file_path":"guard.bot", "source":"workflow guard:\n  entry: done\n",
		"run_id":"fork-must-not-launch", "vars":{"pr_url":"https://github.com/o/r/pull/7"}
	}`))
	req = req.WithContext(auth.WithIdentity(req.Context(), auth.Identity{TeamID: "team1"}))
	rec := httptest.NewRecorder()
	s.handleLaunchRun(rec, req)
	if rec.Code != http.StatusUnprocessableEntity || lookups != 1 || !strings.Contains(rec.Body.String(), "fork") {
		t.Fatalf("status=%d lookups=%d body=%s", rec.Code, lookups, rec.Body.String())
	}
	if _, err := s.runs.RunStore().LoadRun(context.Background(), "fork-must-not-launch"); !errors.Is(err, store.ErrRunNotFound) {
		t.Fatalf("fork launch created a run: %v", err)
	}
}

// The no-team lane (local studio: DisableAuth synthesizes an identity with an
// EMPTY TeamID) applies the SAME fork predicate, resolved through the
// operator's own `forge_token` instead of a team Connection. #702's hazard
// does not depend on a team — the launch pair is still <base>.CloneURL + the
// PR head branch, and a fork branch silently resolves to a same-named branch
// on the base repo whether or not a publish grant was minted.
func TestHTTPLocalPRLaunchAppliesTheForkGuard(t *testing.T) {
	for _, tc := range []struct {
		name, head string
		wantRun    bool
	}{
		{name: "fork-refused", head: "contributor/r"},
		{name: "withheld-head-refused"},
		{name: "same-repo-launches", head: "o/r", wantRun: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newLocalForgeTokenTestServer(t, "ghp_local")
			lookups := 0
			s.forgeGateClientFor = func(_ context.Context, conn forge.Connection) (forgeGateClient, error) {
				lookups++
				if conn.Provider != forge.ProviderGitHub {
					t.Errorf("provider inferred from the PR URL = %q, want github", conn.Provider)
				}
				return launchPullClient{pr: forge.PullRef{State: "open", HeadRepoFullName: tc.head, SourceBranch: "topic"}}, nil
			}
			rec := localPRLaunch(t, s, tc.name)
			if lookups != 1 {
				t.Fatalf("head lookups = %d, want exactly 1", lookups)
			}
			if !tc.wantRun {
				if rec.Code != http.StatusUnprocessableEntity {
					t.Fatalf("unproven head admitted: status=%d body=%s", rec.Code, rec.Body.String())
				}
				assertNoRunPersisted(t, s, tc.name)
				return
			}
			if rec.Code/100 != 2 {
				t.Fatalf("proven same-repo PR refused: status=%d body=%s", rec.Code, rec.Body.String())
			}
			if _, err := s.runs.RunStore().LoadRun(context.Background(), tc.name); err != nil {
				t.Fatalf("same-repo launch created no run: %v", err)
			}
		})
	}
}

// Without a credential the local lane REFUSES and says what is missing — the
// one thing it must never do is admit the launch, which would leave the guard
// advertised and inert on the surface #702 was filed against.
func TestHTTPLocalPRLaunchWithoutCredentialExplains(t *testing.T) {
	s := newQueueOutageHTTPTestServer(t) // no local secret store wired
	s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) {
		t.Error("no credential is available — nothing should have been asked of the forge")
		return nil, errors.New("unreachable")
	}
	rec := localPRLaunch(t, s, "local-no-credential")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("uncredentialed launch was admitted: status=%d body=%s", rec.Code, rec.Body.String())
	}
	for _, want := range []string{"forge_token", "iterion secret set", "iterion run"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("refusal does not explain the limitation (missing %q): %s", want, rec.Body.String())
		}
	}
	assertNoRunPersisted(t, s, "local-no-credential")
}

// A forge_token pinned to another host is not sent to this one: the secret's
// own egress lock outranks a launch naming a PR elsewhere. With no usable
// credential left, the lane refuses rather than assumes.
func TestHTTPLocalPRLaunchHonoursTheTokenHostPin(t *testing.T) {
	s := newLocalForgeTokenTestServer(t, "glpat_other", "gitlab.example")
	s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) {
		t.Error("a token pinned to gitlab.example must not be used against github.com")
		return nil, errors.New("unreachable")
	}
	rec := localPRLaunch(t, s, "local-host-pinned-token")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("off-host token admitted the launch: status=%d body=%s", rec.Code, rec.Body.String())
	}
	assertNoRunPersisted(t, s, "local-host-pinned-token")
}

// An UNPINNED forge_token is not offered to an origin named by the request.
// The assertion that matters is an OUTBOUND one — the destination must record
// ZERO requests — because the hazard is a stored forge write credential
// leaving the machine, not the status code the operator reads. The destination
// is an HTTPS server the client trusts, so a refusal is attributable to the
// host-trust rule ALONE: over plain HTTP the scheme rule would refuse it too
// and neither check could be isolated.
func TestHTTPLocalPRLaunchWithholdsAnUnpinnedTokenFromAnUntrustedHost(t *testing.T) {
	s, forge, reqs := newLocalForgeEgressProbe(t) // unpinned token
	rec := localPRLaunchAt(t, s, "unpinned-untrusted-host", forge+"/o/r/pull/7")
	// The egress oracle FIRST: it is the property under test, and a status
	// assertion could pass on a launch that leaked the token and then failed
	// for some unrelated reason.
	if n := reqs.Load(); n != 0 {
		t.Fatalf("the unpinned forge_token was sent to a host named by the request (%d outbound call(s))", n)
	}
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a launch naming an unrecognised host was admitted: status=%d body=%s", rec.Code, rec.Body.String())
	}
	// The refusal must name the command that authorises the host, or an
	// operator who HAS a token reads "no credential" and adds a second one.
	for _, want := range []string{localForgeTokenSecret, "--hosts", "iterion secret set", "iterion run"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("refusal does not say how to authorise the host (missing %q): %s", want, rec.Body.String())
		}
	}
	assertNoRunPersisted(t, s, "unpinned-untrusted-host")
}

// The positive control for the test above, and the pinned lane's own coverage:
// the SAME origin, with the operator's explicit `--hosts` pin, DOES reach the
// forge — through the real forgeAdminForToken client, not the test seam. If
// this one ever stopped observing a request, the zero above would be measuring
// a broken probe rather than a withheld credential.
func TestHTTPLocalPRLaunchPinnedTokenReachesASelfHostedForge(t *testing.T) {
	s, forge, reqs := newLocalForgeEgressProbe(t, "PIN")
	rec := localPRLaunchAt(t, s, "pinned-self-hosted", forge+"/o/r/pull/7")
	if n := reqs.Load(); n != 1 {
		t.Fatalf("a pinned host must be reachable: outbound calls = %d", n)
	}
	if rec.Code/100 != 2 {
		t.Fatalf("a same-repo PR on a pinned self-hosted forge was refused: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if _, err := s.runs.RunStore().LoadRun(context.Background(), "pinned-self-hosted"); err != nil {
		t.Fatalf("pinned same-repo launch created no run: %v", err)
	}
}

// The trust set is EXACT origins, so the two shapes that read as trusted at a
// glance are not: a lookalike suffix, and an http:// scheme on a real SaaS
// host (GitLab's and Forgejo's constructors keep the scheme verbatim, so that
// one genuinely puts the token on the wire in plaintext). Both fail closed to
// "needs a pin", and neither reaches the forge seam at all.
func TestHTTPLocalPRLaunchUnpinnedTrustIsExact(t *testing.T) {
	for name, prURL := range map[string]string{
		"suffix-lookalike":  "https://github.com.evil.io/o/r/pull/7",
		"http-gitlab":       "http://gitlab.com/o/r/-/merge_requests/7",
		"www-alias":         "https://www.github.com/o/r/pull/7",
		"port-on-saas-host": "https://github.com:8443/o/r/pull/7",
	} {
		t.Run(name, func(t *testing.T) {
			s := newLocalForgeTokenTestServer(t, "ghp_local")
			s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) {
				t.Error("no credential is authorised for this origin — nothing should have been asked of a forge")
				return nil, errors.New("unreachable")
			}
			rec := localPRLaunchAt(t, s, name, prURL)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("%s was treated as a trusted origin: status=%d body=%s", prURL, rec.Code, rec.Body.String())
			}
			assertNoRunPersisted(t, s, name)
		})
	}
}

func TestLocalTokenPermittedAt(t *testing.T) {
	for _, tc := range []struct {
		name    string
		allowed []string
		origin  string
		host    string
		want    bool
	}{
		{name: "unpinned/github", origin: "https://github.com", host: "github.com", want: true},
		{name: "unpinned/gitlab", origin: "https://gitlab.com", host: "gitlab.com", want: true},
		{name: "unpinned/codeberg", origin: "https://codeberg.org", host: "codeberg.org", want: true},
		{name: "unpinned/uppercase-origin", origin: "https://GitHub.com", host: "github.com", want: true},
		{name: "unpinned/http-github", origin: "http://github.com", host: "github.com"},
		{name: "unpinned/suffix-lookalike", origin: "https://github.com.evil.io", host: "github.com.evil.io"},
		{name: "unpinned/subdomain", origin: "https://api.github.com", host: "api.github.com"},
		{name: "unpinned/self-hosted", origin: "https://ghe.example.com", host: "ghe.example.com"},
		{name: "unpinned/port", origin: "https://github.com:8443", host: "github.com:8443"},
		// A pin is the operator's own authorisation: it carries the origin,
		// scheme included, wherever they point it.
		{name: "pinned/exact", allowed: []string{"ghe.example.com"}, origin: "https://ghe.example.com", host: "ghe.example.com", want: true},
		{name: "pinned/parent-domain", allowed: []string{"example.com"}, origin: "https://ghe.example.com", host: "ghe.example.com", want: true},
		{name: "pinned/http", allowed: []string{"git.internal"}, origin: "http://git.internal", host: "git.internal", want: true},
		{name: "pinned/elsewhere", allowed: []string{"gitlab.example"}, origin: "https://github.com", host: "github.com"},
		// A pin NARROWS, it never widens: naming another forge does not
		// re-admit a trusted SaaS origin.
		{name: "pinned/does-not-inherit-saas-trust", allowed: []string{"ghe.example.com"}, origin: "https://github.com", host: "github.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := localTokenPermittedAt(tc.allowed, tc.origin, tc.host); got != tc.want {
				t.Fatalf("localTokenPermittedAt(%v, %q, %q) = %v, want %v", tc.allowed, tc.origin, tc.host, got, tc.want)
			}
		})
	}
}

func TestProviderForPullURL(t *testing.T) {
	for _, tc := range []struct {
		raw          string
		want         forge.Provider
		wantBase     string
		wantResolved bool
	}{
		{raw: "https://github.com/o/r/pull/7", want: forge.ProviderGitHub, wantBase: "https://github.com", wantResolved: true},
		{raw: "https://gitlab.com/o/r/-/merge_requests/7", want: forge.ProviderGitLab, wantBase: "https://gitlab.com", wantResolved: true},
		{raw: "https://codeberg.org/o/r/pulls/7", want: forge.ProviderForgejo, wantBase: "https://codeberg.org", wantResolved: true},
		// Self-hosted: the host says nothing, the PR path shape says everything.
		{raw: "https://ghe.example.com/o/r/pull/7", want: forge.ProviderGitHub, wantBase: "https://ghe.example.com", wantResolved: true},
		{raw: "https://gl.example.com/g/o/r/-/merge_requests/7", want: forge.ProviderGitLab, wantBase: "https://gl.example.com", wantResolved: true},
		{raw: "https://git.example.com/o/r/pulls/7", want: forge.ProviderForgejo, wantBase: "https://git.example.com", wantResolved: true},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			host, _, _, err := forge.ParsePullURL(tc.raw)
			if err != nil {
				t.Fatalf("ParsePullURL: %v", err)
			}
			got, base, ok := providerForPullURL(tc.raw, host)
			if ok != tc.wantResolved || got != tc.want || base != tc.wantBase {
				t.Fatalf("got (%q, %q, %v), want (%q, %q, %v)", got, base, ok, tc.want, tc.wantBase, tc.wantResolved)
			}
		})
	}
}

// newLocalForgeTokenTestServer is the local studio's shape: no team, no forge
// connections, one operator-held `forge_token` in the layered file store.
func newLocalForgeTokenTestServer(t *testing.T, token string, hosts ...string) *Server {
	t.Helper()
	dir := t.TempDir()
	global, err := secrets.NewFileGenericSecretStore(filepath.Join(dir, "secrets.json"))
	if err != nil {
		t.Fatalf("global secret store: %v", err)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	sealer, err := secrets.NewAESGCMSealer(key)
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	if _, _, err := global.UpsertByName(sealer, localForgeTokenSecret, token, hosts, len(hosts) > 0); err != nil {
		t.Fatalf("seed %s: %v", localForgeTokenSecret, err)
	}
	workDir := t.TempDir()
	s := New(Config{
		WorkDir:                 workDir,
		StoreDir:                filepath.Join(workDir, ".iterion"),
		SkipProjectRegistration: true,
		GenericSecrets:          secrets.NewLayeredGenericSecretStore(global, nil),
		Sealer:                  sealer,
		DisableAuth:             true,
	}, iterlog.Nop())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	})
	// Guard the premise: without the layered store the lane below would refuse
	// for want of a credential and every assertion would be about that instead.
	if s.localSecrets == nil {
		t.Fatal("server did not wire the local layered secret store")
	}
	return s
}

// newLocalForgeEgressProbe is the local studio wired against a REAL HTTPS
// forge it can reach, so "the credential did not leave the machine" is proven
// by the destination's own request count rather than by a stub that was never
// called. Pass "PIN" to pin the forge_token to the probe's host:port (the
// operator's explicit authorisation), or nothing for the unpinned default
// `iterion secret set` produces.
//
// forgeGateClientFor is deliberately left nil: the lookup must go through the
// real forgeAdminForToken client, which is the code that actually attaches
// `Authorization: Bearer <forge_token>`.
func newLocalForgeEgressProbe(t *testing.T, pin ...string) (*Server, string, *atomic.Int64) {
	t.Helper()
	var reqs atomic.Int64
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs.Add(1)
		if got := r.Header.Get("Authorization"); got == "" {
			t.Errorf("the probe received an UNAUTHENTICATED request — it cannot witness a credential leak")
		}
		w.Header().Set("Content-Type", "application/json")
		// Same-repo, so the guard's verdict is "admitted" and a refusal in
		// these tests can only come from the egress rule under test.
		_, _ = w.Write([]byte(`{"number":7,"state":"open","head":{"ref":"topic","sha":"abc","repo":{"full_name":"o/r"}},"base":{"ref":"main"}}`))
	}))
	t.Cleanup(ts.Close)
	host := strings.TrimPrefix(ts.URL, "https://")

	var hosts []string
	if len(pin) > 0 {
		hosts = []string{host}
	}
	s := newLocalForgeTokenTestServer(t, "ghp_local", hosts...)
	// Trust the probe's self-signed cert by burning the once-guard before the
	// server can build its own client. Nothing else about the outbound path is
	// replaced — the admin client, its headers and its API base are the real
	// ones.
	s.forgeHTTPOnce.Do(func() {})
	s.forgeHTTP = ts.Client()
	return s, ts.URL, &reqs
}

// localPRLaunch drives POST /api/runs with EXACTLY the identity requireAuth
// synthesizes under DisableAuth: a super-admin with no team.
func localPRLaunch(t *testing.T, s *Server, runID string) *httptest.ResponseRecorder {
	t.Helper()
	return localPRLaunchAt(t, s, runID, "https://github.com/o/r/pull/7")
}

func localPRLaunchAt(t *testing.T, s *Server, runID, prURL string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"file_path": "guard.bot",
		"source":    "workflow guard:\n  entry: done\n",
		"run_id":    runID,
		"vars":      map[string]string{"pr_url": prURL},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/runs", bytes.NewReader(body))
	req = req.WithContext(auth.WithIdentity(req.Context(), auth.Identity{UserID: "dev", IsSuperAdmin: true}))
	rec := httptest.NewRecorder()
	s.handleLaunchRun(rec, req)
	return rec
}

func assertNoRunPersisted(t *testing.T, s *Server, runID string) {
	t.Helper()
	if _, err := s.runs.RunStore().LoadRun(context.Background(), runID); !errors.Is(err, store.ErrRunNotFound) {
		t.Fatalf("refused launch created a run: %v", err)
	}
}
