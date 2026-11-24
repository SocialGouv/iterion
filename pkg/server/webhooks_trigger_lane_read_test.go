package server

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/boardmongo"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// The bot lanes and the cross-store reads of the one lane reader
// (webhooks_trigger_lane.go, ADR-172 §5). The handlers cannot route by
// subscription yet, so these witnesses drive readTriggerLane and the legacy
// writer (cardTriggerLaneDecide) directly — the prod shape once the router
// exists, with the spine families a flipped integration carries.

// laneNow is a stable ReceivedAt for the seeded rows.
var laneNow = time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)

// ghBase is the legacy base string of trigger on issue 42 (the format
// webhooks_github.go hashes), pre-hash.
func ghBase(trigger string) string {
	return "gh|issue|t1|ghw|acme/widgets|42|" + trigger
}

// laneCfg is the config the lane reads run under.
func laneCfg() webhooks.Config {
	return webhooks.Config{TenantID: "t1", ID: "ghw", Provider: webhooks.ProviderGitHub}
}

// laneSeedRow inserts one Delivery claim row of the shared fixture subject.
func laneSeedRow(t *testing.T, s *Server, row webhooks.Delivery) {
	t.Helper()
	row.TenantID = "t1"
	row.WebhookID = "ghw"
	row.Provider = webhooks.ProviderGitHub
	row.SubjectID = "issue:42"
	row.ProjectPath = "acme/widgets"
	if err := s.webhookDeliveries.Insert(context.Background(), row); err != nil {
		t.Fatalf("seed the delivery row: %v", err)
	}
}

// botLane is a bot lane of the shared subject.
func botLane(bot, trigger string) triggerLane {
	return triggerLane{Project: "acme/widgets", Subject: "issue:42", Trigger: trigger, Bot: bot}
}

// readBotLane runs the one reader for a bot lane, defaulting the read's lane
// and base to the lane's own.
func readBotLane(t *testing.T, s *Server, cfg webhooks.Config, ln triggerLane, ev laneEvent, rd laneRead) laneVerdict {
	t.Helper()
	if rd.Lane == nil {
		l := ln
		rd.Lane = &l
	}
	if len(rd.Bases) == 0 {
		rd.Bases = []string{botLaneBase(ghBase(ln.Trigger), ln.Bot)}
	}
	v, err := s.readTriggerLane(context.Background(), cfg, rd, ev)
	if err != nil {
		t.Fatalf("readTriggerLane: %v", err)
	}
	return v
}

// wantLaneAbsorbed asserts the verdict is absorbed by the given row.
func wantLaneAbsorbed(t *testing.T, v laneVerdict, what, rowID string) {
	t.Helper()
	if v.answered == nil || !v.absorb || v.answered.ID != rowID {
		t.Fatalf("%s: answered=%+v absorb=%v, want absorbed by row %s", what, v.answered, v.absorb, rowID)
	}
}

// wantLaneDuplicate asserts the verdict answers (a duplicate) by the given
// row, without absorbing.
func wantLaneDuplicate(t *testing.T, v laneVerdict, what, rowID string) {
	t.Helper()
	if v.answered == nil || v.absorb || v.answered.ID != rowID {
		t.Fatalf("%s: answered=%+v absorb=%v, want a duplicate of row %s", what, v.answered, v.absorb, rowID)
	}
}

// wantLaneFree asserts nothing answers and the claim key is the chain's
// first generation.
func wantLaneFree(t *testing.T, v laneVerdict, what, base string) {
	t.Helper()
	if v.answered != nil || v.card != nil {
		t.Fatalf("%s: answered=%+v card=%+v, want a free lane", what, v.answered, v.card)
	}
	if want := laneGenerationKey(base, 0); v.nextKey != want {
		t.Fatalf("%s: nextKey=%q, want %q", what, v.nextKey, want)
	}
}

// seedLaneCard creates one board card.
func seedLaneCard(t *testing.T, bs native.BoardStore, c native.Issue) *native.Issue {
	t.Helper()
	if c.Title == "" {
		c.Title = "card"
	}
	got, err := bs.Create(c)
	if err != nil {
		t.Fatalf("seed the card: %v", err)
	}
	return got
}

// laneCardLabels is the label set of a lane card of the subject: the subject
// label, the lane label, and an optional delivery marker key.
func laneCardLabels(ln triggerLane, eventKey string) []string {
	labels := []string{"cmd:issue:42", ln.boardLabel()}
	if eventKey != "" {
		labels = append(labels, boardDeliveryLabel(eventKey))
	}
	return labels
}

