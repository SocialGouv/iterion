package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// The App client is the PRODUCTION shape of a GitHub connection, so the
// PullClient capability the card's PR/CI panel asserts must exist on it too —
// through the same per-profile scoped tokens as the issue capability, never
// the runtime baseline.
var _ forge.PullClient = (*AppClient)(nil)

// pullMintRecorder is a fake GitHub serving the installation-token mint, the
// installation probe and the pull/CI endpoints. It records the permission set
// of every mint, the bearer of every REST call and the full request line, and
// applies GitHub's two refusals on demand: a mint outside the installation's
// grant (422) and a call the bearer lacks the permission for (403).
type pullMintRecorder struct {
	mu sync.Mutex
	// granted is what the installation approved; nil = accept every mint.
	granted map[string]string
	// refuseMint forces the 422 regardless of granted.
	refuseMint bool
	// notAccessible names URL-path suffixes refused with GitHub's 403
	// "Resource not accessible by integration" whatever the mint carried.
	notAccessible map[string]bool
	mints         []map[string]string
	bearers       []string
	paths         []string
	srv           *httptest.Server
}

func newPullMintRecorder(t *testing.T) *pullMintRecorder {
	t.Helper()
	r := &pullMintRecorder{notAccessible: map[string]bool{}}
	pr := map[string]any{
		"number": 7, "title": "feat: x (closes #12)", "state": "open", "html_url": "https://gh/o/r/pull/7",
		"head": map[string]any{"ref": "feat/x", "sha": "deadbeef", "repo": map[string]any{"full_name": "o/r"}},
		"base": map[string]any{"ref": "main"}, "user": map[string]any{"login": "dave"},
	}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(req.URL.Path, "/access_tokens") {
			var body struct {
				Permissions map[string]string `json:"permissions"`
			}
			_ = json.NewDecoder(req.Body).Decode(&body)
			r.mu.Lock()
			defer r.mu.Unlock()
			refused := r.refuseMint
			for name, level := range body.Permissions {
				got, ok := r.granted[name]
				if r.granted != nil && (!ok || (level == "write" && got != "write" && got != "admin")) {
					refused = true
				}
			}
			if refused {
				w.WriteHeader(http.StatusUnprocessableEntity)
				_ = json.NewEncoder(w).Encode(map[string]any{"message": "The permissions requested are not granted to this installation."})
				return
			}
			r.mints = append(r.mints, body.Permissions)
			n := len(r.mints)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token":      "ghs_scoped_" + string(rune('a'+n-1)),
				"expires_at": "2099-01-01T00:00:00Z",
			})
			return
		}
		if strings.Contains(req.URL.Path, "/app/installations/") {
			r.mu.Lock()
			granted := r.granted
			r.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"account":     map[string]any{"login": "acme"},
				"html_url":    "https://gh/organizations/acme/settings/installations/99",
				"permissions": granted,
			})
			return
		}
		r.mu.Lock()
		r.bearers = append(r.bearers, req.Header.Get("Authorization"))
		r.paths = append(r.paths, req.Method+" "+req.URL.RequestURI())
		refuse := false
		for suffix := range r.notAccessible {
			if strings.HasSuffix(req.URL.Path, suffix) {
				refuse = true
			}
		}
		r.mu.Unlock()
		if refuse {
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]any{"message": "Resource not accessible by integration", "documentation_url": "https://docs.github.com/rest"})
			return
		}
		switch {
		case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/check-runs"):
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "check_runs": []map[string]any{
				{"name": "build", "status": "completed", "conclusion": "success", "html_url": "https://gh/o/r/runs/1", "started_at": "2026-01-01T00:00:00Z"},
			}})
		case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/status"):
			_ = json.NewEncoder(w).Encode(map[string]any{"state": "success", "sha": "deadbeef", "statuses": []map[string]any{}})
		case req.Method == http.MethodPut && strings.HasSuffix(req.URL.Path, "/merge"):
			_ = json.NewEncoder(w).Encode(map[string]any{"merged": true, "sha": "cafe"})
		case req.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/pulls"):
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(pr)
		case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/pulls"):
			_ = json.NewEncoder(w).Encode([]map[string]any{pr})
		default:
			_ = json.NewEncoder(w).Encode(pr)
		}
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *pullMintRecorder) snapshot() ([]map[string]string, []string, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]map[string]string(nil), r.mints...),
		append([]string(nil), r.bearers...),
		append([]string(nil), r.paths...)
}

func (r *pullMintRecorder) appClient(t *testing.T) *AppClient {
	t.Helper()
	return &AppClient{
		HTTP: r.srv.Client(), WebBaseURL: r.srv.URL,
		Cfg: AppConfig{AppID: 42, PrivateKeyPEM: testKeyPEMOnce(t), AppSlug: "iterion"}, InstallationID: 99,
	}
}

