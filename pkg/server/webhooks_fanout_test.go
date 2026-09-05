package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/webhooks"
	"github.com/SocialGouv/iterion/pkg/webhooks/prforge"
)

// The fan-out lane: one repo webhook, two bots, each reacting to ITS OWN
// authors. These tests pin the properties that make it safe — a delivery must
// never launch a bot whose author filter excludes the sender, never drop a bot
// whose filter admits it, and never let two bots share one idempotency claim.

const ghRenovatePR = `{
  "action": "opened",
  "number": 8,
  "repository": {"id": 42, "full_name": "acme/widgets", "clone_url": "https://github.com/acme/widgets.git"},
  "pull_request": {"number": 8, "title": "chore(deps): bump lib", "body": "renovate",
    "html_url": "https://github.com/acme/widgets/pull/8", "state": "open",
    "head": {"ref": "renovate/lib-1.x", "sha": "def456", "repo": {"full_name": "acme/widgets"}}, "base": {"ref": "main"},
    "user": {"login": "socialgouv-renovate[bot]"}},
  "sender": {"login": "socialgouv-renovate[bot]"}
}`

// A human pushing a fix onto the dependency bot's PR: the SENDER is the human,
// the PR is still the bot's. Routing on the sender would hand the delivery to
// the reviewer and let it fill the shared gate context on a dependency PR.
const ghRenovatePRHumanPush = `{
  "action": "synchronize",
  "number": 8,
  "repository": {"id": 42, "full_name": "acme/widgets", "clone_url": "https://github.com/acme/widgets.git"},
  "pull_request": {"number": 8, "title": "chore(deps): bump lib", "body": "renovate",
    "html_url": "https://github.com/acme/widgets/pull/8", "state": "open",
    "head": {"ref": "renovate/lib-1.x", "sha": "fff999", "repo": {"full_name": "acme/widgets"}}, "base": {"ref": "main"},
    "user": {"login": "socialgouv-renovate[bot]"}},
  "sender": {"login": "alice"}
}`

// fanoutConfig is ghConfig plus the two-bot routing table the orchestrator
// produces for a repo with a dependency guard (author-exclusive) and a general
// reviewer (open) co-enabled.
func fanoutConfig(t *testing.T, s *Server) (webhooks.Config, string) {
	t.Helper()
	cfg, pt := ghConfig(t, s)
	cfg.BotIDs = []string{"dep-update-guard", "review-pr"}
	cfg.DefaultBotID = "" // >1 bot → the legacy selection is ambiguous
	depAuthors := []string{"dependabot[bot]", "*renovate[bot]"}
	cfg.BotRules = []webhooks.BotRule{
		{
			BotID:           "dep-update-guard",
			Events:          []string{bundle.ForgeEventPullRequest},
			AuthorAllowlist: depAuthors,
			LaunchVars:      map[string]string{"post_to_board": "false", "max_fix_iterations": "2"},
		},
		{
			BotID:          "review-pr",
			Events:         []string{bundle.ForgeEventPullRequest},
			AuthorDenylist: depAuthors, // materialised from the guard's exclusive claim
			LaunchVars:     map[string]string{"pr_review_mode": "inline"},
		},
	}
	return cfg, pt
}

type launchRecord struct {
	bot  string
	vars map[string]string
}

// recordingLauncher installs a launcher capturing every (bot, vars) launch and
// returns the slice it appends to.
func fanoutLauncher(s *Server) *[]launchRecord {
	var got []launchRecord
	s.webhookLaunchBot = func(_ context.Context, bot string, vars map[string]string, _, _, _ string, _, _ map[string]string) (string, error) {
		got = append(got, launchRecord{bot: bot, vars: vars})
		return "run-" + bot, nil
	}
	return &got
}

func botsOf(recs []launchRecord) []string {
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.bot)
	}
	return out
}

func TestFanOut_RenovatePRLaunchesOnlyTheGuard(t *testing.T) {
	s := newWebhookTestServer(t)
	got := fanoutLauncher(s)
	cfg, pt := fanoutConfig(t, s)

	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghRenovatePR, prforge.EventHeaderPullRequest, pt))

	if w.Code != http.StatusAccepted {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if bots := botsOf(*got); len(bots) != 1 || bots[0] != "dep-update-guard" {
		t.Fatalf("renovate PR must launch only the guard, got %v", bots)
	}
}

func TestFanOut_APushByAHumanKeepsTheBotsPR(t *testing.T) {
	s := newWebhookTestServer(t)
	got := fanoutLauncher(s)
	cfg, pt := fanoutConfig(t, s)
	cfg.ReviewOnSync = true // a synchronize only reviews when this is on

	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghRenovatePRHumanPush, prforge.EventHeaderPullRequest, pt))

	if bots := botsOf(*got); len(bots) != 1 || bots[0] != "dep-update-guard" {
		t.Fatalf("the PR belongs to the dependency bot whoever pushed to it, got %v", bots)
	}
}

