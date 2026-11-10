package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/orgusage"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
	mongostore "github.com/SocialGouv/iterion/pkg/store/mongo"
	"github.com/SocialGouv/iterion/pkg/webhooks"
	"github.com/SocialGouv/iterion/pkg/webhooks/prforge"
)

// The fork review gesture's witnesses (ADR-162 U6): a REAL GitHub
// issue_comment delivery replayed through handleGitHubWebhook on a fake
// forge at the one seam the production path crosses (webhookForkGestureAPI
// → prforgeReplierAPI), so every admission row, the gate, the bound's
// metering and the Q1 note are driven on the code that serves production,
// never on a reimplementation of it.

// fakeForkForge is the fake forge: the replier surface the gesture reads
// (identity, permission, pull request) plus the issue comment the Q1 note
// posts. Every field is what one delivery sees.
type fakeForkForge struct {
	mu sync.Mutex

	login   string            // WhoAmI's answer ("" → named no login)
	whoErr  error             // WhoAmI's refusal
	perms   map[string]string // login → CollaboratorPermission answer
	permErr error             // CollaboraboratorPermission's refusal
	pr      forge.PullRef     // GetPullRequest's answer
	prErr   error             // GetPullRequest's refusal
	// getPR, when set, answers GetPullRequest instead of pr/prErr — the seam
	// a row uses to put the REAL forge client's decoding behind the gesture.
	getPR      func(ctx context.Context, repo string, number int) (forge.PullRef, error)
	notes      []string // CommentIssue bodies, in order
	commentErr error
}

func (f *fakeForkForge) WhoAmI(ctx context.Context) (forge.Identity, error) {
	if f.whoErr != nil {
		return forge.Identity{}, f.whoErr
	}
	return forge.Identity{Login: f.login, Kind: forge.AccountKindUser}, nil
}

func (f *fakeForkForge) CollaboratorPermission(ctx context.Context, repo, user string) (string, error) {
	if f.permErr != nil {
		return "", f.permErr
	}
	return f.perms[user], nil
}

func (f *fakeForkForge) GetPullRequest(ctx context.Context, repo string, number int) (forge.PullRef, error) {
	if f.getPR != nil {
		return f.getPR(ctx, repo, number)
	}
	if f.prErr != nil {
		return forge.PullRef{}, f.prErr
	}
	return f.pr, nil
}

func (f *fakeForkForge) CommentIssue(ctx context.Context, repo string, number int, body string) (forge.CommentRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.commentErr != nil {
		return forge.CommentRef{}, f.commentErr
	}
	f.notes = append(f.notes, body)
	return forge.CommentRef{ID: "c1", Body: body}, nil
}

func (f *fakeForkForge) postedNotes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.notes...)
}

// forkForkPR is the pull request a gesture ADMITS: open, head repository
// named and not the base, public base, numeric author id, head commit.
func forkForkPR() forge.PullRef {
	return forge.PullRef{
		Number:           7,
		State:            "open",
		URL:              "https://github.com/acme/widgets/pull/7",
		SourceBranch:     "feat/x",
		TargetBranch:     "main",
		HeadSHA:          "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		HeadRepoFullName: "alice/widgets-fork",
		HeadRepoDeclared: true,
		BaseCloneURL:     "https://github.com/acme/widgets.git",
		BaseVisibility:   "public",
		Author:           "alice",
		AuthorID:         "4242",
	}
}

// ghForkComment is a REAL GitHub issue_comment delivery: a maintainer's
// /revi on a fork pull request of acme/widgets. commentID varies per test
// so idempotency never collides across replays; labels rides the issue
// object as GitHub sends it (the hold-label veto's input).
func ghForkComment(commentID int64, body string, labels ...string) string {
	labelJSON := "[]"
	if len(labels) > 0 {
		parts := make([]string, len(labels))
		for i, l := range labels {
			parts[i] = fmt.Sprintf(`{"name": %q}`, l)
		}
		labelJSON = "[" + strings.Join(parts, ",") + "]"
	}
	return fmt.Sprintf(`{
  "action": "created",
  "repository": {"id": 42, "full_name": "acme/widgets", "clone_url": "https://github.com/acme/widgets.git"},
  "issue": {"number": 7, "title": "Add X", "body": "the PR description", "state": "open",
    "labels": %s,
    "pull_request": {"html_url": "https://github.com/acme/widgets/pull/7"}},
  "comment": {"id": %d, "body": %q},
  "sender": {"login": "maintainer"}
}`, labelJSON, commentID, body)
}

// forkGestureFixture wires one gesture replay: a fork-lane github config,
// the fake forge at the API seam, a MemoryCounter the gate meters on, and
// a launcher that captures what the launch carries. Returns the dials the
// test holds.
type forkGestureFixture struct {
	s       *Server
	cfg     webhooks.Config
	pt      string
	forge   *fakeForkForge
	counter *orgusage.MemoryCounter

	mu        sync.Mutex
	launches  int
	gotVars   map[string]string
	gotURL    string
	gotRef    string
	gotBot    string
	runIDFor  func() string
	launchErr error
}

func newForkGestureFixture(t *testing.T) *forkGestureFixture {
	t.Helper()
	s := newWebhookTestServer(t)
	s.cfg.PublicURL = "https://iterion.test"
	counter := orgusage.NewMemoryCounter()
	s.orgUsage = counter
	cfg, pt := ghConfig(t, s)
	cfg.ForkLane = true
	cfg.ForkLanePool = "forks"
	cfg.HoldLabels = []string{"hold-review"}
	cfg.OperatorLaunchVars = map[string]string{"gate_context": "acme/required-check"}
	fx := &forkGestureFixture{s: s, cfg: cfg, pt: pt, counter: counter, runIDFor: func() string { return "run-9" }}
	fx.forge = &fakeForkForge{
		login: "iterion-bot",
		perms: map[string]string{"maintainer": "maintain"},
		pr:    forkForkPR(),
	}
	s.webhookForkGestureAPI = func(context.Context, webhooks.Config, webhooks.Provider, prforge.ParsedNote, string) (prforgeReplierAPI, string) {
		return fx.forge, ""
	}
	s.webhookLaunchBot = func(_ context.Context, botID string, vars map[string]string, repoURL, repoRef, projectPath string, _, _ map[string]string) (string, error) {
		fx.mu.Lock()
		defer fx.mu.Unlock()
		fx.launches++
		fx.gotVars, fx.gotURL, fx.gotRef, fx.gotBot = vars, repoURL, repoRef, botID
		if fx.launchErr != nil {
			return "", fx.launchErr
		}
		return fx.runIDFor(), nil
	}
	return fx
}