func (r *pullMintRecorder) patClient() *AdminClient {
	return &AdminClient{HTTP: r.srv.Client(), APIBase: APIBaseFor(r.srv.URL), Token: "ghp_pat"}
}

// pullCalls is every PullClient method paired with the permission profile its
// call is gated on — the profile is GitHub's rule for the endpoint, taken from
// its published permission data, not a guess.
var pullCalls = []struct {
	name string
	call func(ctx context.Context, c forge.PullClient) error
	want map[string]string
}{
	{"ListPullRequests", func(ctx context.Context, c forge.PullClient) error {
		_, err := c.ListPullRequests(ctx, "o/r", forge.PullListOptions{State: "all", PerPage: 100})
		return err
	}, PullListInstallationPermissions()},
	{"GetPullRequest", func(ctx context.Context, c forge.PullClient) error {
		_, err := c.GetPullRequest(ctx, "o/r", 7)
		return err
	}, PullGetInstallationPermissions()},
	{"CreatePull", func(ctx context.Context, c forge.PullClient) error {
		_, err := c.CreatePull(ctx, "o/r", forge.NewPull{Title: "t", SourceBranch: "feat/x", TargetBranch: "main"})
		return err
	}, PullWriteInstallationPermissions()},
	{"UpdatePull", func(ctx context.Context, c forge.PullClient) error {
		title := "renamed"
		_, err := c.UpdatePull(ctx, "o/r", 7, forge.PullPatch{Title: &title})
		return err
	}, PullWriteInstallationPermissions()},
	{"MergePull", func(ctx context.Context, c forge.PullClient) error {
		_, err := c.MergePull(ctx, "o/r", 7, forge.MergeOptions{Method: forge.MergeSquash, DeleteBranch: true})
		return err
	}, PullMergeInstallationPermissions()},
	{"GetCIStatus", func(ctx context.Context, c forge.PullClient) error {
		_, err := c.GetCIStatus(ctx, "o/r", "deadbeef")
		return err
	}, CIStatusInstallationPermissions()},
	{"ListCIHistory", func(ctx context.Context, c forge.PullClient) error {
		_, err := c.ListCIHistory(ctx, "o/r", "deadbeef", 20)
		return err
	}, CIHistoryInstallationPermissions()},
}

// Each PullClient method mints EXACTLY its own profile — nothing rides the
// runtime baseline, no read acquires a write — and issues the very requests
// the PAT client issues (method + URI, query included; MergePull's three-call
// sequence too), so the two connection kinds are interchangeable on the wire.
func TestAppClientPullMethodsMintTheirOwnProfile(t *testing.T) {
	for _, tc := range pullCalls {
		t.Run(tc.name, func(t *testing.T) {
			r := newPullMintRecorder(t)
			ctx := context.Background()
			if err := tc.call(ctx, r.patClient()); err != nil {
				t.Fatalf("PAT %s: %v", tc.name, err)
			}
			if err := tc.call(ctx, r.appClient(t)); err != nil {
				t.Fatalf("App %s: %v", tc.name, err)
			}
			mints, bearers, paths := r.snapshot()
			if len(mints) != 1 {
				t.Fatalf("mints = %d, want exactly 1 (the PAT call mints nothing)", len(mints))
			}
			if !reflect.DeepEqual(mints[0], tc.want) {
				t.Errorf("minted permissions = %v, want the %s profile %v", mints[0], tc.name, tc.want)
			}
			if len(paths)%2 != 0 || len(paths) == 0 {
				t.Fatalf("paths = %v, want the App half to mirror the PAT half", paths)
			}
			half := len(paths) / 2
			for i := 0; i < half; i++ {
				if paths[i] != paths[half+i] {
					t.Errorf("request %d: PAT %q vs App %q — the delegation must reproduce the PAT client's own request", i, paths[i], paths[half+i])
				}
				if bearers[i] != "Bearer ghp_pat" || !strings.HasPrefix(bearers[half+i], "Bearer ghs_scoped_") {
					t.Errorf("bearers = %v, want the PAT then the minted installation token", bearers)
				}
			}
		})
	}
}

