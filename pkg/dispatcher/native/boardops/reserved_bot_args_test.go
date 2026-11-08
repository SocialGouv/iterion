package boardops

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
)

// TestCallWithEnv_RefusesReservedBotArgsAtTheDoor.
//
// bot_args keys under `__iterion_` are the server's launch channel: the
// dispatcher lifts them into the launch (repo to clone, BYOK key and secret
// overrides) and the server stamps them from the webhook's word. CallWithEnv
// is the door every board transport crosses — claw's in-process registry,
// `iterion __mcp-board` (claude_code, pi), the HTTP MCP handler, the operator
// MCP — so the refusal is read there, and it decodes arguments the way the
// operations do: a spelling the operation would honour must be one the door
// refuses. The grant is the FULL set — what a TRUSTED run holds — because the
// door is trust-independent (ADR-162 U4): every tool caller is a client.
func TestCallWithEnv_RefusesReservedBotArgsAtTheDoor(t *testing.T) {
	all := NewCapabilities(strings.Join(AllCapabilities(), ","))
	for _, row := range []struct {
		name, tool, args string
		keys             []string
	}{
		{"create_issue, one reserved key", "create_issue", `{"title":"t","bot_args":{"__iterion_secret_overrides":"{}","ok":"1"}}`, []string{"__iterion_secret_overrides"}},
		{"create_issue, every reserved key the launcher knows", "create_issue", `{"title":"t","bot_args":{"__iterion_repo_url":"u","__iterion_repo_ref":"r","__iterion_project_path":"p","__iterion_key_overrides":"{}","__iterion_secret_overrides":"{}","__iterion_webhook_id":"w"}}`,
			[]string{"__iterion_key_overrides", "__iterion_project_path", "__iterion_repo_ref", "__iterion_repo_url", "__iterion_secret_overrides", "__iterion_webhook_id"}},
		{"a key under the prefix this build does not know yet", "create_issue", `{"title":"t","bot_args":{"__iterion_tomorrow_channel":"x"}}`, []string{"__iterion_tomorrow_channel"}},
		{"a prefix spelled in capitals", "create_issue", `{"title":"t","bot_args":{"__ITERION_repo_url":"u"}}`, []string{"__ITERION_repo_url"}},
		{"the field name spelled in capitals, which the decoder folds", "create_issue", `{"title":"t","BOT_ARGS":{"__iterion_repo_url":"u"}}`, []string{"__iterion_repo_url"}},
		{"a second bot_args object, which the decoder merges", "create_issue", `{"title":"t","bot_args":{"ok":"1"},"bot_args":{"__iterion_repo_url":"u"}}`, []string{"__iterion_repo_url"}},
		{"an operation that carries no bot_args today", "set_bot", `{"id":"x","bot":"b","bot_args":{"__iterion_repo_url":"u"}}`, []string{"__iterion_repo_url"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			s := newStore(t)
			_, err := CallWithEnv(s, all, row.tool, json.RawMessage(row.args), CallEnv{})
			if !errors.Is(err, ErrReservedBotArg) {
				t.Fatalf("err = %v, want ErrReservedBotArg", err)
			}
			for _, k := range row.keys {
				if !strings.Contains(err.Error(), `"`+k+`"`) {
					t.Errorf("the refusal does not name %q: %v", k, err)
				}
			}
			issues, lerr := s.List(native.ListFilter{})
			if lerr != nil {
				t.Fatal(lerr)
			}
			if len(issues) != 0 {
				t.Errorf("a card was written: %+v", issues[0])
			}
		})
	}

	t.Run("the clean control lands, with its args", func(t *testing.T) {
		s := newStore(t)
		raw, err := CallWithEnv(s, all, "create_issue", json.RawMessage(`{"title":"t","bot":"feature-dev","bot_args":{"target_branch":"main","iterion_note":"x"}}`), CallEnv{})
		if err != nil {
			t.Fatalf("the clean call was refused: %v", err)
		}
		var iss native.Issue
		if err := json.Unmarshal(raw, &iss); err != nil {
			t.Fatal(err)
		}
		if iss.BotArgs["target_branch"] != "main" || iss.BotArgs["iterion_note"] != "x" {
			t.Errorf("the clean card lost its args: %v", iss.BotArgs)
		}
	})

	t.Run("a missing capability is refused first, as before", func(t *testing.T) {
		_, err := CallWithEnv(newStore(t), NewCapabilities("board.read"), "create_issue", json.RawMessage(`{"title":"t","bot_args":{"__iterion_repo_url":"u"}}`), CallEnv{})
		var denied *CapabilityDeniedError
		if !errors.As(err, &denied) {
			t.Fatalf("err = %v, want a typed capability denial", err)
		}
	})
}

// getSpyStore wraps a store so a test can fail every Get (the set_bot
// read-fails row) or record what a Update call carried (the window row).
type getSpyStore struct {
	native.BoardStore
	failGet  bool
	gotPatch *native.Patch
}

func (s *getSpyStore) Get(id string) (*native.Issue, error) {
	if s.failGet {
		return nil, errors.New("index rebuild in progress")
	}
	return s.BoardStore.Get(id)
}

func (s *getSpyStore) Update(id string, p native.Patch) (*native.Issue, error) {
	s.gotPatch = &p
	return s.BoardStore.Update(id, p)
}