// TestTriggerLane_BotLanesDoNotAbsorbEachOther: what a lane owns derives
// from (bot, trigger) alone — A's serving claim blocks A, never B.
//
// Mutations: drop Bot from boardLabel → the board half reds (B's lane reads
// A's card as its own); drop Bot from the base (botLaneBase ignoring the
// bot) → the direct half reds (B's chain holds A's claim).
func TestTriggerLane_BotLanesDoNotAbsorbEachOther(t *testing.T) {
	s := newWebhookTestServer(t)
	s.webhookRunIsLive = func(_ context.Context, id string) bool { return id == "run-a" }
	cfg := laneCfg()
	laneA, laneB := botLane("nestor", "implement"), botLane("saboteur", "implement")
	baseA := botLaneBase(ghBase("implement"), "nestor")
	baseB := botLaneBase(ghBase("implement"), "saboteur")

	// A's serving claim, under its own bot-scoped base.
	laneSeedRow(t, s, webhooks.Delivery{
		ID: "row-a", IdempotencyKey: laneGenerationKey(baseA, 0), BotID: "nestor",
		PayloadHash: "hash-a", ForgeDeliveryID: "d-a", Status: webhooks.StatusAccepted,
		ReceivedAt: laneNow,
	})

	t.Run("direct", func(t *testing.T) {
		// A's own fresh label is absorbed by A's serving claim.
		vA := readBotLane(t, s, cfg, laneA, laneEvent{PayloadHash: "hash-a2", DeliveryID: "d-a2"}, laneRead{Bot: "nestor"})
		wantLaneAbsorbed(t, vA, "a fresh label on A's lane", "row-a")

		// A serving does not absorb B: B's lane reads free, under its own
		// base and its own next key.
		vB := readBotLane(t, s, cfg, laneB, laneEvent{PayloadHash: "hash-b", DeliveryID: "d-b"}, laneRead{Bot: "saboteur"})
		wantLaneFree(t, vB, "B's lane while A serves", baseB)
		if vB.nextCardID != "" {
			t.Fatalf("B's free direct read carries nextCardID %q, want none", vB.nextCardID)
		}
	})

	t.Run("board", func(t *testing.T) {
		s, board, _ := newBoardLaneServer(t)
		s.webhookRunIsLive = func(_ context.Context, id string) bool { return id == "run-a" }
		cardA := seedLaneCard(t, board, native.Issue{
			State: native.StateReady, Bot: "nestor",
			Labels:  laneCardLabels(laneA, "delivery:d-a"),
			BotArgs: map[string]string{boardProjectPathKey: "acme/widgets"},
		})
		// A's redelivery is a duplicate of its card; A's fresh label is
		// absorbed by it.
		vA1 := readBotLane(t, s, cfg, laneA, laneEvent{PayloadHash: "hash-a", DeliveryID: "d-a"}, laneRead{Bot: "nestor"})
		if vA1.card == nil || vA1.card.ID != cardA.ID || vA1.absorb {
			t.Fatalf("A's redelivery: card=%+v absorb=%v, want a duplicate of %s", vA1.card, vA1.absorb, cardA.ID)
		}
		vA2 := readBotLane(t, s, cfg, laneA, laneEvent{PayloadHash: "hash-a2", DeliveryID: "d-a2"}, laneRead{Bot: "nestor"})
		if vA2.card == nil || vA2.card.ID != cardA.ID || !vA2.absorb {
			t.Fatalf("A's fresh label: card=%+v absorb=%v, want absorbed by %s", vA2.card, vA2.absorb, cardA.ID)
		}
		// A serving does not absorb B: B's lane holds no cards of its own
		// and reads free, with a next id derived without A's card.
		vB := readBotLane(t, s, cfg, laneB, laneEvent{PayloadHash: "hash-b", DeliveryID: "d-b"}, laneRead{Bot: "saboteur"})
		if vB.card != nil {
			t.Fatalf("B's lane while A's card serves: answered by %s, want free", vB.card.ID)
		}
		if want := laneCardID("t1", laneB, nil); vB.nextCardID != want {
			t.Fatalf("B's next card id = %q, want the empty-set id %q — A's card must not sit in B's held set", vB.nextCardID, want)
		}
	})
}