// The profiles are their own, exactly as narrow as the endpoint's rule, and
// every one of them is REQUESTABLE by an App the manifest creates today —
// a profile the manifest does not cover could never be minted on a fresh
// installation. `checks` stays out of the runtime baseline on purpose: the
// management and run tokens are minted from it, and a baseline the
// installation cannot serve fails EVERY mint for that installation (422),
// not just the CI panel.
func TestPullPermissionProfiles(t *testing.T) {
	exact := map[string]map[string]string{
		"list":    {"pull_requests": "read", "metadata": "read"},
		"get":     {"pull_requests": "read", "contents": "read", "metadata": "read"},
		"write":   {"pull_requests": "write", "metadata": "read"},
		"merge":   {"contents": "write", "pull_requests": "read", "metadata": "read"},
		"ci":      {"checks": "read", "statuses": "read", "metadata": "read"},
		"history": {"checks": "read", "metadata": "read"},
	}
	got := map[string]map[string]string{
		"list":    PullListInstallationPermissions(),
		"get":     PullGetInstallationPermissions(),
		"write":   PullWriteInstallationPermissions(),
		"merge":   PullMergeInstallationPermissions(),
		"ci":      CIStatusInstallationPermissions(),
		"history": CIHistoryInstallationPermissions(),
	}
	manifest := BuildAppManifest("it", "https://x", "https://x/cb").DefaultPermissions
	for name, want := range exact {
		if !reflect.DeepEqual(got[name], want) {
			t.Errorf("%s profile = %v, want exactly %v", name, got[name], want)
		}
		for perm, level := range got[name] {
			granted, ok := manifest[perm]
			if !ok || (level == "write" && granted != "write") {
				t.Errorf("%s profile needs %s:%s, which the App manifest does not request (%v): no fresh installation could mint it", name, perm, level, manifest)
			}
		}
	}
	for _, ro := range []string{"list", "get", "ci", "history"} {
		for perm, level := range got[ro] {
			if level != "read" {
				t.Errorf("the %s profile is a read: it must carry no write grant, got %s:%s", ro, perm, level)
			}
		}
	}
	if _, ok := RuntimeInstallationPermissions()["checks"]; ok {
		t.Error("checks must NOT join the runtime baseline: every existing installation lacks it, and a baseline mint that asks for it 422s the management and run tokens alike")
	}
}

// One CI profile is one cached token: the status read (check-runs + combined
// status, two calls) mints once and reuses it; the history read is a
// different profile and gets its own token, never the status one.
func TestAppClientCIReadsShareOneTokenPerProfile(t *testing.T) {
	r := newPullMintRecorder(t)
	a := r.appClient(t)
	ctx := context.Background()
	if _, err := a.GetCIStatus(ctx, "o/r", "deadbeef"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.GetCIStatus(ctx, "o/r", "deadbeef"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ListCIHistory(ctx, "o/r", "deadbeef", 5); err != nil {
		t.Fatal(err)
	}
	mints, bearers, _ := r.snapshot()
	if len(mints) != 2 {
		t.Fatalf("mints = %d, want 2 (one status profile reused, one history profile)", len(mints))
	}
	if len(bearers) != 5 {
		t.Fatalf("bearers = %d, want 5 REST calls (2×2 for the statuses, 1 for the history)", len(bearers))
	}
	for _, b := range bearers[1:4] {
		if b != bearers[0] {
			t.Errorf("the status reads must share one cached token, got %v", bearers)
		}
	}
	if bearers[4] == bearers[0] {
		t.Error("the history read must not ride the status token: it is a narrower profile of its own")
	}
}

// An installation approved before `checks: read` was requested: GitHub
// refuses the CI profile's mint (422 "permissions not granted"), a body that
// names NO permission. The client turns it into the typed refusal naming the
// grant the installation withholds — resolved against the installation's
// live grant, so a `statuses: write` the owner did approve is not blamed —
// and the page where it is approved, while the mint sentinel stays
// reachable for the refresh worker's classification.
func TestAppClientCIStatusNamesTheWithheldGrant(t *testing.T) {
	r := newPullMintRecorder(t)
	r.granted = map[string]string{
		"contents": "write", "pull_requests": "write", "issues": "write", "metadata": "read",
		"repository_hooks": "write", "statuses": "write",
	}
	_, err := r.appClient(t).GetCIStatus(context.Background(), "o/r", "deadbeef")
	if err == nil {
		t.Fatal("a refused mint must be an error")
	}
	var pe *forge.PermissionError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v (%T), want a *forge.PermissionError naming the withheld grant", err, err)
	}
	if !reflect.DeepEqual(pe.Missing, []string{"checks:read"}) {
		t.Errorf("Missing = %v, want exactly [checks:read] — statuses is granted at write, which covers the read", pe.Missing)
	}
	if !errors.Is(err, forge.ErrPermissionsNotGranted) {
		t.Error("the mint sentinel must stay reachable: the refresh worker classifies on it")
	}
	for _, want := range []string{"checks:read", "approve", "settings/installations/99", "not granted"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to carry %q", err.Error(), want)
		}
	}
	if _, _, paths := r.snapshot(); len(paths) != 0 {
		t.Errorf("no REST call may be attempted on a refused mint, got %v", paths)
	}

	// Two grants short → both named, sorted, so the operator approves once.
	r2 := newPullMintRecorder(t)
	r2.granted = map[string]string{"contents": "write", "pull_requests": "write", "metadata": "read"}
	_, err = r2.appClient(t).GetCIStatus(context.Background(), "o/r", "deadbeef")
	if !errors.As(err, &pe) || !reflect.DeepEqual(pe.Missing, []string{"checks:read", "statuses:read"}) {
		t.Errorf("err = %v, want Missing [checks:read statuses:read]", err)
	}

	// The probe cannot say which one (an installation record with no
	// permissions): the whole requested set is named rather than nothing.
	r3 := newPullMintRecorder(t)
	r3.refuseMint = true
	_, err = r3.appClient(t).ListCIHistory(context.Background(), "o/r", "deadbeef", 5)
	if !errors.As(err, &pe) || !reflect.DeepEqual(pe.Missing, []string{"checks:read", "metadata:read"}) {
		t.Errorf("err = %v, want the full history profile named when the grant is unknown", err)
	}
}

