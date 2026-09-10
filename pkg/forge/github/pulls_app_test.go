package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"sort"
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

// githubEndpointGrants is GitHub's OWN rule for every endpoint the PullClient
// calls, transcribed row-for-row from its published per-endpoint permission
// data (the source behind "Permissions required for GitHub Apps"). It is the
// ONE place an endpoint's requirement is written down: the fake gates each
// REST call on it, and TestPullPermissionProfiles derives every profile's
// expected shape from it — so a profile is checked against GitHub's rule, not
// against a second hand-written copy of itself.
//
// Note the two rows of .../pulls/{n}/merge: the PUT (merge) is Contents write,
// the GET (check if merged, which iterion does not call) is Pull requests read.
// Only GET .../pulls/{n} is dual-listed, and that is why its row carries two.
var githubEndpointGrants = []struct {
	method string
	path   *regexp.Regexp
	grants map[string]string
	// slug is GitHub's own operation id for the row, so a failure names the
	// documentation row to re-read rather than a bare path.
	slug string
}{
	{"GET", regexp.MustCompile(`^/repos/[^/]+/[^/]+/pulls$`),
		map[string]string{"pull_requests": "read"}, "list-pull-requests"},
	{"POST", regexp.MustCompile(`^/repos/[^/]+/[^/]+/pulls$`),
		map[string]string{"pull_requests": "write"}, "create-a-pull-request"},
	{"GET", regexp.MustCompile(`^/repos/[^/]+/[^/]+/pulls/\d+$`),
		map[string]string{"pull_requests": "read", "contents": "read"}, "get-a-pull-request (dual-listed)"},
	{"PATCH", regexp.MustCompile(`^/repos/[^/]+/[^/]+/pulls/\d+$`),
		map[string]string{"pull_requests": "write"}, "update-a-pull-request"},
	{"PUT", regexp.MustCompile(`^/repos/[^/]+/[^/]+/pulls/\d+/merge$`),
		map[string]string{"contents": "write"}, "merge-a-pull-request"},
	{"DELETE", regexp.MustCompile(`^/repos/[^/]+/[^/]+/git/refs/.+$`),
		map[string]string{"contents": "write"}, "delete-a-reference"},
	{"GET", regexp.MustCompile(`^/repos/[^/]+/[^/]+/commits/[^/]+/check-runs$`),
		map[string]string{"checks": "read"}, "list-check-runs-for-a-git-reference"},
	{"GET", regexp.MustCompile(`^/repos/[^/]+/[^/]+/commits/[^/]+/status$`),
		map[string]string{"statuses": "read"}, "get-the-combined-status-for-a-specific-reference"},
	{"GET", regexp.MustCompile(`^/repos/[^/]+/[^/]+/pulls/\d+/comments$`),
		map[string]string{"pull_requests": "read"}, "list-review-comments-on-a-pull-request"},
}

// pullListOpts is the listing the reply-gate parity test issues after a
// thread fetch.
func pullListOpts() forge.PullListOptions { return forge.PullListOptions{State: "all", PerPage: 100} }

// grantsForRequestLine returns GitHub's rule for one recorded request line
// ("GET /api/v3/repos/o/r/pulls?state=all") and the row's slug, or nil for a
// path no PullClient method calls. The API-base prefix and the query string
// are stripped so the table stays the endpoint shapes GitHub publishes.
func grantsForRequestLine(line string) (map[string]string, string) {
	method, rest, ok := strings.Cut(line, " ")
	if !ok {
		return nil, ""
	}
	path, _, _ := strings.Cut(rest, "?")
	path = strings.TrimPrefix(path, "/api/v3")
	for _, e := range githubEndpointGrants {
		if e.method == method && e.path.MatchString(path) {
			return e.grants, e.slug
		}
	}
	return nil, ""
}

