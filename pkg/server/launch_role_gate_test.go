package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/budgetfloor"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/eventbus"
	"github.com/SocialGouv/iterion/pkg/identity"
	mongostore "github.com/SocialGouv/iterion/pkg/store/mongo"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// Launching a run spends the org's budget and executes code in its
// workspace, so it takes at least the MEMBER rung of the team. The three
// human launch surfaces — POST /api/runs, POST /api/v1/triggers/emit and
// POST /api/v1/pipeline-board/tasks/{id}/launch — all reach that rung
// through gateLaunch, which is also where the principals with no human role
// (an inbound webhook, the daemons of this process) are exempted by kind.

// roleGateCaller is a signed-in user of the newRunScopeServer fixture: the
// standing their credential pins, which is exactly what gateLaunch reads.
func roleGateCaller(userID, teamID string, role identity.Role) auth.Identity {
	return auth.Identity{UserID: userID, Email: userID + "@x", TeamID: teamID, OrgID: "org-1", Role: role}
}

// denialToken reads the stable reason token out of a response envelope —
// the launch-gate denial and every httpError share the `error` key, so a
// refusal for another cause is distinguishable from this one.
func denialToken(body string) string {
	var out struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal([]byte(body), &out)
	return out.Error
}

// roleGateLadder is the verdict each rung owes on a launch surface.
var roleGateLadder = []struct {
	name     string
	identity auth.Identity
	refused  bool
}{
	{"viewer", roleGateCaller("u-viewer-a", "tenant-A", identity.RoleViewer), true},
	{"config_editor", roleGateCaller("u-configed-a", "tenant-A", identity.RoleConfigEditor), true},
	{"role-less", roleGateCaller("u-roleless", "tenant-A", ""), true},
	{"member", roleGateCaller("u-member-a", "tenant-A", identity.RoleMember), false},
	{"super-admin", auth.Identity{UserID: "root", TeamID: "tenant-A", IsSuperAdmin: true}, false},
}

// W2 — POST /api/runs, through requireAuth and a real bearer, so a role the
// JWT carries but the gate never reads would show up here.
func TestLaunchRunTakesTheMemberRung(t *testing.T) {
	srv, _ := newRunScopeServer(t)
	ts := fullStack(t, srv)

	post := func(t *testing.T, id auth.Identity) (int, string) {
		t.Helper()
		token, _, err := srv.signer.IssueAccess(id)
		if err != nil {
			t.Fatalf("mint bearer: %v", err)
		}
		// The handler decodes the body BEFORE the gate — the launch's subject
		// comes from it — so a body that does not decode never reaches the
		// rung. The probe names a bot the catalog does not hold: a principal
		// past the rung is then refused on the launch's own merits.
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/runs", strings.NewReader(`{"bot_id":"no-such-bot"}`))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST /api/runs: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, denialToken(string(body))
	}

	for _, tc := range roleGateLadder {
		t.Run(tc.name, func(t *testing.T) {
			code, token := post(t, tc.identity)
			if tc.refused {
				if code != http.StatusForbidden || token != denyMemberRoleRequired {
					t.Fatalf("POST /api/runs as %s = %d %q, want 403 %s", tc.name, code, token, denyMemberRoleRequired)
				}
				return
			}
			// Past the rung. The unknown bot is then refused on its own
			// merits (400 "resolve bot: …") — what matters is that it is
			// NOT the authorization refusal.
			if token == denyMemberRoleRequired {
				t.Fatalf("POST /api/runs as %s = %d %q: the rung refused a principal that clears it", tc.name, code, token)
			}
			if code != http.StatusBadRequest {
				t.Fatalf("POST /api/runs as %s = %d %q, want the unknown bot's own 400", tc.name, code, token)
			}
		})
	}
}