// TestSetBot_ARepointDropsTheReservedLaunchContext.
//
// A webhook-born card carries its launch context in reserved `__iterion_`
// bot args, minted for the bot the card names. The door refuses to WRITE
// those keys; a re-point could still RETARGET them: set_bot drops them
// whenever it changes the card's bot (re-setting the same bot keeps them),
// and it decides from a READ of the card — a read that fails refuses the
// re-point, and the args are rewritten from the card's own non-reserved ones
// even when the read found nothing reserved, so a key written in the window
// cannot ride the change. Trust-independent like the door: the grant below
// is the full set a trusted run holds.
func TestSetBot_ARepointDropsTheReservedLaunchContext(t *testing.T) {
	all := NewCapabilities(strings.Join(AllCapabilities(), ","))
	newCard := func(t *testing.T, s native.BoardStore, bot string, args map[string]string) native.Issue {
		t.Helper()
		raw, err := CallWithEnv(s, all, "create_issue", json.RawMessage(`{"title":"t"}`), CallEnv{})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		var iss native.Issue
		if err := json.Unmarshal(raw, &iss); err != nil {
			t.Fatal(err)
		}
		patchBotArgs := map[string]string{}
		for k, v := range args {
			patchBotArgs[k] = v
		}
		if _, err := s.Update(iss.ID, native.Patch{Bot: &bot, BotArgs: &patchBotArgs}); err != nil {
			t.Fatalf("plant args: %v", err)
		}
		got, err := s.Get(iss.ID)
		if err != nil {
			t.Fatal(err)
		}
		return *got
	}
	setBot := func(t *testing.T, s native.BoardStore, id, bot string) error {
		t.Helper()
		_, err := CallWithEnv(s, all, "set_bot", json.RawMessage(`{"id":"`+id+`","bot":"`+bot+`"}`), CallEnv{})
		return err
	}

	t.Run("a re-point drops the reserved context, ordinary args survive", func(t *testing.T) {
		s := newStore(t)
		iss := newCard(t, s, "feature-dev", map[string]string{
			"__iterion_repo_url": "https://git.example/reviewed", "__iterion_secret_overrides": "{}",
			"target_branch": "main",
		})
		if err := setBot(t, s, iss.ID, "branch-improve-loop"); err != nil {
			t.Fatalf("set_bot: %v", err)
		}
		got, err := s.Get(iss.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Bot != "branch-improve-loop" {
			t.Fatalf("bot = %q, want the re-point to land", got.Bot)
		}
		if got.BotArgs["__iterion_repo_url"] != "" || got.BotArgs["__iterion_secret_overrides"] != "" {
			t.Errorf("the reserved context rode the re-point: %v", got.BotArgs)
		}
		if got.BotArgs["target_branch"] != "main" {
			t.Errorf("an ordinary arg was lost: %v", got.BotArgs)
		}
	})

	t.Run("re-setting the same bot keeps the context", func(t *testing.T) {
		s := newStore(t)
		iss := newCard(t, s, "feature-dev", map[string]string{"__iterion_repo_url": "https://git.example/reviewed"})
		if err := setBot(t, s, iss.ID, "feature-dev"); err != nil {
			t.Fatalf("set_bot: %v", err)
		}
		got, err := s.Get(iss.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.BotArgs["__iterion_repo_url"] == "" {
			t.Errorf("the same-bot re-set dropped the context: %v", got.BotArgs)
		}
	})

	t.Run("a card whose read fails keeps its bot", func(t *testing.T) {
		spy := &getSpyStore{BoardStore: newStore(t)}
		iss := newCard(t, spy, "feature-dev", nil)
		spy.failGet = true
		err := setBot(t, spy, iss.ID, "branch-improve-loop")
		if err == nil || !strings.Contains(err.Error(), "read issue") {
			t.Fatalf("err = %v, want the re-point refused on the failed read", err)
		}
		spy.failGet = false
		got, gerr := spy.Get(iss.ID)
		if gerr != nil {
			t.Fatal(gerr)
		}
		if got.Bot != "feature-dev" {
			t.Errorf("a card nobody could read was re-pointed to %q", got.Bot)
		}
	})

	t.Run("a reserved key written in the window does not ride the change", func(t *testing.T) {
		s := newStore(t)
		iss := newCard(t, s, "feature-dev", map[string]string{"target_branch": "main"})
		spy := &getSpyStore{BoardStore: s}
		// The card as set_bot's read sees it AFTER another writer planted a
		// reserved key: the re-point's rewrite starts from that read, so the
		// key must be dropped from the very args the write carries.
		planted, err := s.Update(iss.ID, native.Patch{BotArgs: &map[string]string{"target_branch": "main", "__iterion_repo_url": "https://attacker.example/clone"}})
		if err != nil {
			t.Fatal(err)
		}
		_ = planted
		if err := setBot(t, spy, iss.ID, "branch-improve-loop"); err != nil {
			t.Fatalf("set_bot: %v", err)
		}
		if spy.gotPatch == nil || spy.gotPatch.BotArgs == nil {
			t.Fatalf("the re-point carried no args rewrite: %+v", spy.gotPatch)
		}
		if (*spy.gotPatch.BotArgs)["__iterion_repo_url"] != "" {
			t.Errorf("a reserved key written in the window rode the change: %v", *spy.gotPatch.BotArgs)
		}
		if (*spy.gotPatch.BotArgs)["target_branch"] != "main" {
			t.Errorf("an ordinary arg did not survive the rewrite: %v", *spy.gotPatch.BotArgs)
		}
	})
}