// TestTriggerLane_PreLaneAndLegacyCardsOwnedOnlyByTheirBot: a bot lane owns
// the legacy card of its own trigger only, only when the card's bot is its
// own; a pre-lane card of another bot blocks nothing.
//
// Mutations: the bot lane ignores c.Bot → the other-bot subtests red (a
// serving card of saboteur absorbs nestor's lane); own every legacy card of
// the bot regardless of trigger → the other-trigger subtest reds.
func TestTriggerLane_PreLaneAndLegacyCardsOwnedOnlyByTheirBot(t *testing.T) {
	cfg := laneCfg()
	ln := botLane("nestor", "implement")
	fresh := laneEvent{PayloadHash: "hash-1", DeliveryID: "d-1"}

	t.Run("a legacy card of the same trigger and bot is owned", func(t *testing.T) {
		s, board, _ := newBoardLaneServer(t)
		c1 := seedLaneCard(t, board, native.Issue{
			State: native.StateReady, Bot: "nestor", Labels: laneCardLabels(ln, ""),
		})
		v := readBotLane(t, s, cfg, ln, fresh, laneRead{Bot: "nestor"})
		if v.card == nil || v.card.ID != c1.ID || !v.absorb {
			t.Fatalf("the legacy card of the same trigger: card=%+v absorb=%v, want absorbed by %s", v.card, v.absorb, c1.ID)
		}
	})

	t.Run("a legacy card of another trigger is not owned", func(t *testing.T) {
		s, board, _ := newBoardLaneServer(t)
		seedLaneCard(t, board, native.Issue{
			State: native.StateReady, Bot: "nestor", Labels: laneCardLabels(botLane("nestor", "bug"), ""),
		})
		v := readBotLane(t, s, cfg, ln, fresh, laneRead{Bot: "nestor"})
		if v.card != nil {
			t.Fatalf("a serving card under another trigger absorbed the lane: %s", v.card.ID)
		}
		if want := laneCardID("t1", ln, nil); v.nextCardID != want {
			t.Fatalf("nextCardID = %q, want the empty-set id %q — the other trigger's card must not sit in the held set", v.nextCardID, want)
		}
	})

	t.Run("a legacy card of another bot is not owned", func(t *testing.T) {
		s, board, _ := newBoardLaneServer(t)
		seedLaneCard(t, board, native.Issue{
			State: native.StateReady, Bot: "saboteur", Labels: laneCardLabels(botLane("saboteur", "implement"), ""),
		})
		v := readBotLane(t, s, cfg, ln, fresh, laneRead{Bot: "nestor"})
		if v.card != nil {
			t.Fatalf("a serving legacy card of another bot absorbed the lane: %s", v.card.ID)
		}
		if want := laneCardID("t1", ln, nil); v.nextCardID != want {
			t.Fatalf("nextCardID = %q, want the empty-set id %q — the other bot's card must not sit in the held set", v.nextCardID, want)
		}
	})

	t.Run("a pre-lane card of another bot is not owned", func(t *testing.T) {
		s, board, _ := newBoardLaneServer(t)
		seedLaneCard(t, board, native.Issue{
			State: native.StateReady, Bot: "saboteur",
			Labels:  []string{"cmd:issue:42"},
			BotArgs: map[string]string{boardProjectPathKey: "acme/widgets"},
		})
		v := readBotLane(t, s, cfg, ln, fresh, laneRead{Bot: "nestor"})
		if v.card != nil {
			t.Fatalf("a serving pre-lane card of another bot absorbed the lane: %s", v.card.ID)
		}
		if want := laneCardID("t1", ln, nil); v.nextCardID != want {
			t.Fatalf("nextCardID = %q, want the empty-set id %q", v.nextCardID, want)
		}
	})

	t.Run("the bot's own pre-lane and bot-less cards are owned", func(t *testing.T) {
		s, board, _ := newBoardLaneServer(t)
		own := seedLaneCard(t, board, native.Issue{
			State: native.StateReady, Bot: "nestor", Labels: []string{"cmd:issue:42"},
		})
		v := readBotLane(t, s, cfg, ln, fresh, laneRead{Bot: "nestor"})
		if v.card == nil || v.card.ID != own.ID || !v.absorb {
			t.Fatalf("the bot's own pre-lane card: card=%+v absorb=%v, want absorbed by %s", v.card, v.absorb, own.ID)
		}
		// The pre-lane card ends; the bot-less era card then serves the lane.
		finishCard(t, board, own.ID)
		less := seedLaneCard(t, board, native.Issue{
			State: native.StateReady, Labels: []string{"cmd:issue:42"},
			BotArgs: map[string]string{boardProjectPathKey: "acme/widgets"},
		})
		v2 := readBotLane(t, s, cfg, ln, laneEvent{PayloadHash: "hash-2", DeliveryID: "d-2"}, laneRead{Bot: "nestor"})
		if v2.card == nil || v2.card.ID != less.ID || !v2.absorb {
			t.Fatalf("the bot-less era card: card=%+v absorb=%v, want absorbed by %s", v2.card, v2.absorb, less.ID)
		}
	})

	t.Run("the terminal owned set fixes the next card id", func(t *testing.T) {
		s, board, _ := newBoardLaneServer(t)
		legacy := seedLaneCard(t, board, native.Issue{
			State: native.StateDone, Bot: "nestor", Labels: laneCardLabels(ln, ""),
		})
		preLane := seedLaneCard(t, board, native.Issue{
			State: native.StateDone, Bot: "nestor", Labels: []string{"cmd:issue:42"},
		})
		botless := seedLaneCard(t, board, native.Issue{
			State: native.StateDone, Labels: []string{"cmd:issue:42"},
			BotArgs: map[string]string{boardProjectPathKey: "acme/widgets"},
		})
		seedLaneCard(t, board, native.Issue{
			State: native.StateDone, Bot: "nestor", Labels: laneCardLabels(botLane("nestor", "bug"), ""),
		})
		seedLaneCard(t, board, native.Issue{
			State: native.StateDone, Bot: "saboteur", Labels: laneCardLabels(botLane("saboteur", "implement"), ""),
		})
		seedLaneCard(t, board, native.Issue{
			State: native.StateDone, Bot: "saboteur", Labels: []string{"cmd:issue:42"},
		})
		owned := []string{legacy.ID, preLane.ID, botless.ID}
		slices.Sort(owned)
		v := readBotLane(t, s, cfg, ln, fresh, laneRead{Bot: "nestor"})
		if want := laneCardID("t1", ln, owned); v.nextCardID != want {
			t.Fatalf("nextCardID = %q, want the id over exactly the owned ids %v", v.nextCardID, owned)
		}
	})
}

