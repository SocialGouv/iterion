package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/budgetfloor"
	"github.com/SocialGouv/iterion/pkg/credusage"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/orgusage"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// The repository identity that BOUNDS (launchSubject.Repo) and METERS
// (spec.ProjectPath → run.ProjectPath → repoForSpend) a board run — and the
// repo audience a BYOK key resolves for — must be the server's derivation,
// never a bot-arg a member typed. These tests walk boardCardRepoIdentity's
// four quadrants and the write routes that keep the class out of the cards.
func newRepoIdentityServer(t *testing.T, quota budgetfloor.RepoQuota, spent float64) (*Server, *store.FilesystemRunStore, *tierPublisher) {
	t.Helper()
	pub := &tierPublisher{}
	s, rs := newTeamForkServer(t, pub)
	s.orgUsage = orgusage.NewMemoryCounter()
	s.credUsage = credusage.NewMemoryCounter()
	withFloor(t, s, budgetfloor.Policy{RepoQuotas: []budgetfloor.RepoQuota{quota}})
	if spent > 0 {
		seedRepoUsage(t, s.credUsage, quota.Repo, spent, 3)
	}
	return s, rs, pub
}

// The write routes a member drives (POST/PATCH /pipeline-board/tasks) persist
// bot args minus the reserved launch context: a forged
// __iterion_project_path must never reach a card.
func TestBoardDispatch_AForgedStampNeverDecidesTheRepoIdentity(t *testing.T) {
	s, rs, pub := newRepoIdentityServer(t, budgetfloor.RepoQuota{Repo: "acme/widgets", MonthlyUSD: 10}, 12.0)
	pub.onLaunch = func(runID string) { finishRunAs(t, rs, runID, store.RunStatusFinished) }
	// The forged card: what a PATCH of the board task API would leave behind
	// pre-fix — the clone URL honest, the slug whatever the client typed.
	err := s.processBoardCard(boundedCtx(t), "t1", native.Issue{
		ID: "native:forged", Bot: "probe", State: native.StateReady,
		BotArgs: map[string]string{
			boardRepoURLKey:     "https://github.com/acme/widgets.git",
			boardProjectPathKey: "harmless/other",
		},
	})
	if !errors.Is(err, errCardLaunchRefused) {
		t.Fatalf("processBoardCard = %v, want errCardLaunchRefused — an unverifiable stamp must not move the bound off the repository the card clones", err)
	}
	if n := len(pub.specs); n != 0 {
		t.Fatalf("the forged card published %d launch(es), want 0", n)
	}
}

// A stamp is honored only when the card names an integration OF ITS TENANT —
// and even then the clone URL re-derived against that integration's forge
// base decides (the REST lane's derivation, sub-path bases included), not
// the card's word.
func TestBoardDispatch_TheStampNeedsAnIntegrationOfTheTenant(t *testing.T) {
	s, rs, pub := newRepoIdentityServer(t, budgetfloor.RepoQuota{Repo: "acme/widgets", MonthlyUSD: 10}, 12.0)
	pub.onLaunch = func(runID string) { finishRunAs(t, rs, runID, store.RunStatusFinished) }
	seedIntegrationConfig(t, s, webhooks.Config{
		ID: "wh-sub", TenantID: "t1", Provider: webhooks.ProviderGitHub,
		ForgeBaseURL: "https://forge.example/gitlab",
	})
	err := s.processBoardCard(boundedCtx(t), "t1", native.Issue{
		ID: "native:sub", Bot: "probe", State: native.StateReady,
		BotArgs: map[string]string{
			boardWebhookIDKey:   "wh-sub",
			boardRepoURLKey:     "https://forge.example/gitlab/acme/widgets.git",
			boardProjectPathKey: "harmless/other",
		},
	})
	if !errors.Is(err, errCardLaunchRefused) {
		t.Fatalf("processBoardCard = %v, want errCardLaunchRefused — the slug must come from the clone URL against the integration's base, not the client's stamp", err)
	}
	if n := len(pub.specs); n != 0 {
		t.Fatalf("the forged card published %d launch(es), want 0", n)
	}
}