// pullMintRecorder is a fake GitHub serving the installation-token mint, the
// installation probe and the pull/CI endpoints. It records the permission set
// of every mint, the bearer of every REST call and the full request line, and
// applies GitHub's two refusals: a mint outside the installation's grant
// (422), and a call whose BEARER was minted without the grant the endpoint is
// gated on (403 "Resource not accessible by integration").
//
// That second rule is what makes a permission profile falsifiable here. While
// the fake served every REST call 2xx whatever the token carried, a profile
// that omitted a grant its endpoint requires passed the whole suite; now the
// call fails exactly as GitHub would fail it.
type pullMintRecorder struct {
	mu sync.Mutex
	// granted is what the installation approved; nil = accept every mint.
	granted map[string]string
	// refuseMint forces the 422 regardless of granted.
	refuseMint bool
	// notAccessible names URL-path suffixes refused with GitHub's 403
	// "Resource not accessible by integration" whatever the mint carried.
	notAccessible map[string]bool
	// tokenPerms maps an issued installation token to the permission set it
	// was minted with — the scope every REST call is then held to.
	tokenPerms map[string]map[string]string
	mints      []map[string]string
	bearers    []string
	paths      []string
	srv        *httptest.Server
	// t reports a served path githubEndpointGrants does not know. The scope
	// gate can only hold a call to a rule it HAS, so an unknown path would
	// otherwise be served unchecked — the gate going quiet exactly where a
	// new method or a new round trip needs it loudest.
	t *testing.T
}

// scopeRefusal reports the grant an endpoint needs and the call's bearer was
// not minted with, or "" when the call is within scope. A bearer the fake
// never issued (the PAT client) is unrestricted: a fine-grained PAT's scope is
// not a mint this fake can see, and the PAT half of these tests is about the
// wire shape, not about scoping.
func (r *pullMintRecorder) scopeRefusal(authz, line string) string {
	tok := strings.TrimPrefix(authz, "Bearer ")
	r.mu.Lock()
	perms, minted := r.tokenPerms[tok]
	r.mu.Unlock()
	if !minted {
		return ""
	}
	need, _ := grantsForRequestLine(line)
	for _, name := range sortedGrantNames(need) {
		// The production rule, not a fourth copy of it: what the client uses
		// to decide a grant is withheld is what the fake uses to withhold it.
		if !grantCovers(perms, name, need[name]) {
			return name + ":" + need[name]
		}
	}
	return ""
}

// grantCovers is the one rule the mint, the withheld-grant diagnostic and this
// fake all read, so its ordering is worth pinning: read < write < admin, a
// grant covering every level at or below its own. The `admin` rows are what a
// "write needs write-or-admin, anything else passes" shortcut gets wrong.
func TestGrantCoversOrdersTheLevels(t *testing.T) {
	for _, tc := range []struct {
		granted, requested string
		want               bool
	}{
		{"read", "read", true},
		{"write", "read", true},
		{"admin", "read", true},
		{"read", "write", false},
		{"write", "write", true},
		{"admin", "write", true},
		{"read", "admin", false},
		{"write", "admin", false},
		{"admin", "admin", true},
		// An unfamiliar level the installation holds covers what is asked of
		// it; an unfamiliar level we ask for is satisfied by no level we know.
		{"maintain", "write", true},
		{"admin", "maintain", false},
	} {
		got := grantCovers(map[string]string{"p": tc.granted}, "p", tc.requested)
		if got != tc.want {
			t.Errorf("grantCovers(granted %s, want %s) = %v, want %v", tc.granted, tc.requested, got, tc.want)
		}
	}
	if grantCovers(map[string]string{"other": "write"}, "p", "read") {
		t.Error("a permission the installation does not hold at all is never covered")
	}
}