func TestFanOut_HumanPRLaunchesOnlyTheReviewer(t *testing.T) {
	s := newWebhookTestServer(t)
	got := fanoutLauncher(s)
	cfg, pt := fanoutConfig(t, s)

	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghOpenPR, prforge.EventHeaderPullRequest, pt))

	if w.Code != http.StatusAccepted {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if bots := botsOf(*got); len(bots) != 1 || bots[0] != "review-pr" {
		t.Fatalf("human PR must launch only the reviewer, got %v", bots)
	}
}

func TestFanOut_BothMatchLaunchesBoth(t *testing.T) {
	s := newWebhookTestServer(t)
	got := fanoutLauncher(s)
	cfg, pt := fanoutConfig(t, s)
	// Drop the exclusivity: both rules now admit any author.
	cfg.BotRules[0].AuthorAllowlist = nil
	cfg.BotRules[1].AuthorDenylist = nil

	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghOpenPR, prforge.EventHeaderPullRequest, pt))

	if w.Code != http.StatusAccepted {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	bots := botsOf(*got)
	if len(bots) != 2 {
		t.Fatalf("both rules match → 2 launches, got %v", bots)
	}
	var body struct {
		Status   string `json:"status"`
		Launches []struct {
			Bot    string `json:"bot"`
			Status string `json:"status"`
			RunID  string `json:"run_id"`
		} `json:"launches"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v (%s)", err, w.Body.String())
	}
	if body.Status != webhooks.StatusLaunched || len(body.Launches) != 2 {
		t.Fatalf("aggregate response: %s", w.Body.String())
	}
	if body.Launches[0].RunID == body.Launches[1].RunID {
		t.Fatalf("each bot must get its own run: %s", w.Body.String())
	}
}

// The single most likely way to ship this broken: if both bots claimed the
// same idempotency key, the second would read the first one's claim as a
// replay and never launch.
func TestFanOut_PerBotIdempotencyKeysDiffer(t *testing.T) {
	base := "gh|t1|ghw|acme/widgets|8|def456"
	a := forgeIdemKey(base, "dep-update-guard", true)
	b := forgeIdemKey(base, "review-pr", true)
	if a == b {
		t.Fatalf("fan-out keys must differ, both = %s", a)
	}
	// A legacy (no BotRules) delivery must keep the historical bot-less key
	// byte for byte, so a redelivery in flight across the upgrade dedupes.
	if legacy := forgeIdemKey(base, "review-pr", false); legacy == b {
		t.Fatalf("legacy key must not carry the bot: %s", legacy)
	}
}

func TestFanOut_VarsAreIsolatedPerBot(t *testing.T) {
	s := newWebhookTestServer(t)
	got := fanoutLauncher(s)
	cfg, pt := fanoutConfig(t, s)
	cfg.BotRules[0].AuthorAllowlist = nil
	cfg.BotRules[1].AuthorDenylist = nil
	cfg.OperatorLaunchVars = map[string]string{"scope_notes": "operator pin"}
	// The manifest UNION carries a key one bot pins for itself: the union must
	// not override that bot's own value (that is the collision BotRule exists
	// to prevent), but must still reach the bot that declares nothing.
	cfg.LaunchVars = map[string]string{"max_fix_iterations": "99"}

	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghOpenPR, prforge.EventHeaderPullRequest, pt))

	recs := *got
	if len(recs) != 2 {
		t.Fatalf("want 2 launches, got %v", botsOf(recs))
	}
	byBot := map[string]map[string]string{}
	for _, r := range recs {
		byBot[r.bot] = r.vars
	}
	// injectForgePublishVars mutates the vars map in place, so a shared map
	// would hand both runs the same grant AND leak each bot's vars.
	if &recs[0].vars == &recs[1].vars {
		t.Fatal("bots must not share a vars map")
	}
	if got := byBot["dep-update-guard"]["max_fix_iterations"]; got != "2" {
		t.Fatalf("the cross-bot union overrode the guard's own value: %v", byBot["dep-update-guard"])
	}
	if got := byBot["review-pr"]["max_fix_iterations"]; got != "99" {
		t.Fatalf("a bot pinning nothing still gets the union value, got %q", got)
	}
	for bot, vars := range byBot {
		if vars["scope_notes"] != "operator pin" {
			t.Fatalf("%s: operator LaunchVars must win last, got %q", bot, vars["scope_notes"])
		}
	}
}

// A config written before BotRules existed must behave EXACTLY as before:
// one launch of the pinned review default, historical response shape.
func TestFanOut_LegacyConfigUnchanged(t *testing.T) {
	s := newWebhookTestServer(t)
	got := fanoutLauncher(s)
	cfg, pt := ghConfig(t, s) // BotIDs = ["review-pr"], no BotRules

	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghOpenPR, prforge.EventHeaderPullRequest, pt))

	if w.Code != http.StatusAccepted {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if bots := botsOf(*got); len(bots) != 1 || bots[0] != "review-pr" {
		t.Fatalf("legacy config: want one review-pr launch, got %v", bots)
	}
	if body := w.Body.String(); strings.Contains(body, "launches") {
		t.Fatalf("legacy config must keep the historical single response shape: %s", body)
	}
}

// A redelivery relaunches ONLY the bot whose launch failed; the one that
// succeeded stays deduped.
func TestFanOut_PartialFailureRetriesOnlyTheFailedBot(t *testing.T) {
	s := newWebhookTestServer(t)
	cfg, pt := fanoutConfig(t, s)
	cfg.BotRules[0].AuthorAllowlist = nil
	cfg.BotRules[1].AuthorDenylist = nil

	var got []launchRecord
	failReviewer := true
	s.webhookLaunchBot = func(_ context.Context, bot string, vars map[string]string, _, _, _ string, _, _ map[string]string) (string, error) {
		got = append(got, launchRecord{bot: bot, vars: vars})
		if bot == "review-pr" && failReviewer {
			return "", context.DeadlineExceeded
		}
		return "run-" + bot, nil
	}

	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghOpenPR, prforge.EventHeaderPullRequest, pt))
	if w.Code != http.StatusAccepted {
		t.Fatalf("a sibling failure must not fail the delivery: code=%d body=%s", w.Code, w.Body.String())
	}
	if len(got) != 2 {
		t.Fatalf("first delivery: want both attempted, got %v", botsOf(got))
	}

	failReviewer = false
	got = nil
	w2 := httptest.NewRecorder()
	s.handleGitHubWebhook(w2, ghReq(ghCtx(cfg), ghOpenPR, prforge.EventHeaderPullRequest, pt))
	if bots := botsOf(got); len(bots) != 1 || bots[0] != "review-pr" {
		t.Fatalf("redelivery must relaunch only the failed bot, got %v", bots)
	}
}

func TestFanOut_ReplayLaunchesNothing(t *testing.T) {
	s := newWebhookTestServer(t)
	got := fanoutLauncher(s)
	cfg, pt := fanoutConfig(t, s)
	cfg.BotRules[0].AuthorAllowlist = nil
	cfg.BotRules[1].AuthorDenylist = nil

	for range 2 {
		w := httptest.NewRecorder()
		s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghOpenPR, prforge.EventHeaderPullRequest, pt))
	}
	if bots := botsOf(*got); len(bots) != 2 {
		t.Fatalf("replay must not relaunch, got %v", bots)
	}
}

// The fork-PR and iterion-bot-author guards run BEFORE the fan-out: no rule,
// however permissive, may re-open those lanes.
func TestFanOut_ForkPRStillBlocked(t *testing.T) {
	s := newWebhookTestServer(t)
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		t.Fatal("fork PR must not launch any bot")
		return "", nil
	}
	cfg, pt := fanoutConfig(t, s)
	cfg.BlockForkPRs = true
	forkPR := strings.Replace(ghOpenPR,
		`"head": {"ref": "feature/x", "sha": "abc123", "repo": {"full_name": "acme/widgets"}}`,
		`"head": {"ref": "feature/x", "sha": "abc123", "repo": {"full_name": "mallory/widgets"}}`, 1)

	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), forkPR, prforge.EventHeaderPullRequest, pt))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), webhooks.StatusFiltered) {
		t.Fatalf("fork PR: code=%d body=%s", w.Code, w.Body.String())
	}
}

func TestRulesForEvent(t *testing.T) {
	rules := []webhooks.BotRule{
		{BotID: "guard", Events: []string{bundle.ForgeEventPullRequest}, AuthorAllowlist: []string{"*renovate[bot]"}},
		{BotID: "reviewer", Events: []string{bundle.ForgeEventPullRequest}, AuthorDenylist: []string{"*renovate[bot]"}},
		{BotID: "commander"}, // command-only: claims no event
		{BotID: "not-enabled", Events: []string{bundle.ForgeEventPullRequest}},
	}
	cfg := webhooks.Config{BotIDs: []string{"guard", "reviewer", "commander"}, BotRules: rules}

	for _, tc := range []struct {
		name, event, author string
		want                []string
	}{
		{"hosted renovate → guard", bundle.ForgeEventPullRequest, "renovate[bot]", []string{"guard"}},
		{"self-hosted app → guard", bundle.ForgeEventPullRequest, "socialgouv-renovate[bot]", []string{"guard"}},
		{"human → reviewer", bundle.ForgeEventPullRequest, "alice", []string{"reviewer"}},
		{"comment event claims nobody", bundle.ForgeEventPullRequestComment, "alice", nil},
		{"unknown event", "push", "alice", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, r := range cfg.RulesForEvent(tc.event, tc.author) {
				got = append(got, r.BotID)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("want %v, got %v", tc.want, got)
			}
		})
	}

	// A rule for a bot the webhook no longer permits must never launch.
	if rs := cfg.RulesForEvent(bundle.ForgeEventPullRequest, "nobody-special"); len(rs) != 1 || rs[0].BotID != "reviewer" {
		t.Fatalf("a de-scoped bot must be excluded, got %v", rs)
	}
}