// The honest quadrants keep working: a webhook event that names the repo
// without carrying a clone URL stamps the payload's own project path, and a
// card naming no integration falls back to the clone URL alone.
func TestBoardDispatch_TheHonestIdentitiesKeepWorking(t *testing.T) {
	t.Run("the payload stamp stands without a clone URL", func(t *testing.T) {
		s, rs, pub := newRepoIdentityServer(t, budgetfloor.RepoQuota{Repo: "unrelated/other", MonthlyUSD: 10}, 0)
		pub.onLaunch = func(runID string) { finishRunAs(t, rs, runID, store.RunStatusFinished) }
		seedIntegrationConfig(t, s, webhooks.Config{ID: "wh-t1", TenantID: "t1", Provider: webhooks.ProviderGitHub})
		if err := s.processBoardCard(boundedCtx(t), "t1", native.Issue{
			ID: "native:nourl", Bot: "probe", State: native.StateReady,
			BotArgs: map[string]string{
				boardWebhookIDKey:   "wh-t1",
				boardProjectPathKey: "acme/widgets",
			},
		}); err != nil {
			t.Fatalf("processBoardCard = %v, want nil — a webhook event without a clone URL still names its repository", err)
		}
		if got := pub.only(t).ProjectPath; got != "acme/widgets" {
			t.Errorf("launched spec.ProjectPath = %q, want the payload's own %q", got, "acme/widgets")
		}
	})

	t.Run("a card naming no integration keeps the clone-URL fallback", func(t *testing.T) {
		// The sub-path caveat forge.ProjectPathFromCloneURL documents: the
		// fallback derives "gitlab/acme/widgets" — pinned here as the
		// documented behavior for a card that cannot prove provenance.
		s, rs, pub := newRepoIdentityServer(t, budgetfloor.RepoQuota{Repo: "unrelated/other", MonthlyUSD: 10}, 0)
		pub.onLaunch = func(runID string) { finishRunAs(t, rs, runID, store.RunStatusFinished) }
		if err := s.processBoardCard(boundedCtx(t), "t1", native.Issue{
			ID: "native:hand", Bot: "probe", State: native.StateReady,
			BotArgs: map[string]string{boardRepoURLKey: "https://forge.example/gitlab/acme/widgets.git"},
		}); err != nil {
			t.Fatalf("processBoardCard = %v, want nil", err)
		}
		if got := pub.only(t).ProjectPath; got != "gitlab/acme/widgets" {
			t.Errorf("launched spec.ProjectPath = %q, want the documented fallback %q", got, "gitlab/acme/widgets")
		}
	})
}

// ensureBoardCard stamps the slug whenever the payload names it — not only
// when it carries a clone URL: the slug does not depend on the clone, and an
// event without one would leave the card without the identity its quota
// reads.
func TestEnsureBoardCard_StampsTheSlugWithoutACloneURL(t *testing.T) {
	pub := &tierPublisher{}
	s, _ := newTeamForkServer(t, pub)
	board, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("native.NewStore: %v", err)
	}
	s.cfg.CloudBoardFor = func(string) native.BoardStore { return board }
	s.ensureBoardCard(boundedCtx(t), webhooks.Config{TenantID: "t1", Provider: webhooks.ProviderGitHub},
		webhooks.CommandRoute{BotID: "probe", Mode: "board"},
		map[string]string{"pr_url": "https://github.com/Acme/Widgets/pull/7"},
		webhookEventMeta{Kind: "note", ProjectPath: "Acme/Widgets", SubjectID: "note:1", SubjectURL: "https://github.com/Acme/Widgets/pull/7"},
		"", "", "")
	cards, err := board.List(native.ListFilter{})
	if err != nil || len(cards) != 1 {
		t.Fatalf("board.List = %v, %d cards, want 1", err, len(cards))
	}
	if got := cards[0].BotArgs[boardProjectPathKey]; got != "Acme/Widgets" {
		t.Errorf("the card's %s = %q, want the payload's own %q — an event without a clone URL still names its repository", boardProjectPathKey, got, "Acme/Widgets")
	}
}