// TestTriggerLane_OwnershipIndependentOfPayloadOrder (GitLab shape): a
// terminal legacy card of nestor under trigger "x" (the legacy trigger the
// payload order of the first event picks); two fresh events adding [x,
// nestor] and [nestor] read on two replicas that share the stores. The
// spine trigger is the smallest claimed label — the same lane for both — so
// both hold the same set, compute the same next card id, and one card lands.
//
// Mutation: own the event's legacy-trigger card (the spine half's bot lanes
// extending a bot lane's owned set) → the first event's read owns the "x"
// card, the second's does not, the ids part and two cards land.
func TestTriggerLane_OwnershipIndependentOfPayloadOrder(t *testing.T) {
	xLane := botLane("nestor", "x")
	spineLane := botLane("nestor", "nestor")

	for name, order := range map[string][2]string{
		"in order": {"e1", "e2"},
		"reversed": {"e2", "e1"},
	} {
		t.Run(name, func(t *testing.T) {
			// A fresh shared pair of stores per order: the two replicas of
			// this scenario share the board and the Delivery store.
			board, err := native.NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			deliveries := webhooks.NewMemoryDeliveryStore()
			xCard := seedLaneCard(t, board, native.Issue{
				State: native.StateDone, Bot: "nestor",
				Labels:  laneCardLabels(xLane, "delivery:d-0"),
				BotArgs: map[string]string{boardProjectPathKey: "acme/widgets"},
			})
			s1, s2 := newWebhookTestServer(t), newWebhookTestServer(t)
			s1.webhookDeliveries = deliveries
			s2.webhookDeliveries = deliveries
			b := &barrierBoard{BoardStore: board, armed: 2}
			b.arrived.Add(2)
			for _, s := range []*Server{s1, s2} {
				s.cfg.CloudBoardFor = func(string) native.BoardStore { return b }
				s.cfg.CloudBoardCoordinator = &boardmongo.Coordinator{}
			}
			events := map[string]struct {
				triggers []string
				ev       laneEvent
			}{
				// The labels each event carries, payload order: the first
				// event adds x then nestor, the second nestor only. The
				// spine lane is the smallest claimed label — the same for
				// both — and the labels ride as the read's triggers exactly
				// as a legacy spine half would carry them. The lane's
				// ownership must not follow them.
				"e1": {[]string{"x", "nestor"}, laneEvent{PayloadHash: "hash-1", DeliveryID: "d-1"}},
				"e2": {[]string{"nestor"}, laneEvent{PayloadHash: "hash-2", DeliveryID: "d-2"}},
			}
			first, second := events[order[0]], events[order[1]]

			var (
				mu       sync.Mutex
				verdicts []laneVerdict
				created  int
				wg       sync.WaitGroup
			)
			pair := [2]struct {
				s *Server
				e struct {
					triggers []string
					ev       laneEvent
				}
			}{{s1, first}, {s2, second}}
			for i, p := range pair {
				wg.Add(1)
				go func(i int, s *Server, e struct {
					triggers []string
					ev       laneEvent
				}) {
					defer wg.Done()
					rd := laneRead{
						Lane:        func() *triggerLane { l := spineLane; return &l }(),
						Bases:       []string{botLaneBase(ghBase("nestor"), "nestor")},
						Bot:         "nestor",
						BotTriggers: e.triggers,
						Board:       true,
					}
					v, err := s.readTriggerLane(context.Background(), laneCfg(), rd, e.ev)
					if err != nil {
						t.Errorf("readTriggerLane: %v", err)
						return
					}
					mu.Lock()
					verdicts = append(verdicts, v)
					mu.Unlock()
					// The claim phase: card under the read's next id.
					card := native.Issue{
						Title: "card", State: native.StateReady, Bot: "nestor",
						Labels: laneCardLabels(spineLane, e.ev.key()),
					}
					card.ID = v.nextCardID
					if _, err := b.Create(card); err == nil {
						mu.Lock()
						created++
						mu.Unlock()
					}
				}(i, p.s, p.e)
			}
			wg.Wait()

			if len(verdicts) != 2 {
				t.Fatalf("verdicts = %d, want 2", len(verdicts))
			}
			want := laneCardID("t1", spineLane, nil)
			for _, v := range verdicts {
				if v.card != nil {
					t.Fatalf("a fresh event absorbed by the done %q card: %+v", xCard.ID, v.card)
				}
				if v.nextCardID != want {
					t.Fatalf("nextCardID = %q, want the shared id %q — the held set followed the payload order", v.nextCardID, want)
				}
			}
			if created != 1 {
				t.Fatalf("cards created = %d, want 1 — the two reads must collide on one deterministic id", created)
			}
			if cards := laneCards(t, b); len(cards) != 2 {
				t.Fatalf("cards = %d, want 2 (the done x card and ONE new one)", len(cards))
			}
		})
	}
}