// replay delivers one issue_comment body through the REAL GitHub handler.
func (fx *forkGestureFixture) replay(t *testing.T, commentID int64, body string, labels ...string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req := ghReq(ghCtx(fx.cfg), ghForkComment(commentID, body, labels...), prforge.EventHeaderIssueComment, fx.pt)
	fx.s.handleGitHubWebhook(w, req)
	return w
}

func (fx *forkGestureFixture) launchedCount() int {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	return fx.launches
}

// TestForkGesture_AdmitsAProvenPublicFork: the happy row. A real GitHub
// delivery, a maintainer with maintain permission, an open public-base
// fork — the run launches on the base's clone URL and the base's own
// pull-request head ref, pinned to the head commit, with the author's
// numeric forge id on the target; both budgets meter; the Q1 note names
// the run link and the pinned commit and NOTHING the run wrote; and the
// base repo's operator vars are NOT layered onto the launch.
func TestForkGesture_AdmitsAProvenPublicFork(t *testing.T) {
	fx := newForkGestureFixture(t)
	w := fx.replay(t, 901, "/revi")

	if w.Code != http.StatusAccepted {
		t.Fatalf("code=%d body=%s — an admitted gesture must launch", w.Code, w.Body.String())
	}
	if fx.launchedCount() != 1 {
		t.Fatalf("launches=%d, want 1", fx.launchedCount())
	}
	// The launch pair names ONE repository: the base's clone URL and the
	// base's own pull-request head ref.
	if fx.gotURL != "https://github.com/acme/widgets.git" {
		t.Fatalf("repoURL=%q, want the BASE repository's clone URL", fx.gotURL)
	}
	if fx.gotRef != "refs/pull/7/head" {
		t.Fatalf("repoRef=%q, want the base repository's own pull-request head ref", fx.gotRef)
	}
	if fx.gotBot != "review-pr" {
		t.Fatalf("bot=%q, want the lane's default route /revi → review-pr", fx.gotBot)
	}
	// The pinned commit rides the vars the launcher received (the pin
	// itself is enforced at the tail and by the runner).
	if fx.gotVars["head_sha"] != "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef" {
		t.Fatalf("vars[head_sha]=%q, want the head commit the admission proved", fx.gotVars["head_sha"])
	}
	// Decision 4: the base repo's operator layer is NOT applied. The
	// config pins a gate_context; a var the repo chose must not reach a
	// run whose code the repo did not write.
	if _, ok := fx.gotVars["gate_context"]; ok {
		t.Fatalf("vars carry the base repo's operator pin gate_context=%q — the base's configuration may stop a fork run, never steer it", fx.gotVars["gate_context"])
	}
	// Both budgets metered on the first charge: the org's and the
	// author's own subject (orgless team t1 → metered on the team id).
	now := time.Now().UTC()
	orgU, err := fx.counter.Usage(context.Background(), orgusage.OrgSubject("t1"), now)
	if err != nil {
		t.Fatal(err)
	}
	if orgU.Runs != 1 {
		t.Fatalf("org runs = %d, want 1 — the fork launch spends the org's budget like every launch", orgU.Runs)
	}
	authorU, err := fx.counter.Usage(context.Background(), orgusage.ForkAuthorSubject("t1", "github", "4242"), now)
	if err != nil {
		t.Fatal(err)
	}
	if authorU.Runs != 1 {
		t.Fatalf("author runs = %d, want 1 — the per-contributor bound must meter beside the org's", authorU.Runs)
	}
	// Q1: a note on the fork pull request naming the run link and the
	// pinned commit, and nothing else the run produced.
	notes := fx.forge.postedNotes()
	if len(notes) != 1 {
		t.Fatalf("notes posted = %d (%v), want exactly one — the gesture's answer where the gesture was made", len(notes), notes)
	}
	note := notes[0]
	if !strings.Contains(note, "run-9") || !strings.Contains(note, "https://iterion.test") {
		t.Fatalf("note does not name the run link:\n%s", note)
	}
	if !strings.Contains(note, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef") {
		t.Fatalf("note does not name the pinned commit:\n%s", note)
	}
	// "nothing the run wrote": the note is server-authored prose + the two
	// facts. The PR's own title/description (what a steered run would echo)
	// must not appear.
	for _, leaked := range []string{"Add X", "the PR description"} {
		if strings.Contains(note, leaked) {
			t.Fatalf("note carries the pull request's %q — it names the run link and the pinned commit, nothing else:\n%s", leaked, note)
		}
	}
	// The delivery row records the launch.
	rows, err := fx.s.webhookDeliveries.ListByWebhook(context.Background(), "t1", fx.cfg.ID, 10)
	if err != nil || len(rows) != 1 || rows[0].Status != webhooks.StatusLaunched || rows[0].RunID != "run-9" {
		t.Fatalf("delivery rows = %+v (err %v), want one launched row for run-9", rows, err)
	}
}

