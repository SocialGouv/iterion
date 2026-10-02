package dispatcher

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

const grantEchoChildBot = `
vars:
  publish_bearer: string = ""

schema seen_out:
  seen: string

tool echo:
  command: ` + "`printf '{\"seen\":\"%s\"}' {{vars.publish_bearer}}`" + `
  output: seen_out

workflow echo_child:
  entry: echo
  echo -> done
`

const grantEchoParentBot = `
vars:
  forge_publish_token: string = ""

schema child_out:
  seen: string

tool echo_self:
  command: ` + "`printf '{\"seen\":\"self-%s\"}' {{vars.forge_publish_token}}`" + `
  output: child_out

subbot run_child:
  source: "child.bot"
  with {
    publish_bearer: "{{vars.forge_publish_token}}"
  }
  output: child_out

workflow echo_parent:
  entry: echo_self
  echo_self -> run_child
  run_child -> done
`

// A dispatched bot's sub-bot child receives the parent's grant under its own
// name before its record exists: its guard knows the grant by value from the
// parent (ExecutorSpec.ParentRunID), so the child's event log never carries it.
func TestEngineRunner_ASubbotChildsEventsNeverCarryTheParentsGrant(t *testing.T) {
	for _, raw := range os.Environ() {
		if name, _, _ := strings.Cut(raw, "="); store.IsSecretEnvName(name) && !strings.HasPrefix(name, "GIT_CONFIG_") {
			t.Setenv(name, "")
		}
	}
	botDir := t.TempDir()
	parentPath := filepath.Join(botDir, "parent.bot")
	if err := os.WriteFile(parentPath, []byte(grantEchoParentBot), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(botDir, "child.bot"), []byte(grantEchoChildBot), 0o644); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	grant := hex.EncodeToString(b)

	storeDir := t.TempDir()
	runner, err := NewEngineRunner(parentPath, iterlog.Nop())
	if err != nil {
		t.Fatalf("NewEngineRunner: %v", err)
	}
	defer func() { _ = runner.Close() }()
	runID, err := store.GenerateRunID()
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.Dispatch(context.Background(), DispatchSpec{
		RunID:         runID,
		WorkspacePath: t.TempDir(),
		StoreDir:      storeDir,
		Vars:          map[string]any{store.ForgePublishTokenVar: grant},
		Issue:         &IssueRef{ID: "native:" + runID, Identifier: runID, Title: "grant under dispatch"},
	}); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	s, err := store.New(storeDir, store.WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatal(err)
	}
	ids, err := s.ListRuns(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var childID string
	for _, id := range ids {
		if r, lerr := s.LoadRun(context.Background(), id); lerr == nil && r.ParentRunID == runID {
			childID = id
		}
	}
	if childID == "" {
		t.Fatal("no child run linked to the parent")
	}
	evts, err := s.LoadEvents(context.Background(), childID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(evts)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(raw), grant); n > 0 {
		t.Fatalf("the child's event log carries the parent's grant %d time(s)", n)
	}
	if !strings.Contains(string(raw), `"seen"`) {
		t.Fatal("the child's echo node left no trace: the test proves nothing")
	}

	// The dispatched run itself: its record does not exist when its executor
	// is built, so its own dispatch vars are the only place its grant can be.
	parentEvts, err := s.LoadEvents(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	parentRaw, err := json.Marshal(parentEvts)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(parentRaw), "self-") {
		t.Fatal("the dispatched run's own echo left no trace: the test proves nothing")
	}
	if n := strings.Count(string(parentRaw), grant); n > 0 {
		t.Fatalf("the dispatched run's own event log carries the grant it was dispatched with %d time(s)", n)
	}
}
