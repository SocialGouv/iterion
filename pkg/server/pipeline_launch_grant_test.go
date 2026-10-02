package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// wireForgePublishForT1 gives team t1 a forge connection covering o/r, a
// public URL and a PR reader: a pull-request launch that composes its launch
// context mints a grant.
func wireForgePublishForT1(t *testing.T, s *Server) {
	t.Helper()
	s.forgeConnections = forge.NewMemoryConnectionStore()
	s.forgePublishTokens = NewForgePublishTokenRegistry()
	if err := s.forgeConnections.Create(context.Background(), forge.Connection{ID: "conn1", TenantID: "t1", Provider: forge.ProviderGitHub}); err != nil {
		t.Fatal(err)
	}
	s.cfg.PublicURL = "https://iterion.example"
	s.forgeIntegrations = forge.NewMemoryRepoIntegrationStore()
	s.webhookConfigs = webhooks.NewMemoryConfigStore()
	s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) {
		return &fakeGateClient{headSHA: "abc"}, nil
	}
}

// plantCard writes a card straight into the board store — the shape of a card
// written before the write paths dropped the grant, or through a writer that
// stores bot args verbatim.
func plantCard(t *testing.T, env *pipelineTierEnv, carried string) native.Issue {
	t.Helper()
	card, err := env.board.Create(native.Issue{
		Title: "Review PR 7", State: native.StateReady, Bot: "probe",
		BotArgs: map[string]string{
			"pr_url":                   "https://github.com/o/r/pull/7",
			store.ForgePublishTokenVar: carried,
			store.ForgePublishURLVar:   "https://collector.example/publish",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return *card
}

// "Launch now" composes a card's launch as the board dispatcher does: the
// grant the card carries is never pinned, and the run gets its own.
func TestPipelineLaunchNow_ACardsGrantIsNeverRepinned(t *testing.T) {
	for _, tc := range []struct{ name, carried string }{
		{"the mask a view showed", store.RedactedLaunchVar},
		{"an expired grant", "expired-grant-the-registry-no-longer-knows"},
		{"an earlier grant of the card's own team", "own-team-earlier-grant"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newPipelineTierEnv(t)
			wireForgePublishForT1(t, env.srv)
			registerPublishToken(t, env.srv, "own-team-earlier-grant", ForgePublishGrant{TeamID: "t1", ConnectionID: "conn1", Repo: "o/r"})
			card := plantCard(t, env, tc.carried)
			env.launchCard(t, card.ID)
			spec := env.pub.only(t)
			tok := spec.Vars[store.ForgePublishTokenVar]
			grant, ok := env.srv.forgePublishTokens.lookup(tok)
			if tok == tc.carried || !ok || grant.TeamID != "t1" {
				t.Fatalf("launched with token %q (registered=%v team=%q), want a fresh grant of t1", tok, ok, grant.TeamID)
			}
			if u := spec.Vars[store.ForgePublishURLVar]; !strings.HasPrefix(u, "https://iterion.example/") {
				t.Errorf("the run publishes to the card's endpoint %q", u)
			}
		})
	}
}

// A card carrying another team's live grant is refused, not laundered into a
// fresh grant — and a refused launch leaves the card where it was.
func TestPipelineLaunchNow_AnotherTeamsGrantIsRefused(t *testing.T) {
	env := newPipelineTierEnv(t)
	wireForgePublishForT1(t, env.srv)
	registerPublishToken(t, env.srv, "team-b-grant", ForgePublishGrant{TeamID: "team-b", ConnectionID: "conn-b", Repo: "o/r"})
	card := plantCard(t, env, "team-b-grant")

	r := env.req(http.MethodPost, "/api/v1/pipeline-board/tasks/"+card.ID+"/launch", "")
	r.SetPathValue("id", card.ID)
	w := httptest.NewRecorder()
	env.srv.handlePipelineBoardTaskLaunch(w, r)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("launch = %d %s, want 422", w.Code, w.Body.String())
	}
	env.pub.mu.Lock()
	launched := len(env.pub.specs)
	env.pub.mu.Unlock()
	if launched != 0 {
		t.Errorf("a refused launch published %d runs", launched)
	}
	cur, err := env.board.Get(card.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cur.State != native.StateReady {
		t.Errorf("the refused launch moved the card to %q", cur.State)
	}
}

// grantCount is how many grants the test registry holds.
func grantCount(t *testing.T, s *Server) int {
	t.Helper()
	reg, ok := s.forgePublishTokens.(*ForgePublishTokenRegistry)
	if !ok {
		t.Fatalf("grant registry is %T", s.forgePublishTokens)
	}
	reg.mu.RLock()
	defer reg.mu.RUnlock()
	return len(reg.tokens)
}

// claimDrift is a board whose launch claim always finds the card moved.
type claimDrift struct{ native.BoardStore }

func (claimDrift) SetStateFrom(string, string, string) (*native.Issue, bool, error) {
	return nil, false, nil
}

// A launch that never starts a run revokes the grant it minted: nothing holds
// the token, and every retry would otherwise leave one more grant occupying
// the registry until its TTL.
func TestALaunchThatNeverStartsRevokesTheGrantItMinted(t *testing.T) {
	t.Run("the board dispatcher, refused by admission", func(t *testing.T) {
		env := newPipelineTierEnv(t)
		wireForgePublishForT1(t, env.srv)
		suspended := identity.TeamStatusSuspended
		if _, err := env.srv.authStore().PatchTeam(context.Background(), "t1", identity.TeamPatch{Status: &suspended}); err != nil {
			t.Fatal(err)
		}
		card := plantCard(t, env, "")
		if err := env.srv.processBoardCard(boundedCtx(t), "t1", card); err == nil {
			t.Fatal("a suspended team's card launched")
		}
		if n := grantCount(t, env.srv); n != 0 {
			t.Fatalf("%d grant(s) left in the registry by a launch that never started", n)
		}
	})
	t.Run("launch now, whose claim finds the card moved", func(t *testing.T) {
		env := newPipelineTierEnv(t)
		wireForgePublishForT1(t, env.srv)
		card := plantCard(t, env, "")
		if _, err := env.srv.launchTicketNow(context.Background(), "t1", env.srv.runs, claimDrift{env.board}, &card); err == nil {
			t.Fatal("a launch whose claim failed reported success")
		}
		if n := grantCount(t, env.srv); n != 0 {
			t.Fatalf("%d grant(s) left in the registry by a launch that never started", n)
		}
	})
}

// The mask a read surface showed is never stored as a run's token — on every
// launch, including one with no pull request, which mints nothing.
func TestALaunchNeverStoresTheMaskAsItsToken(t *testing.T) {
	s := newQueueOutageHTTPTestServer(t)
	body, err := json.Marshal(map[string]any{
		"file_path": "pr.bot",
		"source":    "vars:\n  forge_publish_token: string = \"\"\n\nworkflow pr:\n  entry: done\n",
		"vars":      map[string]string{store.ForgePublishTokenVar: store.RedactedLaunchVar},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/runs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleLaunchRun(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("launch = %d %s, want 202", rec.Code, rec.Body.String())
	}
	var out struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.RunID == "" {
		t.Fatalf("decode launch response %s: %v", rec.Body.String(), err)
	}
	run, err := s.runs.RunStore().LoadRun(context.Background(), out.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := run.Inputs[store.ForgePublishTokenVar]; ok && got != "" {
		t.Fatalf("the run stores %s = %v, want no token", store.ForgePublishTokenVar, got)
	}
}

// repoPolicyPinning gives team t1's repo a launch policy that pins a publish
// token — an operator's own credential, filled into every launch of that repo.
func repoPolicyPinning(t *testing.T, s *Server, token string) {
	t.Helper()
	if err := s.forgeIntegrations.Create(context.Background(), forge.RepoIntegration{
		ID: "ri-pin", TenantID: "t1", ConnectionID: "conn1", RepoFullName: "o/r",
		LaunchVars: map[string]string{store.ForgePublishTokenVar: token},
	}); err != nil {
		t.Fatal(err)
	}
}

// A launch revokes the grant IT minted, and nothing else. The tokens that
// reach a launch from elsewhere — an operator's pin, a repo policy's — are
// not this launch's to end: revoking one leaves every later launch of that
// repo pinning a dead token, and an unresolvable pin is honoured, so those
// runs could never publish again.
func TestALaunchThatNeverStartsRevokesNothingItDidNotMint(t *testing.T) {
	const pinned = "policy-pinned-token"
	t.Run("the board dispatcher", func(t *testing.T) {
		env := newPipelineTierEnv(t)
		wireForgePublishForT1(t, env.srv)
		repoPolicyPinning(t, env.srv, pinned)
		if err := env.srv.forgePublishTokens.Register(pinned, ForgePublishGrant{TeamID: "t1", ConnectionID: "conn1", Repo: "o/r"}); err != nil {
			t.Fatal(err)
		}
		suspended := identity.TeamStatusSuspended
		if _, err := env.srv.authStore().PatchTeam(context.Background(), "t1", identity.TeamPatch{Status: &suspended}); err != nil {
			t.Fatal(err)
		}
		card := plantCard(t, env, "")
		if err := env.srv.processBoardCard(boundedCtx(t), "t1", card); err == nil {
			t.Fatal("a suspended team's card launched")
		}
		if _, live := env.srv.forgePublishTokens.lookup(pinned); !live {
			t.Fatal("a launch that started nothing revoked the token its repo policy pins")
		}
	})
	t.Run("launch now", func(t *testing.T) {
		env := newPipelineTierEnv(t)
		wireForgePublishForT1(t, env.srv)
		repoPolicyPinning(t, env.srv, pinned)
		if err := env.srv.forgePublishTokens.Register(pinned, ForgePublishGrant{TeamID: "t1", ConnectionID: "conn1", Repo: "o/r"}); err != nil {
			t.Fatal(err)
		}
		card := plantCard(t, env, "")
		if _, err := env.srv.launchTicketNow(context.Background(), "t1", env.srv.runs, claimDrift{env.board}, &card); err == nil {
			t.Fatal("a launch whose claim failed reported success")
		}
		if _, live := env.srv.forgePublishTokens.lookup(pinned); !live {
			t.Fatal("a launch that started nothing revoked the token its repo policy pins")
		}
	})
}

// lateClaimBoard answers the first Get unclaimed, then every later one with a
// live claim: a card another launcher took while this call was away asking
// the forge.
type lateClaimBoard struct {
	native.BoardStore
	id    string
	first bool
}

func (b *lateClaimBoard) Get(id string) (*native.Issue, error) {
	iss, err := b.BoardStore.Get(id)
	if err != nil || id != b.id {
		return iss, err
	}
	if !b.first {
		b.first = true
		return iss, nil
	}
	clone := *iss
	clone.Claim = "board-dispatcher-replica-2"
	clone.ClaimLeaseUntil = time.Now().Add(10 * time.Minute)
	return &clone, nil
}

// Composing a launch asks the forge — the fork guard's pull-request read, the
// connection lookup — and mints during those round trips. The board
// dispatcher claims a card WITHOUT moving its state, so the CAS cannot see
// it: the claim is read again against what is true after the composition, and
// the grant this call minted is revoked rather than left orphaned.
func TestPipelineLaunchNow_RefusesAClaimTakenDuringTheComposition(t *testing.T) {
	env := newPipelineTierEnv(t)
	wireForgePublishForT1(t, env.srv)
	card := plantCard(t, env, "")
	board := &lateClaimBoard{BoardStore: env.board, id: card.ID}
	_, err := env.srv.launchTicketNow(context.Background(), "t1", env.srv.runs, board, &card)
	if err == nil {
		t.Fatal("a card claimed while this launch was composing was launched anyway")
	}
	if !strings.Contains(err.Error(), "claimed by") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
	if n := grantCount(t, env.srv); n != 0 {
		t.Errorf("%d grant(s) left in the registry by a launch that refused", n)
	}
}