// The create route a member drives never persists the reserved launch
// context: the member's own var lands, the forged identity keys do not.
func TestPipelineBoardTaskCreateDropsTheReservedLaunchContext(t *testing.T) {
	env := newPipelineTierEnv(t)
	env.seedRow(t, "t1", "probe", tierForkBot)
	card := env.createCard(t, `{"bot":"probe","title":"injected","bot_args":{`+
		`"member_var":"v",`+
		`"`+boardProjectPathKey+`":`+quoteJSON("harmless/other")+`,`+
		`"`+boardWebhookIDKey+`":`+quoteJSON("wh-x")+`,`+
		`"`+boardRepoURLKey+`":`+quoteJSON("https://github.com/acme/widgets.git")+`}}`)
	if card.BotArgs["member_var"] != "v" {
		t.Fatalf("the create path dropped the member's own var (%v) — this test only means something while the door is open", card.BotArgs)
	}
	for _, res := range native.ReservedLaunchArgs {
		if got, ok := card.BotArgs[res]; ok {
			t.Errorf("the create route persisted the reserved %s = %q — launch context is the server's stamp, not the client's", res, got)
		}
	}
}

// The update route keeps the STORED card's launch context across a wholesale
// bot-args patch: the member's own vars land, the payload's identity the
// client never wrote survives.
func TestPipelineBoardTaskUpdateKeepsTheStoredLaunchContext(t *testing.T) {
	env := newPipelineTierEnv(t)
	env.seedRow(t, "t1", "probe", tierForkBot)
	stored, err := env.board.Create(native.Issue{
		Title: "webhook card", State: native.StateReady, Bot: "probe",
		BotArgs: map[string]string{
			"member_var":        "v1",
			boardRepoURLKey:     "https://github.com/acme/widgets.git",
			boardProjectPathKey: "acme/widgets",
			boardWebhookIDKey:   "wh-t1",
		},
	})
	if err != nil {
		t.Fatalf("board.Create: %v", err)
	}
	body := `{"bot_args":{"member_var2":"v2","` + boardProjectPathKey + `":` + quoteJSON("harmless/other") + `}}`
	r := env.req(http.MethodPatch, "/api/v1/pipeline-board/tasks/"+stored.ID, body)
	r.SetPathValue("id", stored.ID)
	w := httptest.NewRecorder()
	env.srv.handlePipelineBoardTaskUpdate(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("update card = %d %s, want 200", w.Code, w.Body.String())
	}
	after, err := env.board.Get(stored.ID)
	if err != nil {
		t.Fatalf("board.Get: %v", err)
	}
	if after.BotArgs["member_var2"] != "v2" {
		t.Errorf("the patch's own var did not land: %v", after.BotArgs)
	}
	if _, carried := after.BotArgs["member_var"]; carried {
		t.Errorf("a wholesale patch must replace the member's own vars: %v", after.BotArgs)
	}
	for _, res := range native.ReservedLaunchArgs {
		if got, ok := after.BotArgs[res]; ok && got != stored.BotArgs[res] {
			t.Errorf("the patch rewrote the reserved %s to %q, want the stored %q", res, got, stored.BotArgs[res])
		}
	}
	if after.BotArgs[boardProjectPathKey] != "acme/widgets" {
		t.Errorf("the card's %s = %q, want the stored %q — a member PATCH must not forge the repository identity", boardProjectPathKey, after.BotArgs[boardProjectPathKey], "acme/widgets")
	}
}

