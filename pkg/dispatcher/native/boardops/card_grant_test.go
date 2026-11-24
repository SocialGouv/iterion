package boardops

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/store"
)

// toolGrantSentinel is the bearer planted on the card. The board tools are the
// path that reaches an LLM, so a result printing it is the defect itself.
const toolGrantSentinel = "sentinel-forge-grant-2036-tool"

// plantToolCardCarryingAGrant is a card written BEFORE the store-boundary drop
// existed: it still holds a live publish bearer in its bot args. Planted in the
// store's files, which is the only way such a card can exist now, and read back
// through a freshly opened store.
func plantToolCardCarryingAGrant(t *testing.T) (*native.Store, string) {
	t.Helper()
	root := t.TempDir()
	seed, err := native.NewStore(root)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	iss, err := seed.Create(native.Issue{Title: "a card from before the drop", State: "ready", BotArgs: map[string]string{"loop_cap": "5"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	files, err := filepath.Glob(filepath.Join(root, "issues", "*.json"))
	if err != nil || len(files) != 1 {
		t.Fatalf("issue files = %v (err %v), want exactly one", files, err)
	}
	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatalf("read card: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal card: %v", err)
	}
	doc["bot_args"] = map[string]string{"loop_cap": "5", store.ForgePublishTokenVar: toolGrantSentinel}
	patched, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatalf("marshal card: %v", err)
	}
	if err := os.WriteFile(files[0], patched, 0o600); err != nil {
		t.Fatalf("write card: %v", err)
	}
	s, err := native.NewStore(root)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	held, err := s.Get(iss.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if held.BotArgs[store.ForgePublishTokenVar] != toolGrantSentinel {
		t.Fatalf("the planted card does not carry the grant (%v) — the fixture, not the mask, is broken", held.BotArgs)
	}
	return s, iss.ID
}

// TestNoBoardToolResultCarriesACardsPublishGrant walks the read tools an LLM,
// an agent's in-process board tool and the operator MCP all reach through
// (boardops.Call), plus the write tools that answer with the card they touched.
func TestNoBoardToolResultCarriesACardsPublishGrant(t *testing.T) {
	s, id := plantToolCardCarryingAGrant(t)
	caps := NewCapabilities(strings.Join(AllCapabilities(), ","))

	for _, call := range []struct {
		tool string
		args string
	}{
		{"get_issue", `{"id":"` + id + `"}`},
		{"list_issues", `{}`},
		{"assign_issue", `{"id":"` + id + `","assignee":"someone"}`},
		{"set_labels", `{"id":"` + id + `","labels":["chore"]}`},
		{"add_labels", `{"id":"` + id + `","labels":["extra"]}`},
		{"set_bot", `{"id":"` + id + `","bot":"feature_dev"}`},
		{"comment_issue", `{"id":"` + id + `","body":"a note"}`},
		{"transition_issue", `{"id":"` + id + `","to":"in_progress"}`},
		{"close_issue", `{"id":"` + id + `"}`},
	} {
		res, err := Call(s, caps, call.tool, json.RawMessage(call.args))
		if err != nil {
			t.Fatalf("%s: %v", call.tool, err)
		}
		if strings.Contains(string(res), toolGrantSentinel) {
			t.Errorf("%s hands the bearer to its caller: %s", call.tool, res)
		}
		if !strings.Contains(string(res), store.RedactedLaunchVar) {
			t.Errorf("%s says nothing about the grant the card holds (no mask): %s", call.tool, res)
		}
	}

	// The card itself is untouched: the launch lane reads that bearer to
	// refuse a crossing, and a tool result may not scrub the record.
	held, err := s.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if held.BotArgs[store.ForgePublishTokenVar] != toolGrantSentinel {
		t.Errorf("a tool call rewrote the card's grant: %v", held.BotArgs)
	}
}

// TestTheCreateToolDropsAGrantTheModelSent — create_issue is a writer: the
// grant vars a model puts in bot_args do not reach the card.
func TestTheCreateToolDropsAGrantTheModelSent(t *testing.T) {
	s := newStore(t)
	res, err := Call(s, NewCapabilities(CapBoardCreate), "create_issue",
		json.RawMessage(`{"title":"publish my verdict","bot_args":{"loop_cap":"5","forge_publish_token":"`+toolGrantSentinel+`","forge_pr_state_url":"https://attacker.example/pr"}}`))
	if err != nil {
		t.Fatalf("create_issue: %v", err)
	}
	if strings.Contains(string(res), toolGrantSentinel) {
		t.Errorf("create_issue echoed the bearer: %s", res)
	}
	var created native.Issue
	if err := json.Unmarshal(res, &created); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	held, err := s.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	for _, name := range store.ServerMintedLaunchVars {
		if got, ok := held.BotArgs[name]; ok {
			t.Errorf("the created card carries %s = %q", name, got)
		}
	}
	if held.BotArgs["loop_cap"] != "5" {
		t.Errorf("the model's own arg was lost: %v", held.BotArgs)
	}
}
