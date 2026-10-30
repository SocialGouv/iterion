package boardops

import (
	"encoding/json"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
)

// The board TOOLS door drops the reserved launch context like every other
// client-facing board write: the class says what a carded run clones, which
// credential it resolves and which repository meters, bounds and debits it —
// the server's stamp (native.ReservedLaunchArgs), never the caller's. Every
// transport of this funnel is a client side (a bot run holding a board.*
// grant, a webhook-authenticated MCP caller, the operator's session), so the
// filter sits here in doCreate, one chokepoint for all five transports. The
// caller's own vars land, and the response is the stored card, so the drop is
// visible rather than silent.
func TestToolCreateDropsTheReservedLaunchContext(t *testing.T) {
	s := newStore(t)
	botArgs := map[string]string{"member_var": "v"}
	for _, res := range native.ReservedLaunchArgs {
		botArgs[res] = "forged-" + res
	}
	raw, err := json.Marshal(map[string]any{
		"title": "tool card", "bot": "probe", "bot_args": botArgs,
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := Call(s, NewCapabilities(CapBoardCreate), "create_issue", raw)
	if err != nil {
		t.Fatal(err)
	}
	var answered native.Issue
	if err := json.Unmarshal(out, &answered); err != nil {
		t.Fatal(err)
	}
	if answered.BotArgs["member_var"] != "v" {
		t.Fatalf("the tool door dropped the caller's own var (%v) — this test only means something while the door is open", answered.BotArgs)
	}
	stored, err := s.List(native.ListFilter{})
	if err != nil || len(stored) != 1 {
		t.Fatalf("store.List = %v, %d cards, want 1", err, len(stored))
	}
	// The answer is the stored card: assert the class off the store, not the
	// echo, so the door's filter is what the test sees.
	for _, args := range []map[string]string{answered.BotArgs, stored[0].BotArgs} {
		for _, res := range native.ReservedLaunchArgs {
			if got, ok := args[res]; ok {
				t.Errorf("the tool door persisted the reserved %s = %q — launch context is the server's stamp, not tool input", res, got)
			}
		}
	}
}

// The control half of the tool door: the STORE keeps accepting the reserved
// launch context. The doors filter, never the store — the store twins stay
// writer-agnostic because the server's own stamp (ensureBoardCard, the
// webhook lane) writes through the same Create (card_launch_args.go), so a
// store-side drop would erase every webhook-carried card's identity.
func TestTheStoreStillAcceptsTheServerStamp(t *testing.T) {
	s := newStore(t)
	botArgs := map[string]string{"member_var": "v"}
	for _, res := range native.ReservedLaunchArgs {
		botArgs[res] = "stamped-" + res
	}
	iss, err := s.Create(native.Issue{Title: "webhook card", Bot: "probe", BotArgs: botArgs})
	if err != nil {
		t.Fatal(err)
	}
	if iss.BotArgs["member_var"] != "v" {
		t.Fatalf("the store dropped the writer's own var (%v) — this test only means something while the store is open", iss.BotArgs)
	}
	for _, res := range native.ReservedLaunchArgs {
		if got := iss.BotArgs[res]; got != "stamped-"+res {
			t.Errorf("the store scrubbed the reserved %s (got %q) — the stamp lane writes through Create, so the store must not filter this class", res, got)
		}
	}
}