// native.ReservedLaunchArgs and the server's board keys are ONE list: a key
// the dispatcher lifts but the write routes do not strip (or the reverse)
// would leave the class a member-writable identity again.
func TestReservedLaunchArgsMatchTheBoardKeys(t *testing.T) {
	known := map[string]bool{
		boardRepoURLKey:         true,
		boardRepoRefKey:         true,
		boardProjectPathKey:     true,
		boardKeyOverridesKey:    true,
		boardSecretOverridesKey: true,
		boardWebhookIDKey:       true,
	}
	for _, res := range native.ReservedLaunchArgs {
		if !known[res] {
			t.Errorf("native.ReservedLaunchArgs lists %q, which no board key constant names — the two lists must be one class", res)
		}
		delete(known, res)
	}
	for k := range known {
		t.Errorf("board key %s is missing from native.ReservedLaunchArgs — the write routes would leave it member-writable", k)
	}
}

// The forged-stamp chain replayed END TO END through the door the write
// routes were found to leave open LAST: a comment. The comment door stamps
// bot args wholesale, so a member able to write a comment could carry the
// __iterion_webhook_id + __iterion_project_path pair (ids readable on any
// webhook-created card of the team) and re-key the run's meter, bound and
// BYOK audience one gate below the filtered create/patch doors. The route
// here is the SAME mounting BoardAPI registers, and the dispatch is the
// cloud board dispatcher's common path.
func TestBoardDispatch_AStampForgedByACommentNeverLaunches(t *testing.T) {
	s, rs, pub := newRepoIdentityServer(t, budgetfloor.RepoQuota{Repo: "acme/widgets", MonthlyUSD: 10}, 12.0)
	pub.onLaunch = func(runID string) { finishRunAs(t, rs, runID, store.RunStatusFinished) }
	seedIntegrationConfig(t, s, webhooks.Config{ID: "wh-t1", TenantID: "t1", Provider: webhooks.ProviderGitHub})

	// The honest webhook card: the server stamped acme/widgets under the
	// team's integration, no clone URL — the quadrant where the stamp
	// stands. Over quota, so the honest identity is REFUSED.
	board, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("native.NewStore: %v", err)
	}
	stamped, err := board.Create(native.Issue{
		Title: "webhook card", State: native.StateReady, Bot: "probe",
		BotArgs: map[string]string{
			boardProjectPathKey: "acme/widgets",
			boardWebhookIDKey:   "wh-t1",
		},
	})
	if err != nil {
		t.Fatalf("board.Create: %v", err)
	}

	// The forge attempt, through the mounting itself: one comment carrying
	// the forged pair.
	api := &native.BoardAPI{Resolve: func(*http.Request) (native.BoardStore, error) { return board, nil }}
	mux := http.NewServeMux()
	api.RegisterRoutesWithMiddleware(mux, "", nil)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	body := strings.NewReader(`{"author":"member","body":"run this","bot":"probe","bot_args":{` +
		`"member_var2":"v2",` +
		`"` + boardProjectPathKey + `":` + quoteJSON("harmless/other") + `,` +
		`"` + boardWebhookIDKey + `":` + quoteJSON("wh-t1") + `,` +
		`"` + boardKeyOverridesKey + `":"{}"}}`)
	resp, err := http.Post(srv.URL+"/issues/"+stamped.ID+"/comments", "application/json", body)
	if err != nil {
		t.Fatalf("POST comment: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST comment = %d", resp.StatusCode)
	}
	after, err := board.Get(stamped.ID)
	if err != nil {
		t.Fatalf("board.Get: %v", err)
	}
	if after.BotArgs["member_var2"] != "v2" {
		t.Fatalf("the comment's own var did not land (%v) — this test only means something while the door is open", after.BotArgs)
	}
	if got := after.BotArgs[boardProjectPathKey]; got != "acme/widgets" {
		t.Fatalf("the comment forged %s = %q, want the server's stamp %q — the identity a run is metered and bounded by is not a comment's to write", boardProjectPathKey, got, "acme/widgets")
	}
	if got := after.BotArgs[boardWebhookIDKey]; got != "wh-t1" {
		t.Fatalf("the comment rewrote %s to %q, want the stored %q", boardWebhookIDKey, got, "wh-t1")
	}
	if _, carried := after.BotArgs[boardKeyOverridesKey]; carried {
		t.Fatalf("the comment carried a BYOK override audience: %v", after.BotArgs)
	}

	// The dispatcher's common path, on what the card ACTUALLY carries.
	if err := s.processBoardCard(boundedCtx(t), "t1", *after); !errors.Is(err, errCardLaunchRefused) {
		t.Fatalf("processBoardCard = %v, want errCardLaunchRefused — the over-quota repository must be refused, not launched under a forged slug", err)
	}
	if n := len(pub.specs); n != 0 {
		t.Fatalf("the comment-forged chain published %d launch(es), want 0", n)
	}
}

