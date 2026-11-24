package native

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// grantSentinel is the bearer a test plants on a card. Distinctive enough that
// a surface printing it is unmistakable in the failure message.
const grantSentinel = "sentinel-forge-grant-2036-1dd3f7a0"

// TestTheWriteBoundaryDropsAGrantAWriterSent — the four writers (REST, the
// board MCP tools, `iterion issue`, trigger promote) all land in Create or
// Update, so the drop sits there once instead of at each of them.
func TestTheWriteBoundaryDropsAGrantAWriterSent(t *testing.T) {
	s := newTestStore(t)
	created, err := s.Create(Issue{
		Title: "publish the verdict",
		State: "ready",
		BotArgs: map[string]string{
			"loop_cap":                         "5",
			store.ForgePublishTokenVar:         grantSentinel,
			store.ForgePublishURLVar:           "https://attacker.example/publish",
			store.ForgePRStateURLVar:           "https://attacker.example/pr",
			store.ForgeDeliveryPreflightURLVar: "https://attacker.example/preflight",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, where := range []struct {
		what string
		iss  *Issue
	}{{"the record Create returned", created}, {"the stored card", mustGet(t, s, created.ID)}} {
		for _, name := range store.ServerMintedLaunchVars {
			if got, ok := where.iss.BotArgs[name]; ok {
				t.Errorf("%s carries the minted var %s = %q", where.what, name, got)
			}
		}
		if where.iss.BotArgs["loop_cap"] != "5" {
			t.Errorf("%s lost the writer's own arg: %v", where.what, where.iss.BotArgs)
		}
	}

	// A patch is a wholesale swap: the grant is dropped from it too.
	next := map[string]string{"loop_cap": "7", store.ForgePublishTokenVar: grantSentinel}
	updated, err := s.Update(created.ID, Patch{BotArgs: &next})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got, ok := updated.BotArgs[store.ForgePublishTokenVar]; ok {
		t.Errorf("the patch stored the minted var: %q", got)
	}
	if updated.BotArgs["loop_cap"] != "7" {
		t.Errorf("the patch lost its own arg: %v", updated.BotArgs)
	}
	if next[store.ForgePublishTokenVar] != grantSentinel {
		t.Errorf("the caller's patch map was mutated: %v", next)
	}

	// A patch carrying the grant and NOTHING else clears the args: storing
	// the grant is the one thing it may not do.
	only := map[string]string{store.ForgePublishTokenVar: grantSentinel}
	cleared, err := s.Update(created.ID, Patch{BotArgs: &only})
	if err != nil {
		t.Fatalf("Update grant-only: %v", err)
	}
	if len(cleared.BotArgs) != 0 {
		t.Errorf("grant-only patch stored %v, want empty bot args", cleared.BotArgs)
	}
}

// TestTheWriteBoundaryKeepsAnExplicitWithdrawal — an EMPTY value under a
// minted name grants nothing and names nowhere: it is a card saying "this bot
// does not publish", and dropping it would hand the launch its own fresh grant
// instead, overriding an explicit choice with the opposite one.
func TestTheWriteBoundaryKeepsAnExplicitWithdrawal(t *testing.T) {
	s := newTestStore(t)
	created, err := s.Create(Issue{
		Title:   "no publishing from this card",
		State:   "ready",
		BotArgs: map[string]string{store.ForgePublishTokenVar: ""},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, ok := mustGet(t, s, created.ID).BotArgs[store.ForgePublishTokenVar]
	if !ok || got != "" {
		t.Errorf("the withdrawal was dropped: got %q, present=%v", got, ok)
	}
}

// TestIssueForReadMasksTheGrantAndNeverTouchesTheRecord — the retroactive
// half: a card WRITTEN before the drop existed can still carry a live bearer,
// so every surface handing a card out masks it.
func TestIssueForReadMasksTheGrantAndNeverTouchesTheRecord(t *testing.T) {
	carried := &Issue{
		ID:    "native:11111111-1111-4111-8111-111111111111",
		Title: "a card from before the drop",
		BotArgs: map[string]string{
			"loop_cap":                 "5",
			store.ForgePublishTokenVar: grantSentinel,
		},
	}
	masked := IssueForRead(carried)
	if masked == carried {
		t.Fatal("IssueForRead returned the record itself — the mask would be written into the store's clone")
	}
	if got := masked.BotArgs[store.ForgePublishTokenVar]; got != store.RedactedLaunchVar {
		t.Errorf("read surface shows %q, want %q", got, store.RedactedLaunchVar)
	}
	if masked.BotArgs["loop_cap"] != "5" {
		t.Errorf("the mask ate the card's own args: %v", masked.BotArgs)
	}
	if masked.Title != carried.Title {
		t.Errorf("Title = %q, want the card's own", masked.Title)
	}
	if got := carried.BotArgs[store.ForgePublishTokenVar]; got != grantSentinel {
		t.Errorf("the record was mutated: %q — the launch lane reads this bearer to refuse a crossing", got)
	}

	// A card with nothing to mask is handed out as it is: no copy, so no
	// surface pays for the common case.
	plain := &Issue{ID: carried.ID, BotArgs: map[string]string{"loop_cap": "5"}}
	if IssueForRead(plain) != plain {
		t.Error("IssueForRead copied a card that carries no grant")
	}
	if IssueForRead(nil) != nil {
		t.Error("IssueForRead(nil) should stay nil")
	}
}

// TestIssuesForReadMasksEveryCardThatCarriesAGrant — the list surfaces. A nil
// entry travels as it is rather than panicking a read.
func TestIssuesForReadMasksEveryCardThatCarriesAGrant(t *testing.T) {
	plain := &Issue{ID: "native:22222222-2222-4222-8222-222222222222", BotArgs: map[string]string{"loop_cap": "5"}}
	carried := &Issue{
		ID:      "native:33333333-3333-4333-8333-333333333333",
		BotArgs: map[string]string{store.ForgePublishTokenVar: grantSentinel},
	}
	in := []*Issue{plain, nil, carried}
	out := IssuesForRead(in)
	if len(out) != 3 {
		t.Fatalf("len = %d, want 3", len(out))
	}
	if out[1] != nil {
		t.Error("a nil entry should travel as it is")
	}
	if got := out[2].BotArgs[store.ForgePublishTokenVar]; got != store.RedactedLaunchVar {
		t.Errorf("list surface shows %q, want %q", got, store.RedactedLaunchVar)
	}
	if carried.BotArgs[store.ForgePublishTokenVar] != grantSentinel {
		t.Error("the listed record was mutated")
	}
	if same := IssuesForRead([]*Issue{plain}); len(same) != 1 || same[0] != plain {
		t.Error("IssuesForRead copied a list in which no card carries a grant")
	}
}

func mustGet(t *testing.T, s *Store, id string) *Issue {
	t.Helper()
	iss, err := s.Get(id)
	if err != nil {
		t.Fatalf("Get %s: %v", id, err)
	}
	return iss
}

// plantGrantOnCard writes a live bearer onto a stored card's files and reopens
// the store: the only state a card holding a grant can exist in, since the
// write boundary drops the class from every Create and Update (cards from
// before the drop — the studio's Edit form used to pre-fill it).
func plantGrantOnCard(t *testing.T, s *Store, id string) {
	t.Helper()
	// The FS twin keeps one file per card under issues/, addressed by id.
	iss := mustGet(t, s, id)
	path := s.issuePath(iss.ID)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read card file %s: %v", path, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal card: %v", err)
	}
	doc["bot_args"] = map[string]string{"loop_cap": "5", store.ForgePublishTokenVar: grantSentinel}
	patched, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatalf("marshal card: %v", err)
	}
	if err := os.WriteFile(path, patched, 0o600); err != nil {
		t.Fatalf("write card: %v", err)
	}
	if err := s.Reconcile(); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := mustGet(t, s, id).BotArgs[store.ForgePublishTokenVar]; got != grantSentinel {
		t.Fatalf("the planted card does not hold the grant (%v) — the fixture, not the seam, is broken", got)
	}
}

// TestTheWriteBoundaryKeepsAGrantTheCardAlreadyHeld — the wholesale-patch
// half of the boundary. An args rewrite (a set_bot re-point, a studio Edit
// form, a trigger promote) lands in Update with a whole map: the grant a
// writer SENT is still dropped, and the grant the card HELD survives the swap,
// because the launch lane reads that bearer to refuse a crossing and a patch
// the caller meant as an args edit must not scrub it. The one way past the
// keep is the explicit empty value that withdraws.
func TestTheWriteBoundaryKeepsAGrantTheCardAlreadyHeld(t *testing.T) {
	s := newTestStore(t)
	created, err := s.Create(Issue{Title: "a card from before the drop", State: "ready"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	plantGrantOnCard(t, s, created.ID)

	// A rewrite that does not name the grant: the card's own arg lands, the
	// held bearer comes back off the record.
	rewrite := map[string]string{"loop_cap": "9"}
	updated, err := s.Update(created.ID, Patch{BotArgs: &rewrite})
	if err != nil {
		t.Fatalf("Update rewrite: %v", err)
	}
	if got := updated.BotArgs[store.ForgePublishTokenVar]; got != grantSentinel {
		t.Errorf("an args rewrite scrubbed the card's grant: %v", updated.BotArgs)
	}
	if updated.BotArgs["loop_cap"] != "9" {
		t.Errorf("the rewrite's own arg was lost: %v", updated.BotArgs)
	}

	// A rewrite carrying a forged value under the minted name: the forge is
	// dropped (BotArgsForWrite), the held bearer still survives — a client
	// can neither set nor SWAP the grant, only retire it.
	forged := map[string]string{"loop_cap": "9", store.ForgePublishTokenVar: "attacker-bearer"}
	updated, err = s.Update(created.ID, Patch{BotArgs: &forged})
	if err != nil {
		t.Fatalf("Update forged: %v", err)
	}
	if got := updated.BotArgs[store.ForgePublishTokenVar]; got != grantSentinel {
		t.Errorf("a forged value displaced the card's grant: %v", updated.BotArgs)
	}
	if strings.Contains(fmt.Sprint(updated.BotArgs), "attacker-bearer") {
		t.Errorf("the forged value was stored: %v", updated.BotArgs)
	}

	// The explicit withdrawal is the one patch that retires the grant.
	withdraw := map[string]string{"loop_cap": "9", store.ForgePublishTokenVar: ""}
	updated, err = s.Update(created.ID, Patch{BotArgs: &withdraw})
	if err != nil {
		t.Fatalf("Update withdrawal: %v", err)
	}
	if got, ok := updated.BotArgs[store.ForgePublishTokenVar]; !ok || got != "" {
		t.Errorf("the withdrawal did not retire the grant: present=%v got %q", ok, got)
	}
}
