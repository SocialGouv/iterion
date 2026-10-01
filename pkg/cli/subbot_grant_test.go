package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// cliGrantFixture blanks the host's credential env, pins a sealer that never
// reaches the OS keyring over D-Bus, writes child.bot, and returns the store,
// its directory and a fresh grant.
func cliGrantFixture(t *testing.T, dir, child string) (*store.FilesystemRunStore, string, string) {
	t.Helper()
	for _, raw := range os.Environ() {
		if name, _, _ := strings.Cut(raw, "="); store.IsSecretEnvName(name) && !strings.HasPrefix(name, "GIT_CONFIG_") {
			t.Setenv(name, "")
		}
	}
	t.Setenv("ITERION_SECRETS_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("k"), 32)))
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/nonexistent/iterion-test")
	if err := os.WriteFile(filepath.Join(dir, "child.bot"), []byte(child), 0o644); err != nil {
		t.Fatal(err)
	}
	storeDir := filepath.Join(dir, "store")
	s, err := store.New(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return s, storeDir, hex.EncodeToString(b)
}

// A sub-bot child its parent handed nothing cannot declare its way to the
// parent's grant: a child's `{{vars.X}}` resolves against the child's own
// vars, never the launch vars of the run that spawned it.
func TestSubbotRunnerForCLI_AChildNeverDeclaresItsWayToTheParentsGrant(t *testing.T) {
	dir := t.TempDir()
	written := filepath.Join(dir, "what-the-child-saw.txt")
	s, storeDir, grant := cliGrantFixture(t, dir, `vars:
  forge_publish_token: string = ""

secrets:
  g: "{{vars.forge_publish_token}}"

schema out:
  ok: bool

tool grab:
  command: `+"`printf '%s' {{secrets.g}} > "+written+`; printf '{"ok":true}'`+"`"+`
  output: out

workflow child:
  worktree: none
  entry: grab
  grab -> done
`)
	runner := subbotRunnerForCLI(filepath.Join(dir, "parent.bot"), storeDir, s, iterlog.Nop(),
		RunOptions{NoInteractive: true, Vars: map[string]string{store.ForgePublishTokenVar: grant}})
	if _, err := runner(context.Background(), runtime.SubbotRequest{
		Source:      "child.bot",
		Vars:        map[string]any{}, // the parent hands NOTHING
		ParentRunID: "parent-run",
		NodeID:      "run_child",
	}); err != nil {
		t.Fatalf("child run failed: %v", err)
	}
	saw, err := os.ReadFile(written)
	if err != nil {
		t.Fatalf("the child's tool wrote nothing: the test proves nothing (%v)", err)
	}
	if strings.Contains(string(saw), grant) {
		t.Fatal("a child its parent did not hand the grant to obtained it through a declared secret")
	}
}

// A child the parent DOES hand the grant to, under another name: its sinks
// carry it nowhere. The child's own record does not exist yet when its
// executor is built, so its guard learns the value from the lineage.
func TestSubbotRunnerForCLI_AHandedGrantIsRedactedFromTheChildsEvents(t *testing.T) {
	dir := t.TempDir()
	s, storeDir, grant := cliGrantFixture(t, dir, `vars:
  bearer: string = ""

schema out:
  seen: string

tool echo:
  command: `+"`printf '{\"seen\":\"saw-%s\"}' {{vars.bearer}}`"+`
  output: out

workflow child:
  worktree: none
  entry: echo
  echo -> done
`)
	ctx := context.Background()
	if _, err := s.CreateRun(ctx, "parent-run", "parent", map[string]any{store.ForgePublishTokenVar: grant}); err != nil {
		t.Fatal(err)
	}
	runner := subbotRunnerForCLI(filepath.Join(dir, "parent.bot"), storeDir, s, iterlog.Nop(),
		RunOptions{NoInteractive: true, Vars: map[string]string{store.ForgePublishTokenVar: grant}})
	if _, err := runner(ctx, runtime.SubbotRequest{
		Source:      "child.bot",
		Vars:        map[string]any{"bearer": grant}, // handed under another name
		ParentRunID: "parent-run",
		NodeID:      "run_child",
	}); err != nil {
		t.Fatalf("child run failed: %v", err)
	}
	ids, err := s.ListChildRuns(ctx, "parent-run")
	if err != nil || len(ids) == 0 {
		t.Fatalf("no child run recorded: %v %v", ids, err)
	}
	evts, err := s.LoadEvents(ctx, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(evts)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "saw-") {
		t.Fatal("the child's echo left no trace: the test proves nothing")
	}
	if n := strings.Count(string(raw), grant); n > 0 {
		t.Fatalf("the child's event log carries the grant %d time(s)", n)
	}
}