// A clone URL the forge served with a trailing slash spells the SAME
// repository as the bare .git form, and the meter must key it the same: an
// on-base derivation keeping its own trim turned "…/acme/widgets.git/" into
// a key no quota aims at, and the over-ceiling repository launched.
func TestBoardDispatch_ATrailingSlashCloneURLStaysOneQuotaKey(t *testing.T) {
	t.Run("the over-quota repository is refused", func(t *testing.T) {
		s, rs, pub := newRepoIdentityServer(t, budgetfloor.RepoQuota{Repo: "acme/widgets", MonthlyUSD: 10}, 12.0)
		pub.onLaunch = func(runID string) { finishRunAs(t, rs, runID, store.RunStatusFinished) }
		seedIntegrationConfig(t, s, webhooks.Config{ID: "wh-t1", TenantID: "t1", Provider: webhooks.ProviderGitHub})
		err := s.processBoardCard(boundedCtx(t), "t1", native.Issue{
			ID: "native:slash", Bot: "probe", State: native.StateReady,
			BotArgs: map[string]string{
				boardWebhookIDKey:   "wh-t1",
				boardRepoURLKey:     "https://github.com/acme/widgets.git/",
				boardProjectPathKey: "acme/widgets",
			},
		})
		if !errors.Is(err, errCardLaunchRefused) {
			t.Fatalf("processBoardCard = %v, want errCardLaunchRefused — the .git/ spelling is the same repository, its ceiling still applies", err)
		}
		if n := len(pub.specs); n != 0 {
			t.Fatalf("the trailing-slash spelling published %d launch(es), want 0", n)
		}
	})

	t.Run("the metered key is the forge slug either spelling", func(t *testing.T) {
		s, rs, pub := newRepoIdentityServer(t, budgetfloor.RepoQuota{Repo: "acme/widgets", MonthlyUSD: 10}, 0)
		pub.onLaunch = func(runID string) { finishRunAs(t, rs, runID, store.RunStatusFinished) }
		seedIntegrationConfig(t, s, webhooks.Config{ID: "wh-t1", TenantID: "t1", Provider: webhooks.ProviderGitHub})
		if err := s.processBoardCard(boundedCtx(t), "t1", native.Issue{
			ID: "native:slash", Bot: "probe", State: native.StateReady,
			BotArgs: map[string]string{
				boardWebhookIDKey:   "wh-t1",
				boardRepoURLKey:     "https://github.com/acme/widgets.git/",
				boardProjectPathKey: "acme/widgets",
			},
		}); err != nil {
			t.Fatalf("processBoardCard = %v, want nil", err)
		}
		if got := pub.only(t).ProjectPath; got != "acme/widgets" {
			t.Errorf("launched spec.ProjectPath = %q, want %q — a split key reads its quota as half-used on each half", got, "acme/widgets")
		}
	})
}