// W1 — POST /api/v1/pipeline-board/tasks/{id}/launch. The operator's
// "launch now" override of the admission queue: it bypasses the ranking, not
// the authorization, and not the org's caps.
func TestPipelineBoardLaunchTakesTheMemberRung(t *testing.T) {
	srv, _ := newRunScopeServer(t)
	board, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("native board store: %v", err)
	}
	srv.cfg.CloudBoardFor = func(string) native.BoardStore { return board }
	// The handler reads the card BEFORE the gate — the gate takes the card's
	// bot as its subject — so a missing board or card answers ahead of the
	// rung. The card names a bot the catalog does not hold: a principal past
	// the rung is then refused on the launch's own merits, never admitted.
	card, err := board.Create(native.Issue{Title: "launch me", Bot: "no-such-bot"})
	if err != nil {
		t.Fatalf("seed card: %v", err)
	}

	launch := func(t *testing.T, id auth.Identity) (int, string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/pipeline-board/tasks/"+card.ID+"/launch", strings.NewReader(`{}`))
		req.SetPathValue("id", card.ID)
		req = req.WithContext(auth.WithIdentity(req.Context(), id))
		rec := httptest.NewRecorder()
		srv.handlePipelineBoardTaskLaunch(rec, req)
		return rec.Code, denialToken(rec.Body.String())
	}

	for _, tc := range roleGateLadder {
		t.Run(tc.name, func(t *testing.T) {
			code, token := launch(t, tc.identity)
			if tc.refused {
				if code != http.StatusForbidden || token != denyMemberRoleRequired {
					t.Fatalf("launch-now as %s = %d %q, want 403 %s", tc.name, code, token, denyMemberRoleRequired)
				}
				return
			}
			// Past the rung: the launch is attempted and refused on the
			// unknown bot (409, the launch's own refusal class).
			if token == denyMemberRoleRequired {
				t.Fatalf("launch-now as %s = %d %q: the rung refused a principal that clears it", tc.name, code, token)
			}
			if code != http.StatusConflict {
				t.Fatalf("launch-now as %s = %d %q, want the unknown bot's own 409", tc.name, code, token)
			}
		})
	}
}

