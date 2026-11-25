package server

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
)

// RT-F4: a config never flipped to the spine, board route, board seam wired
// without a coordinator: the label lane's tracking cards are the legacy ones,
// byte for byte — one per subject (the per-comment label dedupes them), no
// lane label. Only the spine passes a lane to ensureBoardCard.
//
// Mutation: the legacy no-coordinator branch of dispatchInvocation passes its
// lane to ensureBoardCard → red on "tracking cards = 2".
func TestLegacyLabelLane_TrackingCardsCarryNoLane(t *testing.T) {
	for _, f := range laneForges() {
		t.Run(f.name, func(t *testing.T) {
			s, runs, _ := newDirectLaneServer(t)
			board, err := native.NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			s.cfg.CloudBoardFor = func(string) native.BoardStore { return board }
			deliver := f.bind(t, s, "")
			wantLaunched(t, deliver(f.labeled("2026-11-18T10:00:00Z", "implement")), "the first label")
			runs.end("run-1")
			wantLaunched(t, deliver(f.labeled("2026-11-18T11:00:00Z", "bug")), "a second label on the same issue")
			cards := laneCards(t, board)
			if len(cards) != 1 {
				t.Fatalf("tracking cards = %d, want 1 — the legacy tracking card is per subject", len(cards))
			}
			for _, l := range cards[0].Labels {
				if strings.HasPrefix(l, boardLaneLabelPrefix) {
					t.Fatalf("the tracking card carries the lane label %q (labels %v): a never-flipped config's cards are the legacy ones", l, cards[0].Labels)
				}
			}
		})
	}
}

// RT-F5: a legacy multi-target answer lists its launches in its targets'
// order, the bytes the legacy lanes have always answered; only the spine's
// array is sorted by bot.
//
// Mutation: acceptWebhookTargetsCore sorts the 202 body whatever
// legacyBodies says → red on "launches order".
func TestLegacyMultiTarget_LaunchesKeepTheTargetsOrder(t *testing.T) {
	s, _, _ := newDirectLaneServer(t)
	cfg, _ := ghConfig(t, s)
	meta := webhookEventMeta{Kind: "pull_request", Action: "opened", ProjectPath: "acme/widgets", SubjectID: "pr:7"}
	ctx := ghCtx(cfg)
	r := httptest.NewRequest(http.MethodPost, "/", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	s.acceptWebhookTargets(ctx, w, r, cfg, meta, []forgeLaunchTarget{
		{BotID: "zeta-review", IdemKey: "k-zeta", Vars: map[string]string{}},
		{BotID: "alpha-review", IdemKey: "k-alpha", Vars: map[string]string{}},
	}, "hash-1", "1.2.3.4")
	if w.Code != http.StatusAccepted {
		t.Fatalf("code=%d body=%s, want 202", w.Code, w.Body.String())
	}
	var order []string
	for _, l := range spineLaunches(t, w) {
		order = append(order, l["bot"])
	}
	if !slices.Equal(order, []string{"zeta-review", "alpha-review"}) {
		t.Fatalf("launches order = %v, want the targets' order [zeta-review alpha-review]", order)
	}
}