// TestForkGesture_RefusalRows: every row of the delivery-reason table,
// each replayed as a real delivery and each proven to refuse BEFORE either
// budget is metered.
func TestForkGesture_RefusalRows(t *testing.T) {
	type row struct {
		name       string
		mutate     func(fx *forkGestureFixture)
		body       string
		labels     []string
		wantReason string
	}
	rows := []row{
		{
			name:       "same-repo pull request",
			mutate:     func(fx *forkGestureFixture) { p := forkForkPR(); p.HeadRepoFullName = "acme/widgets"; fx.forge.pr = p },
			wantReason: "same-repo pull request",
		},
		{
			name: "withheld head",
			mutate: func(fx *forkGestureFixture) {
				p := forkForkPR()
				p.HeadRepoFullName = ""
				p.HeadRepoDeclared = true
				fx.forge.pr = p
			},
			wantReason: "head repo withheld",
		},
		{
			name: "head not named",
			mutate: func(fx *forkGestureFixture) {
				p := forkForkPR()
				p.HeadRepoFullName = ""
				p.HeadRepoDeclared = false
				fx.forge.pr = p
			},
			wantReason: "head repo not named",
		},
		{
			name:       "private base",
			mutate:     func(fx *forkGestureFixture) { p := forkForkPR(); p.BaseVisibility = "private"; fx.forge.pr = p },
			wantReason: "not PROVEN public",
		},
		{
			name:       "internal base is not public",
			mutate:     func(fx *forkGestureFixture) { p := forkForkPR(); p.BaseVisibility = "internal"; fx.forge.pr = p },
			wantReason: "not PROVEN public",
		},
		{
			name:       "base visibility unsaid",
			mutate:     func(fx *forkGestureFixture) { p := forkForkPR(); p.BaseVisibility = ""; fx.forge.pr = p },
			wantReason: "not PROVEN public",
		},
		{
			name:       "no numeric author id",
			mutate:     func(fx *forkGestureFixture) { p := forkForkPR(); p.AuthorID = ""; fx.forge.pr = p },
			wantReason: "no numeric author id",
		},
		{
			name:       "stale SHA argument",
			body:       "/revi 1111111111111111111111111111111111111111",
			wantReason: "the gesture pinned",
		},
		{
			name:       "prose argument is not a pin",
			body:       "/revi please review the diff carefully",
			wantReason: "takes an optional commit SHA",
		},
		{
			name:       "closed pull request",
			mutate:     func(fx *forkGestureFixture) { p := forkForkPR(); p.State = "closed"; fx.forge.pr = p },
			wantReason: "the pull request is closed",
		},
		{
			name:       "merged pull request",
			mutate:     func(fx *forkGestureFixture) { p := forkForkPR(); p.State = "merged"; fx.forge.pr = p },
			wantReason: "the pull request is merged",
		},
		{
			name:       "no route",
			body:       "/featurly add an endpoint",
			wantReason: "no route",
		},
		{
			name: "login-allowlisted non-writer",
			mutate: func(fx *forkGestureFixture) {
				fx.cfg.AuthorizedRepliers = []string{"maintainer"}
				fx.forge.perms = map[string]string{"maintainer": "read"}
			},
			wantReason: "no write-level permission",
		},
		{
			name: "sub-developer configured floor is raised",
			mutate: func(fx *forkGestureFixture) {
				fx.cfg.MinReplierRole = "guest"
				fx.forge.perms = map[string]string{"maintainer": "triage"}
			},
			wantReason: "no write-level permission",
		},
		{
			name:       "hold label blocks the gesture",
			labels:     []string{"hold-review"},
			wantReason: "hold label",
		},
		{
			// The pull request names no head commit: the gesture pins the
			// commit it admits, so the row is the gesture's own — not the
			// tail's later "no admitted commit" refusal of the same input.
			name:       "no head commit",
			mutate:     func(fx *forkGestureFixture) { p := forkForkPR(); p.HeadSHA = ""; fx.forge.pr = p },
			wantReason: "names no head commit",
		},
		{
			// The loop guard's three arms (decision 2: the note the lane
			// posts must never re-trigger it). The bot's own identity
			// unreadable: refused, naming the forge's own error — never
			// passed on to the permission check.
			name:       "loop guard: the bot's identity is unreadable",
			mutate:     func(fx *forkGestureFixture) { fx.forge.whoErr = errors.New("identity service 503") },
			wantReason: loopGuardUnevaluable + ": identity service 503",
		},
		{
			name:       "loop guard: the forge names no login",
			mutate:     func(fx *forkGestureFixture) { fx.forge.login = "" },
			wantReason: loopGuardUnevaluable + ": the forge named no login",
		},
		{
			// The comment was written by the identity the lane speaks as —
			// the Q1 note echoing back, matched case-insensitively. That
			// identity holds write (it is the bot's installation), so without
			// the guard the permission check would admit it.
			name:       "loop guard: the bot's own comment",
			mutate:     func(fx *forkGestureFixture) { fx.forge.login = "Maintainer" },
			wantReason: "self comment (loop-guard)",
		},
	}
	for i, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			fx := newForkGestureFixture(t)
			if r.mutate != nil {
				r.mutate(fx)
			}
			body := r.body
			if body == "" {
				body = "/revi"
			}
			w := fx.replay(t, int64(1000+i), body, r.labels...)
			if fx.launchedCount() != 0 {
				t.Fatalf("a refused row launched %d run(s)", fx.launchedCount())
			}
			if w.Code != http.StatusOK {
				t.Fatalf("code=%d body=%s — a refusal is a 200/filtered verdict the forge must not disable the hook for", w.Code, w.Body.String())
			}
			var resp map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || resp["status"] != webhooks.StatusFiltered {
				t.Fatalf("response=%s err=%v, want a filtered verdict", w.Body.String(), err)
			}
			// The delivery row carries the typed reason.
			rowsOut, err := fx.s.webhookDeliveries.ListByWebhook(context.Background(), "t1", fx.cfg.ID, 10)
			if err != nil || len(rowsOut) != 1 {
				t.Fatalf("delivery rows=%+v err=%v, want the refusal recorded", rowsOut, err)
			}
			if !strings.Contains(rowsOut[0].Error, r.wantReason) {
				t.Fatalf("delivery reason = %q, want it to name %q", rowsOut[0].Error, r.wantReason)
			}
			// Every row refuses BEFORE either budget is metered — the
			// private-base row's own witness, asserted for all of them.
			now := time.Now().UTC()
			for _, subj := range []orgusage.Subject{orgusage.OrgSubject("t1"), orgusage.ForkAuthorSubject("t1", "github", "4242")} {
				u, err := fx.counter.Usage(context.Background(), subj, now)
				if err != nil {
					t.Fatal(err)
				}
				if u.Runs != 0 {
					t.Fatalf("subject %s metered %d run(s) — the refusal must precede the metering", subj, u.Runs)
				}
			}
		})
	}
}