// TestTriggerLane_CrossStore_LegacyCardBlocksSpineDirect (ADR-172 §5, the
// flip): a legacy card serving beside a spine-DIRECT target. The card the
// legacy era carded answers the spine target's redelivery and absorbs its
// fresh label, whether or not the target claims a card.
//
// Mutation: skip the board half of readTriggerLane for direct targets →
// both reds (the spine read claims free).
func TestTriggerLane_CrossStore_LegacyCardBlocksSpineDirect(t *testing.T) {
	s, board, _ := newBoardLaneServer(t)
	cfg := laneCfg()
	since := laneNow
	cfg.SpineSince = &since

	ln := botLane("nestor", "nestor")
	base := botLaneBase(ghBase("nestor"), "nestor")
	legacyBase := ghBase("nestor")
	card := seedLaneCard(t, board, native.Issue{
		State: native.StateReady, Bot: "nestor",
		// A legacy-era card: the bot-less lane label, the event's marker.
		Labels:  laneCardLabels(triggerLane{Project: ln.Project, Subject: ln.Subject, Trigger: ln.Trigger}, "delivery:d-e"),
		BotArgs: map[string]string{boardProjectPathKey: "acme/widgets"},
	})

	spineRead := laneRead{
		Lane:      func() *triggerLane { l := ln; return &l }(),
		Bases:     []string{base},
		Legacy:    []string{legacyBase},
		EventOnly: []string{legacyBase},
		Bot:       "nestor",
		Board:     false, // a DIRECT target
	}
	// The redelivery of the event the card was carded for: a duplicate.
	v := readBotLane(t, s, cfg, ln, laneEvent{PayloadHash: "hash-e", DeliveryID: "d-e"}, spineRead)
	if v.card == nil || v.card.ID != card.ID || v.absorb {
		t.Fatalf("the redelivery: card=%+v absorb=%v, want a duplicate of the legacy card %s", v.card, v.absorb, card.ID)
	}
	// A fresh nestor label while the card serves: absorbed.
	v2 := readBotLane(t, s, cfg, ln, laneEvent{PayloadHash: "hash-f", DeliveryID: "d-f"}, spineRead)
	if v2.card == nil || v2.card.ID != card.ID || !v2.absorb {
		t.Fatalf("the fresh label: card=%+v absorb=%v, want absorbed by the legacy card %s", v2.card, v2.absorb, card.ID)
	}
	// Nothing claimed: the reads carried no claim key and wrote nothing.
	if v.nextKey != "" || v2.nextKey != "" {
		t.Fatalf("answered reads carry nextKey %q / %q, want none", v.nextKey, v2.nextKey)
	}
	if n := len(laneCards(t, board)); n != 1 {
		t.Fatalf("cards = %d, want 1 — a spine read writes nothing", n)
	}
}