// The card names the bot, so launch-now must tell the gate — the rule the
// dispatcher keeps for the same card (boarddispatch_launchgate_test.go):
// judged as ordinary work, a RESERVED bot faces the ceiling its own
// reservation lowered, and the operator's launch-now would be refused on the
// very band held for that bot. The card is therefore read BEFORE the gate.
func TestPipelineBoardLaunchCarriesTheCardsBotToTheGate(t *testing.T) {
	launchNow := func(t *testing.T, reserved string) (*httptest.ResponseRecorder, *countingPublisher) {
		t.Helper()
		pub := &countingPublisher{}
		// 2 slots, 1 held for `reserved`, 1 already running.
		s, rs := newGatedBoardServer(t, gateSpec{id: "t1", maxConcurrentRuns: 2}, pub)
		s.cfg.Store = fakeActiveStore{RunStore: rs, active: 1}
		withFloor(t, s, budgetfloor.Policy{Reservations: []budgetfloor.Reservation{
			{BotID: reserved, Reserve: budgetfloor.Reserve{ConcurrentRuns: 1}},
		}})
		board, err := native.NewStore(t.TempDir())
		if err != nil {
			t.Fatalf("native board store: %v", err)
		}
		s.cfg.CloudBoardFor = func(string) native.BoardStore { return board }
		card, err := board.Create(native.Issue{Title: "launch me", Bot: "probe"})
		if err != nil {
			t.Fatalf("seed card: %v", err)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/v1/pipeline-board/tasks/"+card.ID+"/launch", strings.NewReader(`{}`))
		req.SetPathValue("id", card.ID)
		req = req.WithContext(auth.WithIdentity(req.Context(),
			auth.Identity{UserID: "u1", TeamID: "t1", OrgID: "t1", Role: identity.RoleMember}))
		rec := httptest.NewRecorder()
		s.handlePipelineBoardTaskLaunch(rec, req)
		return rec, pub
	}
	t.Run("the card's bot holds a slot of its own", func(t *testing.T) {
		rec, pub := launchNow(t, "probe")
		if rec.Code == http.StatusTooManyRequests {
			t.Fatalf("launch-now was refused on the card's own reservation: %s", rec.Body.String())
		}
		if got := pub.count(); got != 1 {
			t.Fatalf("the run service was asked %d time(s), want 1 (%d %s)", got, rec.Code, rec.Body.String())
		}
	})
	t.Run("a slot held for another bot still refuses it", func(t *testing.T) {
		rec, pub := launchNow(t, "review-pr")
		if rec.Code != http.StatusTooManyRequests || denialToken(rec.Body.String()) != denyConcurrencyCap {
			t.Fatalf("launch-now = %d %q, want 429 %s — the free slot is held for review-pr: %s",
				rec.Code, denialToken(rec.Body.String()), denyConcurrencyCap, rec.Body.String())
		}
		if got := pub.count(); got != 0 {
			t.Errorf("the run service was asked %d time(s) for a card with no slot, want 0", got)
		}
	})
}

// W3 — POST /api/v1/triggers/emit. The emit is a pre-check (it publishes one
// event that fans out to however many subscriptions match), but the rung is
// not a pre-check: a viewer may not make this process launch work.
func TestEmitTriggerTakesTheMemberRung(t *testing.T) {
	srv, _ := newRunScopeServer(t)
	srv.cfg.TriggerStore = trigger.NewMemorySubscriptionStore()
	srv.cfg.EventsBus = eventbus.NewInProcBus(srv.logger)

	emit := func(t *testing.T, id auth.Identity) (int, string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/triggers/emit", strings.NewReader(`{"kind":"probe"}`))
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(auth.WithIdentity(req.Context(), id))
		rec := httptest.NewRecorder()
		srv.handleEmitTrigger(rec, req)
		return rec.Code, denialToken(rec.Body.String())
	}

	for _, tc := range roleGateLadder {
		t.Run(tc.name, func(t *testing.T) {
			code, token := emit(t, tc.identity)
			if tc.refused {
				if code != http.StatusForbidden || token != denyMemberRoleRequired {
					t.Fatalf("emit as %s = %d %q, want 403 %s", tc.name, code, token, denyMemberRoleRequired)
				}
				return
			}
			if token == denyMemberRoleRequired {
				t.Fatalf("emit as %s = %d %q: the rung refused a principal that clears it", tc.name, code, token)
			}
			if code != http.StatusAccepted {
				t.Fatalf("emit as %s = %d %q, want 202", tc.name, code, token)
			}
		})
	}
}

// The exemption is the KIND, not a role: the principals below carry no team
// role at all — an inbound webhook delivery authorized by its config's
// secret, and the four daemons of this process — and each is admitted. A
// config-share editor link is NOT in the set: it authorizes editing one
// team's config and nothing else (ADR-078), so it stays on the rung.
func TestGateLaunchExemptsTheRolelessLaunchers(t *testing.T) {
	s := newOrgTestServer(t)
	ctx := seedGate(t, s, gateSpec{id: "t1"})
	id, _ := auth.FromContext(ctx)
	if id.Role != identity.RoleMember {
		t.Fatalf("seedGate identity Role = %q: the gate fixture must carry the rung production carries", id.Role)
	}

	t.Run("inbound webhook", func(t *testing.T) {
		webhookCtx := auth.WithIdentity(context.Background(), auth.Identity{
			UserID: "webhook:w1", TeamID: "t1", Kind: auth.KindWebhook,
		})
		if _, d := s.gateLaunch(webhookCtx, launchSubject{}); d != nil {
			t.Fatalf("a webhook principal was denied (%+v) — the inbound lane has no human role to carry", d)
		}
	})

	// The four daemon launchers, minted through the production helper with
	// each one's own actor: the board dispatcher, the cloud schedule tick,
	// the trigger spine and the retry sweeper.
	for _, actor := range []string{boardDispatcherActor, scheduledLaunchActor, triggerSpineActor, "owner-of-the-retried-run"} {
		t.Run("daemon "+actor, func(t *testing.T) {
			if _, d := s.gateLaunch(machineLaunchCtx(context.Background(), "t1", actor), launchSubject{}); d != nil {
				t.Fatalf("daemon %q was denied (%+v) — a daemon launch carries no role to judge", actor, d)
			}
		})
	}

	t.Run("config share link stays on the rung", func(t *testing.T) {
		shareCtx := auth.WithIdentity(context.Background(), auth.Identity{
			UserID: "share:s1", TeamID: "t1", Kind: auth.KindShare,
		})
		_, d := s.gateLaunch(shareCtx, launchSubject{})
		if d == nil || d.reason != denyMemberRoleRequired {
			t.Fatalf("denial = %+v, want %s: a config-share link edits config, it does not launch runs", d, denyMemberRoleRequired)
		}
	})
}

// The ORDER, not just the presence: the team read that follows the rung
// fails OPEN on a degraded store — quotas are operator policy, not a
// security boundary — so a rung sitting BEHIND that read would admit the
// viewer it exists to refuse on every store blip. Every other test here
// stays green when the block is moved down; this one is the witness of
// where it sits.
func TestMemberRungRefusesBeforeTheFailOpenTeamRead(t *testing.T) {
	s, degrade, _ := newDegradableServer(t)
	seedTeam(t, s, "t1", "acme")
	degrade.Store(true)
	ctx := auth.WithIdentity(context.Background(), auth.Identity{UserID: "u-viewer", TeamID: "t1", Role: identity.RoleViewer})
	adm, d := s.gateLaunch(ctx, launchSubject{})
	if d == nil {
		t.Fatalf("a viewer was admitted (admission=%+v) while the team read was degraded — the rung is behind a check that fails OPEN", adm)
	}
	if d.status != http.StatusForbidden || d.reason != denyMemberRoleRequired {
		t.Fatalf("denial = %+v, want 403 %s", d, denyMemberRoleRequired)
	}
}

// retryLaunchCtx is the retry sweeper's own producer: it must mint the same
// machine principal, or every automatic resume dies on the rung.
func TestRetryLaunchCtxMintsAMachinePrincipal(t *testing.T) {
	id, ok := auth.FromContext(retryLaunchCtx(context.Background(), mongostore.RetryDueRef{TenantID: "t1", ID: "run-1", OwnerID: "u1"}))
	if !ok {
		t.Fatal("retryLaunchCtx stamped no identity")
	}
	if !id.IsMachine() {
		t.Fatalf("identity = %+v: the sweeper's resume must be marked machine or the rung refuses it", id)
	}
	if id.TeamID != "t1" || id.UserID != "u1" {
		t.Fatalf("identity = %+v: the retry meters against the run's own tenant and owner", id)
	}
	// And it clears the rung by KIND. That is what makes the sweeper's
	// denial handling below a statement about the switch and not about a
	// reachable state: the gate this principal passes cannot answer
	// member_role_required.
	if !launchExemptPrincipal(id) {
		t.Fatalf("identity = %+v: the sweeper's principal must clear the rung by kind, or every automatic resume dies on it", id)
	}
}

// A machine principal is NOT a synthetic one: the two predicates answer
// different questions, and a reader of one must not silently acquire the
// other's population (every operator RBAC gate reads IsSynthetic).
func TestMachineAndSyntheticAreDistinctPopulations(t *testing.T) {
	machine := auth.Identity{UserID: "dispatcher", TeamID: "t1", Kind: auth.KindInternal}
	if !machine.IsMachine() || machine.IsSynthetic() {
		t.Fatalf("%+v: machine=%v synthetic=%v — KindInternal is machine only", machine, machine.IsMachine(), machine.IsSynthetic())
	}
	for _, kind := range []auth.IdentityKind{auth.KindWebhook, auth.KindShare} {
		syn := auth.Identity{UserID: "x", TeamID: "t1", Kind: kind}
		if !syn.IsSynthetic() || syn.IsMachine() {
			t.Fatalf("kind %q: synthetic=%v machine=%v", kind, syn.IsSynthetic(), syn.IsMachine())
		}
	}
	user := auth.Identity{UserID: "u1", TeamID: "t1", Role: identity.RoleMember}
	if user.IsMachine() || user.IsSynthetic() {
		t.Fatalf("%+v: a real user is neither", user)
	}
}

// The sweeper's denial switch defaults to PERMANENT: a reason it does not
// know abandons the retry and says why, instead of re-arming for ever. An
// authorization refusal is in that default class — it does not clear on its
// own the way a quota or a rate cap does — and so is a reason nobody has
// written yet. The sweeper's own principal is exempt from the rung (see
// above), so member_role_required cannot actually arrive here; what is
// witnessed is the default, for every reason that can.
func TestRetryDenialSwitchDefaultsToPermanent(t *testing.T) {
	for _, reason := range []string{denyMemberRoleRequired, denyNoWorkspace, denyOrgSuspended, "a_denial_reason_nobody_has_written_yet"} {
		if retryDenialIsTransient(reason) {
			t.Fatalf("%s classified transient: the sweeper would re-arm a run nobody may launch, for ever", reason)
		}
	}
	// Control: the caps that DO clear on their own stay transient.
	for _, reason := range []string{denyMonthlyRunQuota, denyConcurrencyCap} {
		if !retryDenialIsTransient(reason) {
			t.Fatalf("%s must stay transient", reason)
		}
	}
}

// The inbound webhook lane end to end: the handler's identity is the one the
// webhook middleware stamps (KindWebhook), and the delivery still launches.
func TestGenericWebhookStillLaunchesUnderItsOwnKind(t *testing.T) {
	s := newWebhookTestServer(t)
	var calls int
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		calls++
		return "run-g-1", nil
	}
	cfg := genConfig()
	ctx := gitlabCtx(cfg)
	id, _ := auth.FromContext(ctx)
	if id.Kind != auth.KindWebhook {
		t.Fatalf("webhook fixture Kind = %q, want %q — the middleware stamps it (middleware_webhook.go)", id.Kind, auth.KindWebhook)
	}
	w := httptest.NewRecorder()
	s.handleGenericWebhook(w, genReq(ctx, `{"bot":"review-pr","idempotency_key":"evt-rung-1"}`))
	if w.Code != http.StatusAccepted {
		t.Fatalf("generic webhook = %d %s, want 202", w.Code, w.Body.String())
	}
	if calls != 1 {
		t.Fatalf("launch stub called %d time(s), want 1", calls)
	}
}

// The ADR-103 ladder already refuses a viewer every mutation of a run
// addressed by id. Kept as the witness that the new rung is ADDITIVE: the
// choke point's own refusal must not become the only one, nor disappear.
func TestViewerStillCannotResumeARun(t *testing.T) {
	srv, _ := newRunScopeServer(t)
	code, _, _ := scoped(t, srv, http.MethodPost, "/api/runs/run-1/resume", roleGateCaller("u-viewer-a", "tenant-A", identity.RoleViewer))
	if code != http.StatusForbidden {
		t.Fatalf("viewer resuming run-1 = %d, want 403 from runScopeLadder", code)
	}
}