// TestForkGesture_HoldLabelNeedsThePayloadToCarryIt: the hold row above
// fires on the config's label; this pins the complement — a payload
// WITHOUT the label gestures normally, so the row is a label match and
// not a blanket refusal.
func TestForkGesture_HoldLabelNeedsThePayloadToCarryIt(t *testing.T) {
	fx := newForkGestureFixture(t)
	fx.cfg.HoldLabels = []string{"hold-review"}
	// The fixture payload carries no labels: the gesture launches.
	w := fx.replay(t, 1100, "/revi")
	if w.Code != http.StatusAccepted || fx.launchedCount() != 1 {
		t.Fatalf("code=%d launches=%d — a pull request without the hold label must gesture normally", w.Code, fx.launchedCount())
	}
}

// TestForkGesture_IssueSurfaceAndProviderRefusals: the gesture is a pull
// request comment on github — an issue comment and a non-github
// fork-lane config are both refused typed.
func TestForkGesture_IssueSurfaceAndProviderRefusals(t *testing.T) {
	t.Run("issue comment", func(t *testing.T) {
		fx := newForkGestureFixture(t)
		w := httptest.NewRecorder()
		body := `{"action":"created","repository":{"id":42,"full_name":"acme/widgets","clone_url":"https://github.com/acme/widgets.git"},"issue":{"number":8,"title":"t","body":"b","state":"open"},"comment":{"id":1201,"body":"/revi"},"sender":{"login":"maintainer"}}`
		fx.s.handleGitHubWebhook(w, ghReq(ghCtx(fx.cfg), body, prforge.EventHeaderIssueComment, fx.pt))
		if fx.launchedCount() != 0 {
			t.Fatalf("an issue comment launched %d run(s) — the gesture reviews a fork pull request", fx.launchedCount())
		}
		rowsOut, _ := fx.s.webhookDeliveries.ListByWebhook(context.Background(), "t1", fx.cfg.ID, 10)
		if len(rowsOut) != 1 || !strings.Contains(rowsOut[0].Error, "not enabled on issue comments") {
			t.Fatalf("rows=%+v, want the surface refusal", rowsOut)
		}
	})
	t.Run("non-github config reaches no half-built path", func(t *testing.T) {
		// A hand-edited forgejo row with ForkLane set: the handler is
		// reachable for forgejo deliveries, and the gesture answers with
		// the provider refusal instead of launching.
		s := newWebhookTestServer(t)
		s.orgUsage = orgusage.NewMemoryCounter()
		cfg, _ := ghConfig(t, s)
		cfg.ForkLane = true
		cfg.ForkLanePool = "forks"
		cfg.Provider = webhooks.ProviderForgejo
		var launched int
		s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
			launched++
			return "run-x", nil
		}
		s.webhookForkGestureAPI = func(context.Context, webhooks.Config, webhooks.Provider, prforge.ParsedNote, string) (prforgeReplierAPI, string) {
			t.Fatal("the API seam must not even be reached on a provider the lane refuses")
			return nil, ""
		}
		w := httptest.NewRecorder()
		// Forgejo shares GitHub's wire shape; the provider label is the
		// config's, which is what the gesture reads.
		s.handlePRForgeComment(ghCtx(cfg), w, nil, cfg, webhooks.ProviderForgejo, []byte(ghForkComment(1301, "/revi")), "hash", "1.2.3.4")
		if launched != 0 {
			t.Fatalf("a forgejo fork-lane config launched %d run(s)", launched)
		}
		if w.Code != http.StatusOK {
			t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
		}
		rowsOut, _ := s.webhookDeliveries.ListByWebhook(context.Background(), "t1", cfg.ID, 10)
		if len(rowsOut) != 1 || !strings.Contains(rowsOut[0].Error, "github-only") {
			t.Fatalf("rows=%+v, want the typed provider refusal", rowsOut)
		}
	})
}

// TestForkGesture_BoardModeRouteRefused: the lane's own table may name a
// board-mode route; the gesture refuses it — a card holds no seat for the
// launch's provenance.
func TestForkGesture_BoardModeRouteRefused(t *testing.T) {
	fx := newForkGestureFixture(t)
	fx.cfg.CommandMap = map[string][]webhooks.CommandRoute{
		"revi": {{BotID: "review-pr", Mode: "board", Scope: "pr"}},
	}
	fx.replay(t, 1400, "/revi")
	if fx.launchedCount() != 0 {
		t.Fatalf("a board-mode route launched %d run(s)", fx.launchedCount())
	}
	rowsOut, _ := fx.s.webhookDeliveries.ListByWebhook(context.Background(), "t1", fx.cfg.ID, 10)
	if len(rowsOut) != 1 || !strings.Contains(rowsOut[0].Error, "board-mode route") {
		t.Fatalf("rows=%+v, want the board-mode refusal", rowsOut)
	}
}

// TestForkGesture_BotOutOfScopeRefused: the delivery-row table's bot-scope
// row — the default route names review-pr and the config's scope omits it,
// so the gesture is filtered naming the bot, never launched on a bot the
// webhook never permitted.
func TestForkGesture_BotOutOfScopeRefused(t *testing.T) {
	fx := newForkGestureFixture(t)
	fx.cfg.BotIDs = []string{"other-bot"}
	fx.replay(t, 1450, "/revi")
	if fx.launchedCount() != 0 {
		t.Fatalf("an out-of-scope bot launched %d run(s)", fx.launchedCount())
	}
	rowsOut, _ := fx.s.webhookDeliveries.ListByWebhook(context.Background(), "t1", fx.cfg.ID, 10)
	if len(rowsOut) != 1 || !strings.Contains(rowsOut[0].Error, `bot "review-pr" is not permitted`) {
		t.Fatalf("rows=%+v, want the bot-scope refusal naming the route's bot", rowsOut)
	}
}