// TestTriggerLane_CrossStore_SpineDirectBlocksLegacyCard (ADR-172 §5, the
// flip back): a spine-direct claim still serving beside the legacy board
// writer. The legacy carded read (cardTriggerLaneDecide, the branch cloud's
// legacy label route takes) answers the redelivery as a duplicate and
// absorbs the fresh label, and cards nothing.
//
// Mutation: cardTriggerLane keeps its own absorbed-row + board pair (the
// read drops the spine half) → both reds (the legacy writer cards).
func TestTriggerLane_CrossStore_SpineDirectBlocksLegacyCard(t *testing.T) {
	s, board, _ := newBoardLaneServer(t)
	cfg := laneCfg()
	since := laneNow
	cfg.SpineSince = &since
	s.webhookRunIsLive = func(_ context.Context, id string) bool { return id == "run-n" }

	legacyLane := triggerLane{Project: "acme/widgets", Subject: "issue:42", Trigger: "nestor"}
	base := ghBase("nestor")
	botBase := botLaneBase(base, "nestor")
	laneSeedRow(t, s, webhooks.Delivery{
		ID: "row-n", IdempotencyKey: laneGenerationKey(botBase, 0), BotID: "nestor",
		PayloadHash: "hash-e", ForgeDeliveryID: "d-e", RunID: "run-n",
		Status: webhooks.StatusAccepted, ReceivedAt: laneNow,
	})

	decide := func(t *testing.T, ev laneEvent) laneCardResult {
		t.Helper()
		res, err := s.cardTriggerLaneDecide(context.Background(),
			cfg, webhookEventMeta{Kind: "issues", ProjectPath: "acme/widgets", SubjectID: "issue:42", DeliveryID: ev.DeliveryID},
			base, webhooks.CommandRoute{BotID: "nestor", Mode: "board", ArgsVar: "feature_prompt"},
			map[string]string{"feature_prompt": "do the thing"},
			"https://github.com/acme/widgets.git", "main", ev.PayloadHash, "1.2.3.4",
			launchProvenance{}, legacyLane, func() {},
			laneSpineHalf{Bot: "nestor", Bases: []string{botBase}, Triggers: []string{"nestor"}},
			boardCardExtras{})
		if err != nil {
			t.Fatalf("cardTriggerLaneDecide: %v", err)
		}
		return res
	}
	res := decide(t, laneEvent{PayloadHash: "hash-e", DeliveryID: "d-e"})
	if res.row == nil || res.Status != webhooks.StatusDuplicate || res.cardID != "" {
		t.Fatalf("the redelivery: %+v, want a duplicate answered by the spine claim row", res)
	}
	res2 := decide(t, laneEvent{PayloadHash: "hash-f", DeliveryID: "d-f"})
	if res2.row == nil || !res2.absorb || res2.cardID != "" {
		t.Fatalf("the fresh label: %+v, want absorbed by the spine claim row", res2)
	}
	if n := len(laneCards(t, board)); n != 0 {
		t.Fatalf("cards = %d, want 0 — the spine claim must card nothing", n)
	}
}

