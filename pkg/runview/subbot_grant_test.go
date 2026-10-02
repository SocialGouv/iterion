package runview

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

const grantEchoChild = `
vars:
  publish_bearer: string = ""

schema seen_out:
  seen: string

schema gate_out:
  approved: bool

tool echo:
  command: ` + "`printf '{\"seen\":\"%s\"}' {{vars.publish_bearer}}`" + `
  output: seen_out

human review:
  output: gate_out

workflow echo_child:
  entry: echo
  echo -> review
  review -> done
`

const grantEchoParent = `
vars:
  forge_publish_token: string = ""

schema child_out:
  approved: bool

subbot run_child:
  source: "echo_child.bot"
  with {
    publish_bearer: "{{vars.forge_publish_token}}"
  }
  output: child_out

workflow echo_parent:
  entry: run_child
  run_child -> done
`

// A sub-bot child receives its parent's grant under its own name, through
// `with:`, before its record exists: its guard knows the grant by value from
// the parent (ExecutorSpec.ParentRunID), so the child's event log never
// carries it.
func TestASubbotChildsEventsNeverCarryTheParentsGrant(t *testing.T) {
	for _, raw := range os.Environ() {
		if name, _, _ := strings.Cut(raw, "="); store.IsSecretEnvName(name) && !strings.HasPrefix(name, "GIT_CONFIG_") {
			t.Setenv(name, "")
		}
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "echo_child.bot"), []byte(grantEchoChild), 0o644); err != nil {
		t.Fatal(err)
	}
	parentPath := filepath.Join(dir, "echo_parent.bot")
	if err := os.WriteFile(parentPath, []byte(grantEchoParent), 0o644); err != nil {
		t.Fatal(err)
	}
	svc, err := NewService(dir, WithLogger(iterlog.Nop()), WithWorkDir(gittest.SourceRepo(t)))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	defer stopService(t, svc)

	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	grant := hex.EncodeToString(b)
	res, err := svc.Launch(context.Background(), LaunchSpec{FilePath: parentPath, Vars: map[string]string{store.ForgePublishTokenVar: grant}})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	childID := waitForSubbotStatus(t, svc, res.RunID, store.RunStatusPausedWaitingHuman)
	evts, err := svc.store.LoadEvents(context.Background(), childID)
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
}

const resumedEchoChild = `
vars:
  publish_bearer: string = ""

schema seen_out:
  seen: string

schema gate_out:
  approved: bool

human review:
  output: gate_out

tool echo_after:
  command: ` + "`printf '{\"seen\":\"after-%s\"}' {{vars.publish_bearer}}`" + `
  output: seen_out

workflow echo_child:
  entry: review
  review -> echo_after
  echo_after -> done
`