// TestForkGesture_ConfiguredTableWins: the lane's own table routes a
// command the config names — the provisioned entry beats the built-in
// default.
func TestForkGesture_ConfiguredTableWins(t *testing.T) {
	fx := newForkGestureFixture(t)
	fx.cfg.BotIDs = []string{"review-pr", "other-bot"}
	fx.cfg.CommandMap = map[string][]webhooks.CommandRoute{
		"revi": {{BotID: "other-bot", Scope: "pr"}},
	}
	w := fx.replay(t, 1500, "/revi")
	if w.Code != http.StatusAccepted || fx.launchedCount() != 1 {
		t.Fatalf("code=%d launches=%d — the lane's own table must route", w.Code, fx.launchedCount())
	}
	fx.mu.Lock()
	defer fx.mu.Unlock()
	if fx.gotBot != "other-bot" {
		t.Fatalf("bot=%q, want the table's entry", fx.gotBot)
	}
}

// TestForkLane_IsGithubOnlyAtCreation: every other provider is refused
// typed at creation, github is accepted.
func TestForkLane_IsGithubOnlyAtCreation(t *testing.T) {
	s := newWebhookTestServer(t)
	ctx := superAdminCtx()
	for _, provider := range []string{"gitlab", "forgejo", "generic"} {
		w := httptest.NewRecorder()
		s.handleCreateWebhook(w, whReq(ctx, "POST", "/api/teams/t1/webhooks",
			`{"name":"x","provider":"`+provider+`","bot_ids":["review-pr"],"fork_lane":true}`, "t1", ""))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: code=%d body=%s, want the typed creation refusal", provider, w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "github-only") {
			t.Fatalf("%s: refusal = %s, want it to name the witness the provider lacks", provider, w.Body.String())
		}
	}
	// Github accepts, and the row carries the lane kind.
	w := httptest.NewRecorder()
	s.handleCreateWebhook(w, whReq(ctx, "POST", "/api/teams/t1/webhooks",
		`{"name":"fork reviews","provider":"github","bot_ids":["review-pr"],"fork_lane":true,"fork_lane_pool":"forks"}`, "t1", ""))
	if w.Code != http.StatusCreated {
		t.Fatalf("github: code=%d body=%s", w.Code, w.Body.String())
	}
	var created struct {
		Config webhooks.Config `json:"config"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if !created.Config.ForkLane {
		t.Fatal("the created github config does not carry fork_lane")
	}
}

// TestForkLane_IsImmutableOnPatch: a flip is refused (409); a restatement
// of the stored value is a no-op.
func TestForkLane_IsImmutableOnPatch(t *testing.T) {
	s := newWebhookTestServer(t)
	ctx := superAdminCtx()
	// Seed an ordinary github config.
	w := httptest.NewRecorder()
	s.handleCreateWebhook(w, whReq(ctx, "POST", "/api/teams/t1/webhooks",
		`{"name":"ordinary","provider":"github","bot_ids":["review-pr"]}`, "t1", ""))
	if w.Code != http.StatusCreated {
		t.Fatalf("create: code=%d body=%s", w.Code, w.Body.String())
	}
	var created struct {
		Config webhooks.Config `json:"config"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	id := created.Config.ID

	// A flip ON is refused.
	w = httptest.NewRecorder()
	s.handleUpdateWebhook(w, whReq(ctx, "PATCH", "/api/teams/t1/webhooks/"+id,
		`{"fork_lane":true}`, "t1", id))
	if w.Code != http.StatusConflict {
		t.Fatalf("flip on: code=%d body=%s, want 409 — flipping the kind re-reads parked targets as the other kind", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "immutable") {
		t.Fatalf("refusal = %s, want it to say why the kind cannot change", w.Body.String())
	}
	// A restatement of the stored value (false) is a no-op — the field
	// stays round-trippable for a client echoing the row back.
	w = httptest.NewRecorder()
	s.handleUpdateWebhook(w, whReq(ctx, "PATCH", "/api/teams/t1/webhooks/"+id,
		`{"fork_lane":false,"name":"ordinary-renamed"}`, "t1", id))
	if w.Code != http.StatusOK {
		t.Fatalf("restatement: code=%d body=%s, want the no-op patch to apply", w.Code, w.Body.String())
	}

	// And a fork-lane config cannot be flipped OFF either.
	w = httptest.NewRecorder()
	s.handleCreateWebhook(w, whReq(ctx, "POST", "/api/teams/t1/webhooks",
		`{"name":"fork","provider":"github","bot_ids":["review-pr"],"fork_lane":true,"fork_lane_pool":"forks"}`, "t1", ""))
	if w.Code != http.StatusCreated {
		t.Fatalf("fork create: code=%d body=%s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	forkID := created.Config.ID
	w = httptest.NewRecorder()
	s.handleUpdateWebhook(w, whReq(ctx, "PATCH", "/api/teams/t1/webhooks/"+forkID,
		`{"fork_lane":false}`, "t1", forkID))
	if w.Code != http.StatusConflict {
		t.Fatalf("flip off: code=%d body=%s, want 409", w.Code, w.Body.String())
	}
}

// erringCounter meters nothing: every AllowRun fails, the metering-error
// arm's witness.
type erringCounter struct {
	orgusage.Counter
}

func (erringCounter) AllowRun(ctx context.Context, subject orgusage.Subject, when time.Time, maxRuns int, maxCostMillis int64) (orgusage.DenyReason, error) {
	return orgusage.DenyNone, errors.New("counter is down")
}

// TestForkGesture_TheBoundDeniesAndFailsClosed: the per-contributor bound
// through the REAL handler — the N+1th run on the run count, the next run
// on dollars after a charged one — and the fail-closed arms: a metering
// error, a missing counter.
func TestForkGesture_TheBoundDeniesAndFailsClosed(t *testing.T) {
	t.Run("the N+1th run on its run count", func(t *testing.T) {
		fx := newForkGestureFixture(t)
		fx.s.orgDefaults = OrgLimitDefaults{ForkAuthorRunQuota: 1}
		if w := fx.replay(t, 2001, "/revi"); w.Code != http.StatusAccepted || fx.launchedCount() != 1 {
			t.Fatalf("first gesture: code=%d launches=%d", w.Code, fx.launchedCount())
		}
		// A SECOND gesture by the SAME author (a new comment, so a fresh
		// idem key): denied on the author's own quota.
		w := fx.replay(t, 2002, "/revi")
		if fx.launchedCount() != 1 {
			t.Fatalf("the N+1th run launched (launches=%d) — the author's bound must deny it", fx.launchedCount())
		}
		var resp map[string]string
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("body=%s: %v", w.Body.String(), err)
		}
		if resp["error"] != denyForkAuthorRuns {
			t.Fatalf("error token = %q, want %q (body %s)", resp["error"], denyForkAuthorRuns, w.Body.String())
		}
		// The denied attempt must not have left the ORG's increment
		// behind: the org still holds exactly the one admitted run.
		orgU, _ := fx.counter.Usage(context.Background(), orgusage.OrgSubject("t1"), time.Now().UTC())
		if orgU.Runs != 1 {
			t.Fatalf("org runs = %d after a denied gesture, want 1 — the author denial must hand the org's slot back", orgU.Runs)
		}
		authorU, _ := fx.counter.Usage(context.Background(), orgusage.ForkAuthorSubject("t1", "github", "4242"), time.Now().UTC())
		if authorU.Runs != 1 {
			t.Fatalf("author runs = %d, want 1 — a denied call consumes nothing", authorU.Runs)
		}
	})

	t.Run("the next run on dollars after a charged run", func(t *testing.T) {
		fx := newForkGestureFixture(t)
		fx.s.orgDefaults = OrgLimitDefaults{ForkAuthorCostCapUSD: 2}
		// A previously charged run: the runner's second charge lands on
		// the same subject the gate meters.
		author := orgusage.ForkAuthorSubject("t1", "github", "4242")
		if err := fx.counter.AddSpend(context.Background(), author, time.Now().UTC(), 2.5, 0, 0, 0); err != nil {
			t.Fatal(err)
		}
		w := fx.replay(t, 2101, "/revi")
		if fx.launchedCount() != 0 {
			t.Fatalf("a gesture over the author's dollar bound launched %d run(s)", fx.launchedCount())
		}
		var resp map[string]string
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("body=%s: %v", w.Body.String(), err)
		}
		if resp["error"] != denyForkAuthorCost {
			t.Fatalf("error token = %q, want %q", resp["error"], denyForkAuthorCost)
		}
		// The org's slot was handed back: no run was admitted.
		orgU, _ := fx.counter.Usage(context.Background(), orgusage.OrgSubject("t1"), time.Now().UTC())
		if orgU.Runs != 0 {
			t.Fatalf("org runs = %d after a dollar-denied gesture, want 0", orgU.Runs)
		}
	})

	t.Run("a metering error refuses", func(t *testing.T) {
		fx := newForkGestureFixture(t)
		fx.s.orgUsage = erringCounter{Counter: fx.counter}
		w := fx.replay(t, 2201, "/revi")
		if fx.launchedCount() != 0 {
			t.Fatalf("a metering error launched %d run(s) — an untrusted launch is refused, not admitted unmetered", fx.launchedCount())
		}
		// A 5xx denial renders through the error envelope, whose stable
		// token field is error_code (renderLaunchDenial).
		var resp map[string]string
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("body=%s: %v", w.Body.String(), err)
		}
		if resp["error_code"] != denyForkMeteringError {
			t.Fatalf("error_code = %q, want %q (body %s)", resp["error_code"], denyForkMeteringError, w.Body.String())
		}
	})

	t.Run("a missing counter refuses", func(t *testing.T) {
		fx := newForkGestureFixture(t)
		fx.s.orgUsage = nil
		w := fx.replay(t, 2301, "/revi")
		if fx.launchedCount() != 0 {
			t.Fatalf("a server with no usage counter launched %d untrusted run(s)", fx.launchedCount())
		}
		var resp map[string]string
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("body=%s: %v", w.Body.String(), err)
		}
		if resp["error_code"] != denyForkMeteringUnavailable {
			t.Fatalf("error_code = %q, want %q (body %s)", resp["error_code"], denyForkMeteringUnavailable, w.Body.String())
		}
	})
}