// TestTriggerLane_CrossStore_LegacyAbsorbedRowBlocksSpineBoard (ADR-172 §5):
// an event a legacy card absorbed before the flip, its absorption recorded
// under the legacy base; the card ends; the flip; a spine-BOARD redelivery
// of the event answers duplicate off the recorded absorption and cards
// nothing.
//
// Mutation: the absorbed-row read drops the side families → red (the spine
// read claims free and would card).
func TestTriggerLane_CrossStore_LegacyAbsorbedRowBlocksSpineBoard(t *testing.T) {
	s, board, _ := newBoardLaneServer(t)
	cfg := laneCfg()
	since := laneNow
	cfg.SpineSince = &since

	ln := botLane("nestor", "nestor")
	base := botLaneBase(ghBase("nestor"), "nestor")
	legacyBase := ghBase("nestor")
	// The legacy era: the event's card ended, the event itself was absorbed
	// and recorded under the legacy base.
	seedLaneCard(t, board, native.Issue{
		State: native.StateDone, Bot: "nestor",
		Labels:  laneCardLabels(triggerLane{Project: ln.Project, Subject: ln.Subject, Trigger: ln.Trigger}, "delivery:d-0"),
		BotArgs: map[string]string{boardProjectPathKey: "acme/widgets"},
	})
	laneSeedRow(t, s, webhooks.Delivery{
		ID: "row-absorbed", IdempotencyKey: laneAbsorbedKey(legacyBase, "delivery:d-e"), BotID: "nestor",
		PayloadHash: "hash-e", ForgeDeliveryID: "d-e", Status: webhooks.StatusDuplicate,
		ReceivedAt: laneNow,
	})

	spineRead := laneRead{
		Lane:      func() *triggerLane { l := ln; return &l }(),
		Bases:     []string{base},
		Legacy:    []string{legacyBase},
		EventOnly: []string{legacyBase},
		Bot:       "nestor",
		Board:     true,
	}
	v := readBotLane(t, s, cfg, ln, laneEvent{PayloadHash: "hash-e", DeliveryID: "d-e"}, spineRead)
	wantLaneDuplicate(t, v, "the spine-board redelivery", "row-absorbed")
	if v.nextCardID != "" {
		t.Fatalf("the redelivery carries nextCardID %q, want none — no new card", v.nextCardID)
	}
	if n := len(laneCards(t, board)); n != 1 {
		t.Fatalf("cards = %d, want 1 — the redelivery cards nothing", n)
	}
}

// TestTriggerLane_LegacyLaneReadsSpineClaimsOnceFlipped: on an integration
// that has ever routed by subscription, BOTH legacy branches read the
// elected bot's spine half — the direct tail's claim walk and the carded
// writer. A spine claim still serving absorbs the legacy lane's fresh
// label; a claim made for the redelivered event answers it.
//
// Mutation: the legacy read drops the Legacy bases (readTriggerLane
// ignoring the spine half) → all three subtests red: the legacy lane claims
// free beside a live spine claim.
func TestTriggerLane_LegacyLaneReadsSpineClaimsOnceFlipped(t *testing.T) {
	s := newWebhookTestServer(t)
	s.webhookRunIsLive = func(_ context.Context, id string) bool { return id == "run-n" }
	cfg := laneCfg()
	since := laneNow
	cfg.SpineSince = &since

	legacyLane := triggerLane{Project: "acme/widgets", Subject: "issue:42", Trigger: "nestor"}
	base := ghBase("nestor")
	botBase := botLaneBase(base, "nestor")
	laneSeedRow(t, s, webhooks.Delivery{
		ID: "row-n", IdempotencyKey: laneGenerationKey(botBase, 0), BotID: "nestor",
		PayloadHash: "hash-e", ForgeDeliveryID: "d-e", RunID: "run-n",
		Status: webhooks.StatusAccepted, ReceivedAt: laneNow,
	})
	legacyRead := laneRead{
		Lane:   func() *triggerLane { l := legacyLane; return &l }(),
		Bases:  []string{base},
		Legacy: []string{botBase},
		Bot:    "nestor",
	}

	t.Run("the direct tail's claim walk", func(t *testing.T) {
		// The redelivery of the event the spine claimed: a duplicate of it.
		v := readBotLane(t, s, cfg, legacyLane, laneEvent{PayloadHash: "hash-e", DeliveryID: "d-e"}, legacyRead)
		wantLaneDuplicate(t, v, "the redelivery", "row-n")
		// A fresh label while the spine claim serves: absorbed by it.
		v2 := readBotLane(t, s, cfg, legacyLane, laneEvent{PayloadHash: "hash-f", DeliveryID: "d-f"}, legacyRead)
		wantLaneAbsorbed(t, v2, "the fresh label", "row-n")
	})

	t.Run("the carded writer", func(t *testing.T) {
		s, board, _ := newBoardLaneServer(t)
		cfg := laneCfg()
		cfg.SpineSince = &since
		s.webhookRunIsLive = func(_ context.Context, id string) bool { return id == "run-n" }
		laneSeedRow(t, s, webhooks.Delivery{
			ID: "row-n", IdempotencyKey: laneGenerationKey(botBase, 0), BotID: "nestor",
			PayloadHash: "hash-e", ForgeDeliveryID: "d-e", RunID: "run-n",
			Status: webhooks.StatusAccepted, ReceivedAt: laneNow,
		})
		decide := func(t *testing.T, ev laneEvent) laneCardResult {
			t.Helper()
			res, err := s.cardTriggerLaneDecide(context.Background(),
				cfg, webhookEventMeta{Kind: "issues", ProjectPath: "acme/widgets", SubjectID: "issue:42", DeliveryID: ev.DeliveryID},
				base, webhooks.CommandRoute{BotID: "nestor", Mode: "board", ArgsVar: "feature_prompt"},
				map[string]string{"feature_prompt": "do the thing"},
				"https://github.com/acme/widgets.git", "main", ev.PayloadHash, "1.2.3.4",
				launchProvenance{}, legacyLane, func() {},
				laneSpineHalf{Bot: "nestor", Bases: []string{botBase}, Triggers: []string{"nestor"}},
				boardCardExtras{})
			if err != nil {
				t.Fatalf("cardTriggerLaneDecide: %v", err)
			}
			return res
		}
		res := decide(t, laneEvent{PayloadHash: "hash-e", DeliveryID: "d-e"})
		if res.row == nil || res.Status != webhooks.StatusDuplicate || res.cardID != "" {
			t.Fatalf("the redelivery: %+v, want a duplicate answered by the spine claim row", res)
		}
		res2 := decide(t, laneEvent{PayloadHash: "hash-f", DeliveryID: "d-f"})
		if res2.row == nil || !res2.absorb || res2.cardID != "" {
			t.Fatalf("the fresh label: %+v, want absorbed by the spine claim row", res2)
		}
		if n := len(laneCards(t, board)); n != 0 {
			t.Fatalf("cards = %d, want 0 — the spine claim must card nothing", n)
		}
	})
}

