package boardops

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
)

// The board TOOLS door refuses the reserved launch context, typed, before
// any operation sees it: the class says what a carded run clones, which
// credential it resolves and which repository meters, bounds and debits it —
// the server's stamp (native.ReservedLaunchArgs), never the caller's. Every
// transport of this funnel is a client side (a bot run holding a board.*
// grant, a webhook-authenticated MCP caller, the operator's session), so the
// refusal sits in CallWithEnv, one chokepoint for all five transports
// (ADR-162 U4's board door — it replaced the silent drop this test used to
// assert: a caller is told it was refused, not told a card that quietly lost
// its launch context). doCreate still drops the class for any future caller
// that reaches it past the door — the store stays writer-agnostic either way
// (TestTheStoreStillAcceptsTheServerStamp below).
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
	_, err = Call(s, NewCapabilities(CapBoardCreate), "create_issue", raw)
	if !errors.Is(err, ErrReservedBotArg) {
		t.Fatalf("err = %v, want the typed refusal of the reserved launch context", err)
	}
	for _, res := range native.ReservedLaunchArgs {
		if !strings.Contains(err.Error(), `"`+res+`"`) {
			t.Errorf("the refusal does not name %s: %v", res, err)
		}
	}
	stored, lerr := s.List(native.ListFilter{})
	if lerr != nil || len(stored) != 0 {
		t.Fatalf("store.List = %v, %d cards, want none — a refused create writes nothing", lerr, len(stored))
	}

	// The caller's own vars still land when the class is absent: the door is
	// about the reserved channel, not about bot_args.
	out, err := Call(s, NewCapabilities(CapBoardCreate), "create_issue", json.RawMessage(`{"title":"tool card","bot":"probe","bot_args":{"member_var":"v"}}`))
	if err != nil {
		t.Fatalf("the clean create was refused: %v", err)
	}
	var answered native.Issue
	if err := json.Unmarshal(out, &answered); err != nil {
		t.Fatal(err)
	}
	if answered.BotArgs["member_var"] != "v" {
		t.Fatalf("the tool door dropped the caller's own var (%v)", answered.BotArgs)
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
