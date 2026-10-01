package runview

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A resumed run's executor spec carries no launch vars; its secret guard still
// learns the grant the run's tools are rendered with, from the run record.
func TestRecordedMintedSecretsReadsTheGrantFromTheRecordOnResume(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const token = "sentinel-publish-token-1997-resume"
	if _, err := st.CreateRun(context.Background(), "run-1", "wf", map[string]any{store.ForgePublishTokenVar: token, "pr_url": "https://x/pull/1"}); err != nil {
		t.Fatal(err)
	}
	got := recordedMintedSecrets(context.Background(), ExecutorSpec{Store: st, RunID: "run-1"})
	if !slices.Equal(got[store.ForgePublishTokenVar], []string{token}) {
		t.Fatalf("recordedMintedSecrets = %v, want the record's minted token", got)
	}
	if _, leaked := got["pr_url"]; leaked {
		t.Errorf("recordedMintedSecrets copied %q from the record; only minted secrets belong to the guard", "pr_url")
	}

	same := map[string]string{store.ForgePublishTokenVar: token}
	if got := recordedMintedSecrets(context.Background(), ExecutorSpec{Store: st, RunID: "run-1", Vars: same}); len(got) != 0 {
		t.Errorf("recordedMintedSecrets = %v, want nothing: the launch vars carry the record's grant", got)
	}
	other := map[string]string{store.ForgePublishTokenVar: "from-launch"}
	if got := recordedMintedSecrets(context.Background(), ExecutorSpec{Store: st, RunID: "run-1", Vars: other}); !slices.Equal(got[store.ForgePublishTokenVar], []string{token}) {
		t.Errorf("recordedMintedSecrets = %v, want the record's grant beside the launch's other value", got)
	}
}

// A fresh launch builds its executor before the engine records the run: the
// missing record is not a failure worth a warning on every run.
func TestRecordedMintedSecretsIsQuietOnARunNotYetRecorded(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	got := recordedMintedSecrets(context.Background(), ExecutorSpec{Store: st, RunID: "not-created-yet", Vars: map[string]string{"pr_url": "https://x/pull/1"}, Logger: iterlog.New(iterlog.LevelDebug, &buf)})
	if len(got) != 0 {
		t.Errorf("recordedMintedSecrets = %v, want nothing", got)
	}
	if buf.Len() != 0 {
		t.Errorf("recordedMintedSecrets logged on a run the engine has not recorded yet: %s", buf.String())
	}
}

