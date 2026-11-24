package server

import (
	"context"
	"flag"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// Row 11 (plan table). The legacy issue-label routing is BYTE-IDENTICAL for a
// config that never flipped: the HTTP bodies, the idempotency keys and the
// card labels the two providers' lanes answered before ADR-172 are pinned
// here, so the spine cut cannot disturb an integration that never left
// legacy. The goldens are green by construction — they were recorded on the
// legacy path — and only their mutation proves them: routing the issues lane
// by spine BY DEFAULT (forgeRoutingFor answering spine for a lane the config
// leaves unset) must red this test, because a never-flipped config would
// then route differently.
//
// The recorded contract carries no wall clock: the bodies are status maps,
// the keys are content checksums over fixed inputs (tenant t1, webhook,
// acme/widgets, issue 42, the label), and the card ids derive from the lane
// and its held set — so no gateNow pinning is needed; the only per-run
// randomness (delivery row ids) is masked to first-appearance tokens.
//
// Mutations (router lot): default routing spine → red (the never-flipped
// config routes by subscription and the answers move).

var updateSpineGolden = flag.Bool("update-spine-golden", false, "rewrite the legacy issue-label routing golden")

// spineGoldenDir is where the recorded legacy contract lives.
const spineGoldenDir = "testdata/forge_spine_golden"

// uuidShape matches every generated identifier a body or a card id carries:
// delivery row ids (random per run) and native card ids (deterministic, but
// masked alike so the golden reads as shape, not as the bytes of the day).
var uuidShape = regexp.MustCompile(`\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)

// spineMaskIDs replaces every generated identifier with a stable
// first-appearance token.
func spineMaskIDs(tokens map[string]string, text string) string {
	return uuidShape.ReplaceAllStringFunc(text, func(id string) string {
		if tok, ok := tokens[id]; ok {
			return tok
		}
		tok := fmt.Sprintf("<id-%d>", len(tokens)+1)
		tokens[id] = tok
		return tok
	})
}

// spineRenderResp renders one HTTP answer: the status and the masked body.
func spineRenderResp(tokens map[string]string, w *httptest.ResponseRecorder) string {
	return fmt.Sprintf("-> %d %s", w.Code, strings.TrimRight(spineMaskIDs(tokens, w.Body.String()), "\n"))
}

// spineRenderRows renders the fixture webhook's delivery audit rows in a
// stable order, with the fields the legacy contract fixes: the idempotency
// key, the status, the bot, the run and the forge event it claimed.
func spineRenderRows(t *testing.T, s *Server, tenantID, webhookID string, tokens map[string]string) []string {
	t.Helper()
	rows, err := s.webhookDeliveries.ListByWebhook(context.Background(), tenantID, webhookID, 50)
	if err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(rows, func(a, b webhooks.Delivery) int { return strings.Compare(a.IdempotencyKey, b.IdempotencyKey) })
	lines := make([]string, 0, len(rows))
	for _, r := range rows {
		lines = append(lines, fmt.Sprintf("row key=%s status=%s bot=%s run=%s event=%s",
			r.IdempotencyKey, r.Status, r.BotID, r.RunID, r.ForgeDeliveryID))
	}
	return lines
}

// spineRenderCards renders a board's lane cards in id order with their label
// sets — the card labels the legacy writer stamps are part of the contract.
func spineRenderCards(t *testing.T, board native.BoardStore, tokens map[string]string) []string {
	t.Helper()
	cards := laneCards(t, board)
	lines := make([]string, 0, len(cards))
	for _, c := range cards {
		labels := slices.Clone(c.Labels)
		slices.Sort(labels)
		lines = append(lines, fmt.Sprintf("card %s state=%s bot=%s labels=%v",
			spineMaskIDs(tokens, c.ID), c.State, c.Bot, labels))
	}
	return lines
}

// spineCardIDOf reads the card id a carded answer named.
func spineCardIDOf(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	id := laneResp(t, w)["card_id"]
	if id == "" {
		t.Fatalf("no card_id in the answer: %d %s", w.Code, w.Body.String())
	}
	return id
}

func TestIssueLabeled_LegacyRouting_ByteIdentical(t *testing.T) {
	for _, f := range laneForges() {
		t.Run(f.name, func(t *testing.T) {
			tokens := map[string]string{}
			var lines []string
			say := func(format string, args ...any) {
				lines = append(lines, fmt.Sprintf(format, args...))
			}
			// The tenant and webhook id the two fixtures pin (t1, ghw / w1).
			webhookID := map[string]string{"github": "ghw", "gitlab": "w1"}[f.name]

			t.Run("direct", func(t *testing.T) {
				s, runs, _ := newDirectLaneServer(t)
				deliver := f.bind(t, s, "")
				say("== direct lane ==")

				first := f.labeled("2026-11-18T10:00:00Z", "implement")
				w := deliver(first, "d-1")
				say("deliver d-1 implement @10:00 %s", spineRenderResp(tokens, w))
				lines = append(lines, spineRenderRows(t, s, "t1", webhookID, tokens)...)

				// A fresh application while run-1 serves: absorbed.
				absorbed := f.labeled("2026-11-18T10:05:00Z", "implement")
				w = deliver(absorbed, "d-2")
				say("deliver d-2 implement @10:05 (fresh application) %s", spineRenderResp(tokens, w))
				lines = append(lines, spineRenderRows(t, s, "t1", webhookID, tokens)...)

				// The first delivery resent after its run ended: a duplicate.
				runs.end("run-1")
				w = deliver(first, "d-1")
				say("redeliver d-1 after run-1 ended %s", spineRenderResp(tokens, w))

				// Another label: its own lane, a new claim.
				w = deliver(f.labeled("2026-11-18T10:10:00Z", "bug"), "d-3")
				say("deliver d-3 bug @10:10 %s", spineRenderResp(tokens, w))
				lines = append(lines, spineRenderRows(t, s, "t1", webhookID, tokens)...)
			})

			t.Run("board", func(t *testing.T) {
				s, board, _ := newBoardLaneServer(t)
				deliver := f.bind(t, s, "")
				say("== board lane ==")

				first := f.labeled("2026-11-18T10:00:00Z", "implement")
				w := deliver(first, "d-1")
				c1 := spineCardIDOf(t, w)
				say("deliver d-1 implement @10:00 %s", spineRenderResp(tokens, w))
				lines = append(lines, spineRenderCards(t, board, tokens)...)

				// A fresh application while the card serves: absorbed by it.
				w = deliver(f.labeled("2026-11-18T10:05:00Z", "implement"), "d-2")
				say("deliver d-2 implement @10:05 (fresh application) %s", spineRenderResp(tokens, w))
				lines = append(lines, spineRenderCards(t, board, tokens)...)

				// The card ends; the redelivery of the carded event answers
				// from the audit and cards nothing; the second label is its
				// own lane.
				finishCard(t, board, c1)
				w = deliver(first, "d-1")
				say("redeliver d-1 after the card ended %s", spineRenderResp(tokens, w))
				w = deliver(f.labeled("2026-11-18T10:10:00Z", "bug"), "d-3")
				say("deliver d-3 bug @10:10 %s", spineRenderResp(tokens, w))
				lines = append(lines, spineRenderCards(t, board, tokens)...)
			})

			got := strings.Join(lines, "\n") + "\n"
			path := filepath.Join(spineGoldenDir, "legacy_"+f.name+".txt")
			if *updateSpineGolden {
				if err := os.MkdirAll(spineGoldenDir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
				t.Logf("golden rewritten: %s", path)
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read the golden: %v (record it with -update-spine-golden)", err)
			}
			if got != string(want) {
				t.Fatalf("the legacy issue-label routing moved off its recorded bytes (%s):\n--- recorded\n%s\n+++ now\n%s", path, want, got)
			}
		})
	}
}