// The call-time shape of the same gap: a token that lacks the permission (a
// grant revoked after the mint, a fine-grained PAT) gets GitHub's 403
// "Resource not accessible by integration" on the check-runs read. Both
// clients turn it into the typed refusal naming checks:read — never a bare
// ErrForbidden the panel would show as an unexplained 502.
func TestCheckRunsRefusedByIntegrationIsATypedError(t *testing.T) {
	r := newPullMintRecorder(t)
	r.notAccessible["/check-runs"] = true
	for name, c := range map[string]forge.PullClient{"pat": r.patClient(), "app": r.appClient(t)} {
		t.Run(name, func(t *testing.T) {
			_, err := c.GetCIStatus(context.Background(), "o/r", "deadbeef")
			var pe *forge.PermissionError
			if !errors.As(err, &pe) {
				t.Fatalf("err = %v (%T), want a *forge.PermissionError", err, err)
			}
			if !reflect.DeepEqual(pe.Missing, []string{"checks:read"}) || pe.Op != "GET check-runs" {
				t.Errorf("typed error = %+v, want Op GET check-runs, Missing [checks:read]", pe)
			}
			if !errors.Is(err, forge.ErrForbidden) {
				t.Error("a refused call must still read as ErrForbidden for the callers that classify on it")
			}
			for _, want := range []string{"checks:read", "Resource not accessible by integration"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want it to carry %q", err.Error(), want)
				}
			}
		})
	}
}

// A 403 that is NOT a permission gap (rate limit, SAML enforcement) must not
// be dressed up as one: it stays ErrForbidden, now carrying GitHub's message
// instead of a bare status.
func TestOtherForbiddenStaysForbiddenWithTheCause(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]any{"message": "API rate limit exceeded for installation ID 99."})
	}))
	defer srv.Close()
	c := &AdminClient{HTTP: srv.Client(), APIBase: srv.URL, Token: "t"}
	_, err := c.ListPullRequests(context.Background(), "o/r", forge.PullListOptions{})
	if !errors.Is(err, forge.ErrForbidden) {
		t.Fatalf("err = %v, want ErrForbidden", err)
	}
	var pe *forge.PermissionError
	if errors.As(err, &pe) {
		t.Errorf("a rate-limit 403 must not be reported as a missing permission: %v", err)
	}
	if !strings.Contains(err.Error(), "rate limit") {
		t.Errorf("error = %q, want GitHub's own message kept", err.Error())
	}
}

// MissingCIPermissions is what the connection health view reports, so the
// gap is visible on the connection before anyone opens a card.
func TestMissingCIPermissions(t *testing.T) {
	if got := MissingCIPermissions(nil); got != nil {
		t.Errorf("an unknown grant set is not evidence of a gap, got %v", got)
	}
	full := map[string]string{"checks": "read", "statuses": "write", "metadata": "read"}
	if got := MissingCIPermissions(full); got != nil {
		t.Errorf("nothing should be missing, got %v", got)
	}
	preChecks := map[string]string{"statuses": "write", "contents": "write", "metadata": "read"}
	if got := MissingCIPermissions(preChecks); !reflect.DeepEqual(got, []string{"checks"}) {
		t.Errorf("got %v, want [checks]", got)
	}
	preGate := map[string]string{"contents": "write", "metadata": "read"}
	if got := MissingCIPermissions(preGate); !reflect.DeepEqual(got, []string{"checks", "statuses"}) {
		t.Errorf("got %v, want [checks statuses] (sorted)", got)
	}
	if got := MissingCIPermissions(map[string]string{"checks": "read", "statuses": "read"}); got != nil {
		t.Errorf("statuses:read serves the combined-status read, got %v", got)
	}
}
