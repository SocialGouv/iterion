package server

import (
	"context"
	"crypto/rand"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
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
			out, err := s.applyPRLaunchContext(context.Background(), "team1", "", "fixer", vars, nil)
			if err == nil {
				t.Fatal("unverified PR launch was admitted")
			}
			if out[forgePublishVarToken] != "" {
				t.Fatal("unverified PR received a launch grant")
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

// localPRLaunch drives POST /api/runs with EXACTLY the identity requireAuth
// synthesizes under DisableAuth: a super-admin with no team.
func localPRLaunch(t *testing.T, s *Server, runID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/runs", strings.NewReader(`{
		"file_path":"guard.bot", "source":"workflow guard:\n  entry: done\n",
		"run_id":"`+runID+`", "vars":{"pr_url":"https://github.com/o/r/pull/7"}
	}`))
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