// A sub-bot child's record does not exist when its executor is built, and it
// may receive the grant under another name: its guard knows the parent's
// grant by value.
func TestRecordedMintedSecretsReadsTheParentsGrantForASubbotChild(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const token = "sentinel-publish-token-1997-parent"
	if _, err := st.CreateRun(context.Background(), "run-parent", "wf", map[string]any{store.ForgePublishTokenVar: token}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	got := recordedMintedSecrets(context.Background(), ExecutorSpec{Store: st, RunID: "run-child", ParentRunID: "run-parent", Logger: iterlog.New(iterlog.LevelDebug, &buf)})
	if !slices.Equal(got[store.ForgePublishTokenVar], []string{token}) {
		t.Fatalf("recordedMintedSecrets = %v, want the parent's grant", got)
	}
	if buf.Len() != 0 {
		t.Errorf("recordedMintedSecrets logged on a child whose record does not exist yet: %s", buf.String())
	}
}

// Only the run that was launched with the grant holds it under the minted
// name; every descendant holds it under the name its `with:` chose, and a
// resume or a nested sub-bot names only its own record or its parent's. The
// guard knows every grant of the lineage by value.
func TestRecordedMintedSecretsWalksTheWholeLineage(t *testing.T) {
	ctx := context.Background()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const grant = "sentinel-publish-token-1997-root"
	if _, err := st.CreateRun(ctx, "run-root", "root", map[string]any{store.ForgePublishTokenVar: grant}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateChildRun(ctx, "run-mid", "mid", "run-root", map[string]any{"bearer1": grant}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateChildRun(ctx, "run-gc", "gc", "run-mid", map[string]any{"bearer2": grant}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		spec ExecutorSpec
	}{
		// A resume names the run alone: its parent is in its record.
		{"resumed child", ExecutorSpec{RunID: "run-mid"}},
		{"resumed grandchild", ExecutorSpec{RunID: "run-gc"}},
		// A nested sub-bot names its parent, which holds the grant under an alias.
		{"grandchild at launch", ExecutorSpec{RunID: "run-ggc", ParentRunID: "run-gc"}},
		// The minted name may carry another value while an alias carries the grant.
		{"minted name holding another value", ExecutorSpec{RunID: "run-gc2", ParentRunID: "run-mid", Vars: map[string]string{store.ForgePublishTokenVar: "decoy", "bearer2": grant}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.spec.Store = st
			got := recordedMintedSecrets(ctx, tc.spec)
			if !slices.Contains(got[store.ForgePublishTokenVar], grant) {
				t.Fatalf("recordedMintedSecrets = %v, want the root's grant", got)
			}
		})
	}
}

// cyclicRuns serves a ParentRunID chain that loops back on itself.
type cyclicRuns map[string]*store.Run

func (c cyclicRuns) LoadRun(_ context.Context, id string) (*store.Run, error) {
	if r, ok := c[id]; ok {
		return r, nil
	}
	return nil, store.ErrRunNotFound
}

// A corrupt lineage ends the walk rather than the process: every record of
// the cycle is read once.
func TestRecordedMintedSecretsEndsOnACyclicLineage(t *testing.T) {
	runs := cyclicRuns{
		"a": {ID: "a", ParentRunID: "b", Inputs: map[string]any{store.ForgePublishTokenVar: "grant-a"}},
		"b": {ID: "b", ParentRunID: "a", Inputs: map[string]any{store.ForgePublishTokenVar: "grant-b"}},
	}
	got := recordedMintedSecrets(context.Background(), ExecutorSpec{Runs: runs, RunID: "a"})
	if want := []string{"grant-a", "grant-b"}; !slices.Equal(got[store.ForgePublishTokenVar], want) {
		t.Fatalf("recordedMintedSecrets = %v, want %v", got, want)
	}
}

// eventsOnly is a store that records events and loads no runs.
type eventsOnly struct{}

func (eventsOnly) AppendEvent(_ context.Context, _ string, evt store.Event) (*store.Event, error) {
	return &evt, nil
}

// A store that cannot load runs, with no ExecutorSpec.Runs beside it, leaves a
// resumed run's grant unredacted: said, not silent.
func TestRecordedMintedSecretsWarnsWhenNoRunLoaderIsWired(t *testing.T) {
	var buf bytes.Buffer
	recordedMintedSecrets(context.Background(), ExecutorSpec{Store: eventsOnly{}, RunID: "run-1", Logger: iterlog.New(iterlog.LevelDebug, &buf)})
	if !strings.Contains(buf.String(), "ExecutorSpec.Runs is unset") {
		t.Fatalf("recordedMintedSecrets said nothing about a store that cannot load runs: %q", buf.String())
	}
}

const childDeclaringTheGrant = `vars:
  forge_publish_token: string = ""

secrets:
  g: "{{vars.forge_publish_token}}"

tool noop:
  command: "true"

workflow child:
  entry: noop
  noop -> done
`

// A sub-bot child its parent did not hand the grant to cannot obtain it by
// declaring a secret over the var: the parent's grant is redacted from the
// child's sinks, and no placeholder of the child's resolves to it.
func TestASubbotChildNeverDeclaresItsWayToTheParentsGrant(t *testing.T) {
	for _, raw := range os.Environ() {
		if name, _, _ := strings.Cut(raw, "="); store.IsSecretEnvName(name) {
			t.Setenv(name, "")
		}
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	grant := hex.EncodeToString(b)
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateRun(context.Background(), "run-parent", "parent", map[string]any{store.ForgePublishTokenVar: grant}); err != nil {
		t.Fatal(err)
	}
	res := ir.Compile(parser.Parse("child.bot", childDeclaringTheGrant).File)
	if res.Workflow == nil {
		t.Fatalf("compile: %v", res.Diagnostics)
	}
	exec, err := BuildExecutor(ExecutorSpec{
		Ctx: context.Background(), Store: st, RunID: "run-child", ParentRunID: "run-parent", Workflow: res.Workflow,
		Vars: map[string]string{}, WorkDir: t.TempDir(), StoreDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("BuildExecutor: %v", err)
	}
	if got := exec.MaterializeForHost("key="+secretguard.PlaceholderForName("g"), "collector.example"); strings.Contains(got, grant) {
		t.Fatal("the child's declared secret resolves to the parent's grant")
	}
	if got := exec.ScrubOutput(map[string]any{"out": "bearer " + grant}); strings.Contains(got["out"].(string), grant) {
		t.Error("the parent's grant is not redacted from the child's sinks")
	}
}