// TestForkLaunch_UnreadableTeamRefusesOnTheDeliverySurface: decision 5's
// degraded arm — a team the gate cannot read fails OPEN for trusted work
// and CLOSED for an untrusted subject, on the launch tail every delivery
// crosses.
func TestForkLaunch_UnreadableTeamRefusesOnTheDeliverySurface(t *testing.T) {
	s, degrade, _ := newDegradableServer(t)
	s.orgUsage = orgusage.NewMemoryCounter()
	degrade.Store(true)
	var launched int
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		launched++
		return "run-1", nil
	}
	ctx := auth.WithIdentity(context.Background(), auth.Identity{UserID: "u", TeamID: "t1", Role: identity.RoleMember, Kind: auth.KindWebhook})
	cfg := webhooks.Config{ID: "wh1", TenantID: "t1", Provider: webhooks.ProviderGitHub, ForkLane: true, ForkLanePool: "forks"}
	meta := webhookEventMeta{ProjectPath: "acme/widgets", SubjectID: "pr:7", Kind: "issue_comment"}
	target := forgeLaunchTarget{
		IdemKey: "idem-unreadable", BotID: "review-pr",
		Vars:        map[string]string{"pr_url": "https://github.com/acme/widgets/pull/7"},
		Trust:       store.RunTrustFork,
		ExpectedSHA: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		ForkAuthor:  &webhooks.ForkAuthorRef{Provider: "github", AuthorID: "4242"},
	}
	res := s.launchWebhookTarget(ctx, nil, cfg, meta, target, "hash", "1.2.3.4")
	if launched != 0 || res.Status == webhooks.StatusLaunched {
		t.Fatalf("an untrusted launch crossed an unreadable team (launched=%d, status=%q)", launched, res.Status)
	}
	if res.denial == nil || res.denial.reason != denyForkTeamUnreadable {
		t.Fatalf("denial = %+v, want %q", res.denial, denyForkTeamUnreadable)
	}
}