// TestTriggerLane_ProjectionsMatchTheReader: the kept-signature projections
// surface the reader's verdicts without drifting from it.
func TestTriggerLane_ProjectionsMatchTheReader(t *testing.T) {
	s := newWebhookTestServer(t)
	s.webhookRunIsLive = func(_ context.Context, id string) bool { return id == "run-a" }
	cfg := laneCfg()
	base := ghBase("implement")
	laneSeedRow(t, s, webhooks.Delivery{
		ID: "row-a", IdempotencyKey: laneGenerationKey(base, 0), BotID: "nestor",
		PayloadHash: "hash-a", ForgeDeliveryID: "d-a", Status: webhooks.StatusAccepted,
		ReceivedAt: laneNow,
	})
	ev := laneEvent{PayloadHash: "hash-a2", DeliveryID: "d-a2"}

	claim, err := s.claimTriggerLane(context.Background(), cfg, base, ev)
	if err != nil {
		t.Fatal(err)
	}
	direct, err := s.readTriggerLane(context.Background(), cfg, laneRead{Bases: []string{base}}, ev)
	if err != nil {
		t.Fatal(err)
	}
	if claim.answered == nil || direct.answered == nil || claim.answered.ID != direct.answered.ID || claim.absorb != direct.absorb {
		t.Fatalf("claimTriggerLane = %+v, readTriggerLane = %+v; the projection must agree with the reader", claim, direct)
	}

	// boardLaneVerdict is the narrow board half: a legacy lane with no bot,
	// no Delivery chains.
	s2, board, _ := newBoardLaneServer(t)
	ln := triggerLane{Project: "acme/widgets", Subject: "issue:42", Trigger: "nestor"}
	card := seedLaneCard(t, board, native.Issue{
		State: native.StateReady, Bot: "nestor", Labels: laneCardLabels(ln, "delivery:d-1"),
	})
	answer, err := boardLaneVerdict(board, "t1", ln, laneEvent{PayloadHash: "hash-1", DeliveryID: "d-1"})
	if err != nil {
		t.Fatal(err)
	}
	botLane := ln
	reader, err := s2.readTriggerLane(context.Background(), laneCfg(), laneRead{Lane: &botLane, Board: true}, laneEvent{PayloadHash: "hash-1", DeliveryID: "d-1"})
	if err != nil {
		t.Fatal(err)
	}
	if answer.card == nil || reader.card == nil || answer.card.ID != card.ID || reader.card.ID != answer.card.ID {
		t.Fatalf("boardLaneVerdict = %+v, the reader = %+v; the projection must agree with the reader", answer, reader)
	}
}