// sortedGrantNames keeps the refusal deterministic when a call is short of
// more than one grant.
func sortedGrantNames(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func newPullMintRecorder(t *testing.T) *pullMintRecorder {
	t.Helper()
	r := &pullMintRecorder{t: t, notAccessible: map[string]bool{}, tokenPerms: map[string]map[string]string{}}
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
			if r.granted != nil {
				// The production rule here too: the mint gate carried its own
				// copy, with the same "a write needs write-or-admin, anything
				// else passes" shortcut grantCovers replaced — so a requested
				// `admin` was minted against a bare `read` grant and the
				// withheld-grant path was never reached for it.
				for name, level := range body.Permissions {
					if !grantCovers(r.granted, name, level) {
						refused = true
					}
				}
			}
			if refused {
				w.WriteHeader(http.StatusUnprocessableEntity)
				_ = json.NewEncoder(w).Encode(map[string]any{"message": "The permissions requested are not granted to this installation."})
				return
			}
			r.mints = append(r.mints, body.Permissions)
			n := len(r.mints)
			tok := "ghs_scoped_" + string(rune('a'+n-1))
			r.tokenPerms[tok] = body.Permissions
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token":      tok,
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
		line := req.Method + " " + req.URL.RequestURI()
		r.mu.Lock()
		r.bearers = append(r.bearers, req.Header.Get("Authorization"))
		r.paths = append(r.paths, line)
		refuse := false
		for suffix := range r.notAccessible {
			if strings.HasSuffix(req.URL.Path, suffix) {
				refuse = true
			}
		}
		r.mu.Unlock()
		// GitHub's own gate: the endpoint's rule against the scope the
		// bearer was minted with. A token short of it never sees the
		// resource, whatever the path serves.
		if short := r.scopeRefusal(req.Header.Get("Authorization"), line); short != "" {
			refuse = true
		}
		// A rule the table does not have is a call nothing held to any scope,
		// so say so rather than serve it: fail LOUD, never open.
		if need, _ := grantsForRequestLine(line); need == nil {
			r.t.Errorf("the fake served %q, which githubEndpointGrants has no row for — its permission rule went unchecked, "+
				"and an unchecked endpoint is how a wrong profile ships green", line)
		}
		if refuse {
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]any{"message": "Resource not accessible by integration", "documentation_url": "https://docs.github.com/rest"})
			return
		}
		switch {
		case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/comments"):
			// Newest first, as GitHub answers sort=created&direction=desc; the
			// client hands the thread back chronological.
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 9002, "in_reply_to_id": 9001, "body": "why?", "path": "pkg/x/y.go", "created_at": "2026-09-02T10:05:00Z", "user": map[string]any{"login": "alice"}},
				{"id": 9001, "body": "the SSRF is reachable", "path": "pkg/x/y.go", "created_at": "2026-09-02T10:00:00Z", "user": map[string]any{"login": "iterion[bot]"}},
			})
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

// Every profile is pinned from BOTH sides against GitHub's own rule, never
// against a second copy of itself: it must COVER each grant the endpoints its
// method actually calls are gated on (else the call 403s in production), and
// carry NOTHING beyond them but the mandatory metadata baseline (else a read
// acquires a privilege it has no use for). Which endpoints a method calls is
// observed, not declared — the call runs against the fake and its request
// lines are read back — so a method that grows a round trip re-derives its own
// requirement instead of drifting from a frozen literal.
func TestPullPermissionProfiles(t *testing.T) {
	for _, tc := range pullCalls {
		t.Run(tc.name, func(t *testing.T) {
			r := newPullMintRecorder(t)
			if err := tc.call(context.Background(), r.appClient(t)); err != nil {
				t.Fatalf("%s against a fully-granted installation: %v", tc.name, err)
			}
			mints, _, paths := r.snapshot()
			if len(mints) != 1 {
				t.Fatalf("mints = %d, want exactly 1", len(mints))
			}
			got := mints[0]

			// What GitHub gates this method's own request sequence on, and
			// which published row demands each grant.
			need, by := map[string]string{}, map[string]string{}
			covered := 0
			for _, line := range paths {
				g, slug := grantsForRequestLine(line)
				if g == nil {
					t.Errorf("request %q is not in githubEndpointGrants: its rule is unknown, so no profile can be checked against it", line)
					continue
				}
				covered++
				for perm, level := range g {
					if level == "write" || need[perm] == "" {
						need[perm], by[perm] = level, slug
					}
				}
			}
			if covered == 0 {
				t.Fatalf("%s issued no recognised request: %v", tc.name, paths)
			}
			for perm, level := range need {
				if cur, ok := got[perm]; !ok || (level == "write" && cur != "write") {
					t.Errorf("%s profile %v is short of %s:%s, which GitHub gates %q on", tc.name, got, perm, level, by[perm])
				}
			}
			for perm, level := range got {
				if perm == "metadata" {
					if level != "read" {
						t.Errorf("%s profile: metadata is the mandatory baseline and a read, got %q", tc.name, level)
					}
					continue
				}
				want, ok := need[perm]
				if !ok {
					t.Errorf("%s profile %v carries %s:%s, which none of its endpoints (%v) is gated on", tc.name, got, perm, level, paths)
					continue
				}
				if want == "read" && level == "write" {
					t.Errorf("%s profile takes %s:write where its endpoints only need read — a read must not acquire a write", tc.name, perm)
				}
			}
			if got["metadata"] != "read" {
				t.Errorf("%s profile = %v, want the mandatory metadata:read baseline", tc.name, got)
			}
		})
	}

	// Every profile must also be REQUESTABLE by an App the manifest creates
	// today — one the manifest does not cover could never be minted on a fresh
	// installation.
	manifest := BuildAppManifest("it", "https://x", "https://x/cb").DefaultPermissions
	for name, profile := range map[string]map[string]string{
		"list":    PullListInstallationPermissions(),
		"get":     PullGetInstallationPermissions(),
		"write":   PullWriteInstallationPermissions(),
		"merge":   PullMergeInstallationPermissions(),
		"ci":      CIStatusInstallationPermissions(),
		"history": CIHistoryInstallationPermissions(),
	} {
		for perm, level := range profile {
			granted, ok := manifest[perm]
			if !ok || (level == "write" && granted != "write") {
				t.Errorf("%s profile needs %s:%s, which the App manifest does not request (%v): no fresh installation could mint it", name, perm, level, manifest)
			}
		}
	}

	// `checks` stays out of the runtime baseline on purpose: the management
	// and run tokens are minted from it, and a baseline the installation
	// cannot serve fails EVERY mint for that installation (422), not just the
	// CI panel.
	if _, ok := RuntimeInstallationPermissions()["checks"]; ok {
		t.Error("checks must NOT join the runtime baseline: every existing installation lacks it, and a baseline mint that asks for it 422s the management and run tokens alike")
	}
}