// TestForkLaunch_AnUnreadableOrgRefusesOnTheDeliverySurface: decision 5's
// other degraded arm — the team resolves but its ORG cannot be read. The
// org is where the fork bound's cap values live and the id both budgets
// meter on; a fork launch admitted past an unreadable org would meter on a
// guessed id, so it is refused.
func TestForkLaunch_AnUnreadableOrgRefusesOnTheDeliverySurface(t *testing.T) {
	s, _, _ := newDegradableServer(t)
	s.orgUsage = orgusage.NewMemoryCounter()
	// The team exists and reads fine, but belongs to an org the store
	// cannot answer for.
	idst := identity.NewMemoryStore()
	if _, err := idst.CreateTeam(context.Background(), identity.Team{
		ID: "t1", Name: "t1", Slug: "fork-org-t1", OrgID: "org-x", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	degrade := &atomic.Bool{}
	degrade.Store(true)
	svc, err := auth.NewService(auth.Config{
		Store:      orgUnreadableStore{Store: idst, err: context.DeadlineExceeded},
		Sessions:   auth.NewMemorySessionStore(),
		Signer:     mustSigner(t),
		SignupMode: auth.SignupOpen,
		RefreshTTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	s.authSvc = svc
	var launched int
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		launched++
		return "run-1", nil
	}
	ctx := auth.WithIdentity(context.Background(), auth.Identity{UserID: "u", TeamID: "t1", Role: identity.RoleMember, Kind: auth.KindWebhook})
	cfg := webhooks.Config{ID: "wh1", TenantID: "t1", Provider: webhooks.ProviderGitHub, ForkLane: true, ForkLanePool: "forks"}
	meta := webhookEventMeta{ProjectPath: "acme/widgets", SubjectID: "pr:7", Kind: "issue_comment"}
	target := forgeLaunchTarget{
		IdemKey: "idem-org-unreadable", BotID: "review-pr",
		Vars:        map[string]string{"pr_url": "https://github.com/acme/widgets/pull/7"},
		Trust:       store.RunTrustFork,
		ExpectedSHA: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		ForkAuthor:  &webhooks.ForkAuthorRef{Provider: "github", AuthorID: "4242"},
	}
	res := s.launchWebhookTarget(ctx, nil, cfg, meta, target, "hash", "1.2.3.4")
	if launched != 0 || res.Status == webhooks.StatusLaunched {
		t.Fatalf("an untrusted launch crossed an unreadable org (launched=%d, status=%q)", launched, res.Status)
	}
	if res.denial == nil || res.denial.reason != denyForkOrgUnreadable {
		t.Fatalf("denial = %+v, want %q", res.denial, denyForkOrgUnreadable)
	}
}

// orgUnreadableStore reads teams through the wrapped store and fails every
// org read — the degraded-org arm's harness.
type orgUnreadableStore struct {
	identity.Store
	err error
}

func (o orgUnreadableStore) GetOrg(ctx context.Context, id string) (identity.Org, error) {
	return identity.Org{}, o.err
}

// TestSweepDueRetries_AnUntrustedRunOnAnUnreadableTeamReArms: the same
// fail-closed arm through the RETRY SWEEPER — the surface the ADR names
// for it. The denial is transient (a store blip), so the sweeper re-arms
// the retry instead of abandoning a paid run.
func TestSweepDueRetries_AnUntrustedRunOnAnUnreadableTeamReArms(t *testing.T) {
	st := newFakeRetryStore()
	st.claimWins["run-a"] = true
	st.loadRun["run-a"] = &store.Run{
		ID: "run-a", Status: store.RunStatusFailedResumable,
		BotID: "review-pr", ProjectPath: "acme/widgets",
		Trust:             store.RunTrustFork,
		ForkAuthorSubject: string(orgusage.ForkAuthorSubject("t1", "github", "4242")),
	}
	resumer := &fakeResumer{}
	s := newRetrySweeperServer(t, st, resumer)
	// A degraded identity read: the team the fork run's budget hangs on
	// cannot be read, which for an untrusted subject is a refusal. The
	// degrade flag stays on for the whole sweep.
	degrade := &atomic.Bool{}
	degrade.Store(true)
	svc, err := auth.NewService(auth.Config{
		Store:      degradedTeamRead{Store: identity.NewMemoryStore(), err: context.DeadlineExceeded, on: degrade},
		Sessions:   auth.NewMemorySessionStore(),
		Signer:     mustSigner(t),
		SignupMode: auth.SignupOpen,
		RefreshTTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	s.authSvc = svc

	at := time.Now().UTC().Add(-time.Minute)
	s.sweepDueRetries(context.Background(), &fakeRetryLister{refs: []mongostore.RetryDueRef{dueRef("run-a", at)}}, resumer, time.Now().UTC())

	if len(resumer.calls) != 0 {
		t.Fatalf("an untrusted run was resumed over an unreadable team (%d calls) — the sweeper must refuse it", len(resumer.calls))
	}
	if len(st.abandoned) != 0 {
		t.Fatalf("the retry was abandoned (%v) — an unreadable team is a blip, and a paid run's retry re-arms", st.abandoned)
	}
	if _, ok := st.rearmed["run-a"]; !ok {
		t.Fatalf("the retry was not re-armed (rearmed=%v) — the refusal must be transient so the sweep retries after the blip", st.rearmed)
	}
}

// mustSigner builds a JWT signer for the sweeper witness's auth service.
func mustSigner(t *testing.T) *auth.JWTSigner {
	t.Helper()
	key := make([]byte, 32)
	signer, err := auth.NewJWTSigner(base64.RawStdEncoding.EncodeToString(key), 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

// forkSpecPublisher records the LaunchSpec the REAL launch path hands the
// run service's publisher — the oracle for what the lane stamps, one seam
// past the launcher every other test doubles.
type forkSpecPublisher struct {
	mu    sync.Mutex
	specs []runview.LaunchSpec
}

func (p *forkSpecPublisher) SubmitLaunch(_ context.Context, _ string, spec runview.LaunchSpec, _ *ir.Workflow, _ *runview.CompiledSource) (runview.LaunchSubmission, error) {
	p.mu.Lock()
	p.specs = append(p.specs, spec)
	p.mu.Unlock()
	return runview.LaunchSubmission{Status: store.RunStatusQueued, QueuePosition: 1}, nil
}

func (*forkSpecPublisher) CancelRun(context.Context, string) error { return nil }
func (*forkSpecPublisher) CancelRunWithReason(context.Context, string, store.RunEndReason) error {
	return nil
}
func (*forkSpecPublisher) SubmitResume(context.Context, runview.ResumeSpec, *ir.Workflow, *runview.CompiledSource) error {
	return nil
}

func (p *forkSpecPublisher) only(t *testing.T) runview.LaunchSpec {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.specs) != 1 {
		t.Fatalf("the lane published %d launches, want exactly 1", len(p.specs))
	}
	return p.specs[0]
}

// TestForkGesture_TheRealLauncherStampsTheSubjectOnTheSpec: the carrier
// chain's first hop, driven on the REAL path end to end — a real delivery,
// the real admission, the real launcher (no webhookLaunchBot double), a
// real run service whose publisher alone is a double. The LaunchSpec is
// what the publisher persists the document and the wire from, so the
// subject MUST be on it: the admission composed it (gateMonthlyCaps stamps
// it back onto the target), and launchWebhookBot is the only site that
// threads it onto the spec.
//
// Mutation: drop `ForkAuthorSubject: forkAuthorSubject` from
// launchWebhookBot's LaunchSpec (webhooks_gitlab.go) → the spec carries an
// empty subject → red on its own assertion, while every other fork-gesture
// witness stays green (the run still launches; only the dollars half of
// the bound goes dark).
func TestForkGesture_TheRealLauncherStampsTheSubjectOnTheSpec(t *testing.T) {
	fx := newForkGestureFixture(t)
	// The REAL launcher: no test double between the delivery and the
	// LaunchSpec. The lane's default bot resolves from the catalog the way
	// production's does.
	fx.s.webhookLaunchBot = nil
	if err := os.WriteFile(filepath.Join(fx.s.cfg.WorkDir, "bots", "review-pr.bot"), []byte(forkReviewProbeBot), 0o644); err != nil {
		t.Fatal(err)
	}
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pub := &forkSpecPublisher{}
	fx.s.runs = newTestRunviewService(t, "", runview.WithStore(rs), runview.WithLaunchPublisher(pub))

	w := fx.replay(t, 3001, "/revi")
	if w.Code != http.StatusAccepted {
		t.Fatalf("code=%d body=%s — an admitted gesture must launch through the real lane", w.Code, w.Body.String())
	}
	spec := pub.only(t)
	// The subject the gate composed and stamped back onto the target: the
	// author's own budget, keyed on the numeric forge id (orgless team t1 →
	// metered on the team id).
	wantSubject := string(orgusage.ForkAuthorSubject("t1", "github", "4242"))
	if spec.ForkAuthorSubject != wantSubject {
		t.Fatalf("spec.ForkAuthorSubject = %q, want %q — the publisher persists the document and the wire from this spec, so the runner's second charge and every resume would meter nobody", spec.ForkAuthorSubject, wantSubject)
	}
	if spec.Trust != store.RunTrustFork {
		t.Fatalf("spec.Trust = %q, want %q — the run must read untrusted on every carrier", spec.Trust, store.RunTrustFork)
	}
	if spec.RepoSHAExpected != forkForkPR().HeadSHA {
		t.Fatalf("spec.RepoSHAExpected = %q, want the admitted head commit — the pin and the subject are one guarantee (the runner compares before it charges)", spec.RepoSHAExpected)
	}
	// The lane's pool (ADR-162 Q3): the publisher stamps it as the run's
	// RunnerPool, so the fork run lands on that pool alone.
	//
	// Mutation: drop ForkLanePool from launchWebhookBot's spec → "" (red
	// here; the publisher would then refuse the launch after the gate).
	if spec.ForkLanePool != "forks" {
		t.Fatalf("spec.ForkLanePool = %q, want the config's \"forks\" — the publisher stamps the run's pool from it", spec.ForkLanePool)
	}
	// Decision 4's floor on the routing pre-flights: an untrusted launch
	// certifies NO operator-decided var names (trustedVarNamesFor). The
	// fixture's config carries an operator layer (gate_context), so a launch
	// certifying it would let a payload-derived var of the same name read as
	// operator-decided at RouteViewForLaunch.
	//
	// Mutation: trustedVarNamesFor returns trustedVarNames(m) whatever the
	// trust → the spec certifies [gate_context] (red on this assertion).
	if len(spec.TrustedVarNames) != 0 {
		t.Fatalf("spec.TrustedVarNames = %v, want none — the base repository's operator layer certifies nothing on a run whose code it did not write", spec.TrustedVarNames)
	}
	// Both budgets metered on the way in, exactly as the stubbed twin
	// already proved — the real lane changes nothing there.
	now := time.Now().UTC()
	for subj, want := range map[orgusage.Subject]int{
		orgusage.OrgSubject("t1"):                          1,
		orgusage.ForkAuthorSubject("t1", "github", "4242"): 1,
	} {
		u, err := fx.counter.Usage(context.Background(), subj, now)
		if err != nil {
			t.Fatal(err)
		}
		if u.Runs != want {
			t.Fatalf("subject %s metered %d run(s), want %d", subj, u.Runs, want)
		}
	}
}

// forkReviewProbeBot is the lane's default bot, compilable: the real
// launcher resolves it from the catalog and the run service compiles it
// before the publisher is asked.
const forkReviewProbeBot = "workflow review_pr:\n  worktree: none\n  entry: done\n"