// A resume names the run alone, and a sub-bot child holds its parent's grant
// under the name its `with:` chose: its resumed guard learns the grant from
// the parent its record names.
func TestAResumedSubbotChildsEventsNeverCarryTheParentsGrant(t *testing.T) {
	svc, dir := grantService(t, map[string]string{"echo_child.bot": resumedEchoChild, "echo_parent.bot": grantEchoParent})
	grant := randomGrant(t)
	res, err := svc.Launch(context.Background(), LaunchSpec{FilePath: filepath.Join(dir, "echo_parent.bot"), Vars: map[string]string{store.ForgePublishTokenVar: grant}})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	childID := waitForSubbotStatus(t, svc, res.RunID, store.RunStatusPausedWaitingHuman)
	child, err := svc.store.LoadRun(context.Background(), childID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Resume(context.Background(), ResumeSpec{RunID: childID, FilePath: child.FilePath, Answers: map[string]any{"approved": true}}); err != nil {
		t.Fatalf("resume child: %v", err)
	}
	waitStatus(t, svc, childID, store.RunStatusFinished)
	assertEventsWithoutGrant(t, svc, childID, grant, `after-`)
}

const echoGrandchild = `
vars:
  bearer2: string = ""

schema seen_out:
  seen: string

schema gate_out:
  approved: bool

tool echo:
  command: ` + "`printf '{\"seen\":\"gc-%s\"}' {{vars.bearer2}}`" + `
  output: seen_out

human review:
  output: gate_out

workflow echo_grandchild:
  entry: echo
  echo -> review
  review -> done
`

const echoMidChild = `
vars:
  bearer1: string = ""

schema gate_out:
  approved: bool

subbot run_gc:
  source: "echo_grandchild.bot"
  with {
    bearer2: "{{vars.bearer1}}"
  }
  output: gate_out

workflow echo_mid:
  entry: run_gc
  run_gc -> done
`

const echoRoot = `
vars:
  forge_publish_token: string = ""

schema gate_out:
  approved: bool

subbot run_mid:
  source: "echo_mid.bot"
  with {
    bearer1: "{{vars.forge_publish_token}}"
  }
  output: gate_out

workflow echo_root:
  entry: run_mid
  run_mid -> done
`

// A grandchild's parent holds the root's grant under an alias only: the
// grandchild's guard learns it from the root, up the whole lineage.
func TestAGrandchildsEventsNeverCarryTheRootsGrant(t *testing.T) {
	svc, dir := grantService(t, map[string]string{"echo_grandchild.bot": echoGrandchild, "echo_mid.bot": echoMidChild, "echo_root.bot": echoRoot})
	grant := randomGrant(t)
	res, err := svc.Launch(context.Background(), LaunchSpec{FilePath: filepath.Join(dir, "echo_root.bot"), Vars: map[string]string{store.ForgePublishTokenVar: grant}})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	midID := waitForSubbotStatus(t, svc, res.RunID, store.RunStatusRunning)
	gcID := waitForSubbotStatus(t, svc, midID, store.RunStatusPausedWaitingHuman)
	assertEventsWithoutGrant(t, svc, gcID, grant, `gc-`)
}

func grantService(t *testing.T, bots map[string]string) (*Service, string) {
	t.Helper()
	for _, raw := range os.Environ() {
		if name, _, _ := strings.Cut(raw, "="); store.IsSecretEnvName(name) && !strings.HasPrefix(name, "GIT_CONFIG_") {
			t.Setenv(name, "")
		}
	}
	dir := t.TempDir()
	for name, body := range bots {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	svc, err := NewService(dir, WithLogger(iterlog.Nop()), WithWorkDir(gittest.SourceRepo(t)))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	t.Cleanup(func() { stopService(t, svc) })
	return svc, dir
}

func randomGrant(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// assertEventsWithoutGrant fails when runID's event log carries grant, and
// when it lacks witness — the trace of the node that rendered the grant,
// without which the absence proves nothing.
func assertEventsWithoutGrant(t *testing.T, svc *Service, runID, grant, witness string) {
	t.Helper()
	evts, err := svc.store.LoadEvents(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(evts)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), witness) {
		t.Fatalf("run %s's event log has no %q: the node that renders the grant never ran", runID, witness)
	}
	if n := strings.Count(string(raw), grant); n > 0 {
		t.Fatalf("run %s's event log carries the grant %d time(s)", runID, n)
	}
}

const pausedRoot = `
vars:
  forge_publish_token: string = ""

schema gate_out:
  approved: bool

human review:
  output: gate_out

workflow paused_root:
  entry: review
  review -> done
`

// A pause's questions carry the paused node's input — at an entry node, the
// run's vars. The event is an observational sink and carries the redacted
// copy; the interaction record keeps the values a resume reads back.
func TestAPauseEventNeverCarriesTheRunsGrant(t *testing.T) {
	svc, dir := grantService(t, map[string]string{"paused_root.bot": pausedRoot})
	grant := randomGrant(t)
	res, err := svc.Launch(context.Background(), LaunchSpec{FilePath: filepath.Join(dir, "paused_root.bot"), Vars: map[string]string{store.ForgePublishTokenVar: grant}})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	waitStatus(t, svc, res.RunID, store.RunStatusPausedWaitingHuman)
	assertEventsWithoutGrant(t, svc, res.RunID, grant, `"questions"`)
	r, err := svc.store.LoadRun(context.Background(), res.RunID)
	if err != nil {
		t.Fatal(err)
	}
	in, err := svc.store.LoadInteraction(context.Background(), res.RunID, r.Checkpoint.InteractionID)
	if err != nil {
		t.Fatalf("LoadInteraction: %v", err)
	}
	if got, _ := in.Questions[store.ForgePublishTokenVar].(string); got != grant {
		t.Errorf("the interaction record holds %q under the grant's name, want the value a resume reads back", got)
	}
}
