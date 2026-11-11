package runview

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The in-process twin of the cloud runner's subbot fold: a child is a run
// of its own bot, the routing policy its root run froze at launch folds
// again for the CHILD's bot through the launch's fold and screens, and the
// child's executor dispatches with that fold. The observable is the child's
// dispatch: its agent `act` runs claw on claude-sonnet-4-5, and a cell
// pinning it to openai/gpt-6-sol makes the offline claw leg refuse on
// gpt-6-sol instead (no credential, no network).

const subbotFoldParent = `schema vout:
  ok: bool

subbot run_child:
  source: "../child/main.bot"
  output: vout

workflow parent:
  entry: run_child
  run_child -> done
`

const subbotFoldMid = `schema vout:
  ok: bool

subbot run_gc:
  source: "../grandchild/main.bot"
  output: vout

workflow mid:
  entry: run_gc
  run_gc -> done
`

const subbotFoldAgent = `schema empty:
  ok: bool

agent act:
  model: "claude-sonnet-4-5"
  output: empty

workflow child:
  entry: act
  act -> done
`

// runInProcessSubbotFold runs the parent's subbot node once, under a root
// run whose document froze pol, and returns the child's error.
func runInProcessSubbotFold(t *testing.T, childDir, childBot string, extra map[string]string, pol *store.RunLLMRoutePolicy, stamped *[]store.RunModelOverride) error {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("ITERION_ORPHAN_RECONCILE_INTERVAL", "0")
	t.Setenv("OPENAI_API_KEY", "")
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "parent"), "main.bot", subbotFoldParent)
	writeFile(t, filepath.Join(dir, childDir), "main.bot", childBot)
	for sub, body := range extra {
		writeFile(t, filepath.Join(dir, sub), "main.bot", body)
	}
	svc, err := NewService(filepath.Join(dir, "store"), WithLogger(iterlog.Nop()), WithWorkDir(dir), WithSandboxDefault("none"))
	if err != nil {
		t.Fatal(err)
	}
	defer stopService(t, svc)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const rootID = "run-parent"
	if _, err := svc.store.CreateRun(ctx, rootID, "parent", nil); err != nil {
		t.Fatal(err)
	}
	root, err := svc.store.LoadRun(ctx, rootID)
	if err != nil {
		t.Fatal(err)
	}
	root.LLMRoutePolicy = pol
	if err := svc.store.SaveRun(ctx, root); err != nil {
		t.Fatal(err)
	}
	_, runErr := svc.subbotRunnerFor(filepath.Join(dir, "parent", "main.bot"), iterlog.Nop())(ctx, runtime.SubbotRequest{
		Source: "../" + childDir + "/main.bot", ParentRunID: rootID, NodeID: "run_child", ReattachKey: "run_child", WorkDir: dir,
	})
	if runErr == nil {
		t.Fatal("precondition: the child's claw leg fails offline (no credential)")
	}
	if stamped != nil {
		*stamped = childDocRows(t, svc, rootID, "run_child")
	}
	return runErr
}

// childDocRows reads the override rows the child recorded under key
// stamped on its document; nil when no child document exists.
func childDocRows(t *testing.T, svc *Service, parentID, key string) []store.RunModelOverride {
	t.Helper()
	parent, err := svc.store.LoadRun(context.Background(), parentID)
	if err != nil {
		t.Fatal(err)
	}
	childID := parent.SubbotChildren[key]
	if childID == "" {
		return nil
	}
	child, err := svc.store.LoadRun(context.Background(), childID)
	if err != nil {
		t.Fatal(err)
	}
	return child.ModelOverrides
}

// dispatchedGPT reports whether the child's agent dispatched on the cell's
// model rather than its authored one.
func dispatchedGPT(t *testing.T, err error) bool {
	t.Helper()
	gpt, sonnet := strings.Contains(err.Error(), "gpt-6-sol"), strings.Contains(err.Error(), "claude-sonnet-4-5")
	if gpt == sonnet {
		t.Fatalf("precondition: the child's error names exactly one of gpt-6-sol and claude-sonnet-4-5: %v", err)
	}
	return gpt
}

func TestInProcessSubbotChildFoldsTheRoutingPolicyForItsOwnBot(t *testing.T) {
	for _, tc := range []struct {
		sel   string
		reach bool
	}{
		{"parent/act", false},
		{"child/act", true},
		{"act", true},
		{"*/act", true},
	} {
		t.Run(tc.sel, func(t *testing.T) {
			pol := &store.RunLLMRoutePolicy{Overrides: []store.RunLLMRouteOverride{{Selector: tc.sel, Model: "openai/gpt-6-sol"}}}
			err := runInProcessSubbotFold(t, "child", subbotFoldAgent, nil, pol, nil)
			if got := dispatchedGPT(t, err); got != tc.reach {
				t.Fatalf("cell %q reached the child bot = %v, want %v (child error: %v)", tc.sel, got, tc.reach, err)
			}
		})
	}
}

// A grandchild folds the ROOT run's snapshot: the middle child's document
// carries no policy.
func TestInProcessSubbotGrandchildFoldsTheRootRunsPolicy(t *testing.T) {
	grandchild := map[string]string{"grandchild": subbotFoldAgent}
	for _, tc := range []struct {
		sel   string
		reach bool
	}{
		{"grandchild/act", true},
		{"mid/act", false},
	} {
		t.Run(tc.sel, func(t *testing.T) {
			pol := &store.RunLLMRoutePolicy{Overrides: []store.RunLLMRouteOverride{{Selector: tc.sel, Model: "openai/gpt-6-sol"}}}
			err := runInProcessSubbotFold(t, "mid", subbotFoldMid, grandchild, pol, nil)
			if got := dispatchedGPT(t, err); got != tc.reach {
				t.Fatalf("cell %q reached the grandchild bot = %v, want %v (error: %v)", tc.sel, got, tc.reach, err)
			}
		})
	}
}

// The child's document stamps the rows its executor runs with, so the
// studio shows them and a resume of the child replays them
// (resumeExecutorSpec reads the document's rows).
func TestInProcessSubbotChildDocumentStampsItsFoldedRows(t *testing.T) {
	pol := &store.RunLLMRoutePolicy{Overrides: []store.RunLLMRouteOverride{{Selector: "child/act", Model: "openai/gpt-6-sol"}}}
	var stamped []store.RunModelOverride
	_ = runInProcessSubbotFold(t, "child", subbotFoldAgent, nil, pol, &stamped)
	want := []store.RunModelOverride{{Selector: "act", Model: "openai/gpt-6-sol"}}
	if !reflect.DeepEqual(stamped, want) {
		t.Fatalf("child document rows = %+v, want its fold %+v", stamped, want)
	}
}
