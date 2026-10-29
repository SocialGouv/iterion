package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/store"
)

// cliGrantSentinel is the bearer planted on the card `iterion issue --json`
// reads back.
const cliGrantSentinel = "sentinel-forge-grant-2036-cli"

// plantCLICardCarryingAGrant seeds the store `iterion issue` opens with a card
// written BEFORE the store-boundary drop existed — one that still holds a live
// publish bearer. It returns the store dir the commands take and the card's id.
func plantCLICardCarryingAGrant(t *testing.T) (string, string) {
	t.Helper()
	storeDir := t.TempDir()
	root := filepath.Join(storeDir, "dispatcher")
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
	doc["bot_args"] = map[string]string{"loop_cap": "5", store.ForgePublishTokenVar: cliGrantSentinel}
	patched, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatalf("marshal card: %v", err)
	}
	if err := os.WriteFile(files[0], patched, 0o600); err != nil {
		t.Fatalf("write card: %v", err)
	}
	return storeDir, iss.ID
}

// TestIterionIssueJSONNeverPrintsACardsPublishGrant — `iterion issue show
// --json` and `iterion issue list --json` are read surfaces like the REST API
// and the board tools.
func TestIterionIssueJSONNeverPrintsACardsPublishGrant(t *testing.T) {
	storeDir, id := plantCLICardCarryingAGrant(t)

	var show bytes.Buffer
	if err := RunIssueShow(&Printer{W: &show, Format: OutputJSON}, IssueRefOptions{
		IssueCommonOptions: IssueCommonOptions{StoreDir: storeDir},
		IDOrPrefix:         id,
	}); err != nil {
		t.Fatalf("RunIssueShow: %v", err)
	}
	var list bytes.Buffer
	if err := RunIssueList(&Printer{W: &list, Format: OutputJSON}, IssueListOptions{
		IssueCommonOptions: IssueCommonOptions{StoreDir: storeDir},
	}); err != nil {
		t.Fatalf("RunIssueList: %v", err)
	}

	for _, out := range []struct {
		what string
		body string
	}{{"issue show --json", show.String()}, {"issue list --json", list.String()}} {
		if strings.Contains(out.body, cliGrantSentinel) {
			t.Errorf("%s prints the bearer: %s", out.what, out.body)
		}
		if !strings.Contains(out.body, store.RedactedLaunchVar) {
			t.Errorf("%s says nothing about the grant the card holds (no mask): %s", out.what, out.body)
		}
		if !strings.Contains(out.body, "loop_cap") {
			t.Errorf("%s lost the card's own bot args: %s", out.what, out.body)
		}
	}
}

// TestIterionIssueCreateDropsAGrantTheOperatorTyped — `iterion issue create`
// is one of the four writers: --bot-arg forge_publish_token=… does not reach
// the card.
func TestIterionIssueCreateDropsAGrantTheOperatorTyped(t *testing.T) {
	storeDir := t.TempDir()
	var out bytes.Buffer
	if err := RunIssueCreate(&Printer{W: &out, Format: OutputJSON}, IssueCreateOptions{
		IssueCommonOptions: IssueCommonOptions{StoreDir: storeDir},
		Title:              "publish the verdict",
		BotArgs:            []string{"loop_cap=5", "forge_publish_token=" + cliGrantSentinel},
	}); err != nil {
		t.Fatalf("RunIssueCreate: %v", err)
	}
	if strings.Contains(out.String(), cliGrantSentinel) {
		t.Errorf("issue create echoed the bearer: %s", out.String())
	}
	s, err := native.NewStore(filepath.Join(storeDir, "dispatcher"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	issues, err := s.List(native.ListFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("stored %d cards, want 1", len(issues))
	}
	if got, ok := issues[0].BotArgs[store.ForgePublishTokenVar]; ok {
		t.Errorf("the created card carries the bearer: %q", got)
	}
	if issues[0].BotArgs["loop_cap"] != "5" {
		t.Errorf("the operator's own arg was lost: %v", issues[0].BotArgs)
	}
}