// The gate above is only worth its lines if a wrong profile actually fails:
// mint the CI profile without `checks` and the check-runs read must be
// refused exactly as GitHub refuses it. Without this, a profile short of a
// grant its endpoint requires passes the whole suite — which is how one
// shipped green once already.
func TestTheFakeRefusesACallOutsideItsMintedScope(t *testing.T) {
	r := newPullMintRecorder(t)
	c, err := r.appClient(t).scopedREST(context.Background(), map[string]string{
		"statuses": "read", "metadata": "read", // deliberately short of checks:read
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.GetCIStatus(context.Background(), "o/r", "deadbeef")
	var pe *forge.PermissionError
	if !errors.As(err, &pe) || !reflect.DeepEqual(pe.Missing, []string{"checks:read"}) {
		t.Fatalf("err = %v, want the check-runs read refused for want of checks:read", err)
	}
	// ...and the very same call succeeds once the mint carries it, so the
	// refusal is the scope and not the path.
	c, err = r.appClient(t).scopedREST(context.Background(), CIStatusInstallationPermissions())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetCIStatus(context.Background(), "o/r", "deadbeef"); err != nil {
		t.Fatalf("the CI profile must serve its own endpoints: %v", err)
	}
}

// The MINT gate holds the same level ordering as the scope gate. `admin` is
// the level that separates a compared ordering from the "a write needs
// write-or-admin, anything else passes" shortcut: under the shortcut this
// mint is issued against an installation that holds the grant one level too
// low, GitHub 422s it for real, and the withheld-grant diagnostic is never
// exercised for the case.
func TestTheFakeRefusesAMintAboveTheInstallationsLevel(t *testing.T) {
	r := newPullMintRecorder(t)
	r.granted = map[string]string{"organization_projects": "write", "metadata": "read"}
	_, err := r.appClient(t).scopedREST(context.Background(),
		map[string]string{"organization_projects": "admin", "metadata": "read"})
	var pe *forge.PermissionError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v (%T), want the mint refused: write does not cover a requested admin", err, err)
	}
	if !reflect.DeepEqual(pe.Missing, []string{"organization_projects:admin"}) {
		t.Errorf("Missing = %v, want exactly [organization_projects:admin] — metadata:read IS granted", pe.Missing)
	}
	// The level the installation does hold is served.
	if _, err := r.appClient(t).scopedREST(context.Background(),
		map[string]string{"organization_projects": "write", "metadata": "read"}); err != nil {
		t.Errorf("a mint at the granted level must be issued: %v", err)
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
