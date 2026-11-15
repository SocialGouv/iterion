package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// An evaluation run (publish_mode=off) posted nothing on its pull request, so
// its review is no review the team saw: the hand-off walk skips it, and the
// fixer is handed the published review — whichever of the two is newer.
func TestWebhookHandoff_SkipsAnEvaluationRun(t *testing.T) {
	for _, order := range []string{"evaluation run newer", "published run newer"} {
		t.Run(order, func(t *testing.T) {
			ctx := context.Background()
			s := newBotMetadataTeamServer(t)
			rs, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			s.cfg.Store = rs
			s.runs = newTestRunviewService(t, "", runview.WithStore(rs))
			const pr = "https://forge.example/acme/widgets/-/merge_requests/201"
			seed := func(inputs map[string]any, title string) {
				tctx := store.WithTenant(ctx, "t1")
				run, err := rs.CreateRun(tctx, mustRunID(t), "reviewer-bot", inputs)
				if err != nil {
					t.Fatal(err)
				}
				run.Status = store.RunStatusFinished
				run.BotID = "reviewer-bot"
				run.BotSourceTenant = "t1"
				run.BotSourceTier = store.BotSourceTierTeam
				run.Trust = store.RunTrustDefault
				if err := rs.SaveRun(tctx, run); err != nil {
					t.Fatal(err)
				}
				if err := rs.WriteArtifact(tctx, &store.Artifact{RunID: run.ID, NodeID: "publish_fork", Data: map[string]any{
					"total_findings": 1,
					"findings": []any{map[string]any{
						"severity": "high", "category": "correctness", "title": title,
						"file": "a.go", "line": float64(3),
					}},
				}}); err != nil {
					t.Fatal(err)
				}
				time.Sleep(20 * time.Millisecond) // created-at ordering
			}
			published := map[string]any{"pr_url": pr}
			evaluation := map[string]any{"pr_url": pr, "publish_mode": "off"}
			if order == "evaluation run newer" {
				seed(published, "PUBLISHED finding")
				seed(evaluation, "EVALUATION-ONLY finding")
			} else {
				seed(evaluation, "EVALUATION-ONLY finding")
				seed(published, "PUBLISHED finding")
			}
			got := s.realWebhookHandoff(ctx, webhooks.Config{ID: "wh-1", TenantID: "t1"}, bundle.HandoffKindReview, handoffQuery{PRURL: pr})
			if strings.Contains(got, "EVALUATION-ONLY") {
				t.Errorf("the fixer is handed the evaluation run's unpublished review:\n%s", got)
			}
			if !strings.Contains(got, "PUBLISHED finding") {
				t.Errorf("the fixer is not handed the published review:\n%s", got)
			}
		})
	}
}

// An evaluation launch holds no publish grant: none is minted, one the caller
// or the repo policy carried is withdrawn, so whatever bot version serves the
// run, the publish endpoint has no grant of it to honour. The same launch
// out of evaluation mode is minted one the endpoint honours.
func TestInjectForgePublishVars_EvaluationLaunchHoldsNoGrant(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mode      string
		pinned    bool
		wantGrant bool
	}{
		{name: "evaluation launch", mode: "off"},
		{name: "evaluation launch carrying a grant", mode: "off", pinned: true},
		{name: "default launch", mode: "", wantGrant: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, fake := newForgePublishTestServer(t)
			s.cfg.PublicURL = "https://iterion.example"
			vars := map[string]string{"pr_url": "https://github.com/o/r/pull/42", "publish_mode": tc.mode}
			if tc.pinned {
				pinned, _, err := s.injectForgePublishVars(context.Background(), "team1", "", "review-pr",
					map[string]string{"pr_url": vars["pr_url"]}, nil, store.RunTrustDefault)
				if err != nil || pinned[forgePublishVarToken] == "" {
					t.Fatalf("minting the grant to pin: %v (%v)", err, pinned)
				}
				for _, k := range forgePublishVars() {
					if v, ok := pinned[k]; ok {
						vars[k] = v
					}
				}
			}
			out, minted, err := s.injectForgePublishVars(context.Background(), "team1", "", "review-pr", vars, nil, store.RunTrustDefault)
			if err != nil {
				t.Fatal(err)
			}
			tok := out[forgePublishVarToken]
			if !tc.wantGrant {
				for _, k := range forgePublishVars() {
					if v, ok := out[k]; ok {
						t.Errorf("an evaluation launch carries %s=%q", k, v)
					}
				}
				if minted != "" {
					t.Errorf("an evaluation launch minted a grant")
				}
			}
			w := httptest.NewRecorder()
			s.handleForgePublishReview(w, publishReq(tok, validPublishBody))
			switch {
			case tc.wantGrant && (w.Code != http.StatusOK || fake.calls != 1):
				t.Errorf("default launch: publish answered %d with %d forge call(s), want 200 and one review", w.Code, fake.calls)
			case !tc.wantGrant && (w.Code == http.StatusOK || fake.calls != 0):
				t.Errorf("evaluation launch: publish answered %d with %d forge call(s), want a refusal and no review", w.Code, fake.calls)
			}
		})
	}
}
