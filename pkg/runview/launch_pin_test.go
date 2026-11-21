package runview

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/internal/proc"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// #3416. A launch pinned to a program (LaunchSpec.ExpectProgramHash) is held
// to it where the launch is published with the program its compile read: the
// publisher lane, witnessed through the server's handlers
// (pkg/server/bots_launch_source_test.go). Every lane a local process takes
// reads the bot again after accepting the launch, and refuses a pin by name —
// one witness per lane here, each with the RIGHT pin, so the refusal is the
// lane's and not a comparison's.

const pinnedBot = "workflow pinned:\n  entry: done\n"

func newPinService(t *testing.T, opts ...ServiceOption) (*Service, store.RunStore, string) {
	t.Helper()
	dir := t.TempDir()
	botPath := filepath.Join(dir, "pinned.bot")
	if err := os.WriteFile(botPath, []byte(pinnedBot), 0o600); err != nil {
		t.Fatalf("write bot: %v", err)
	}
	st, err := store.New(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	svc, err := NewService(dir, append([]ServiceOption{WithLogger(iterlog.Nop()), WithStore(st), WithWorkDir(gittest.SourceRepo(t))}, opts...)...)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		svc.Stop(ctx)
	})
	return svc, st, botPath
}

// refusesThePin launches botPath under its own identity and wants the lane's
// refusal: typed, claiming no run, leaving no run document.
func refusesThePin(t *testing.T, svc *Service, st store.RunStore, botPath, runID string) {
	t.Helper()
	pin, err := LaunchProgramHash(botPath, "", "", "")
	if err != nil {
		t.Fatalf("LaunchProgramHash: %v", err)
	}
	ctx := context.Background()
	_, err = svc.Launch(ctx, LaunchSpec{FilePath: botPath, RunID: runID, ExpectProgramHash: pin})
	if !errors.Is(err, ErrLaunchPinUnsupported) {
		t.Fatalf("a launch pinned to the file's own program = %v, want ErrLaunchPinUnsupported", err)
	}
	if RunMayHaveStarted(err) {
		t.Fatalf("the refusal claims a run may exist: %v — the caller would keep its admission", err)
	}
	if _, lerr := st.LoadRun(ctx, runID); lerr == nil {
		t.Fatalf("the refused launch left a run document behind")
	}

	// Before any compile: a file that does not compile, pinned, draws the
	// lane's refusal too. A compile error in its place would say the bot was
	// read before the pin was refused.
	broken := filepath.Join(filepath.Dir(botPath), "broken.bot")
	if err := os.WriteFile(broken, []byte("workflow broken:\n  entry: nowhere\n  nowhere -> gone\n"), 0o600); err != nil {
		t.Fatalf("write the broken bot: %v", err)
	}
	if _, cerr := LaunchProgramHash(broken, "", "", ""); cerr == nil {
		t.Fatal("premise: the broken bot compiles")
	}
	_, err = svc.Launch(ctx, LaunchSpec{FilePath: broken, RunID: runID + "-broken", ExpectProgramHash: pin})
	if !errors.Is(err, ErrLaunchPinUnsupported) {
		t.Fatalf("a pinned launch of a file that does not compile = %v, want ErrLaunchPinUnsupported before anything of it is read", err)
	}
}

func TestLaunch_TheInProcessLaneRefusesAPin(t *testing.T) {
	t.Setenv(envDetached, "0")
	svc, st, botPath := newPinService(t)
	refusesThePin(t, svc, st, botPath, "pinned-in-process")

	// No pin, nothing refused: the same file launches on this lane.
	res, err := svc.Launch(context.Background(), LaunchSpec{FilePath: botPath, RunID: "unpinned"})
	if err != nil {
		t.Fatalf("launch with no pin: %v", err)
	}
	<-res.Done
}

// The detached lane hands the launch to a runner that compiles its own copy
// of the file: the refusal comes before the pre-flight compile, the run
// document and the spawn.
func TestLaunch_TheDetachedLaneRefusesAPin(t *testing.T) {
	svc, st, botPath := newPinService(t)
	t.Setenv(envDetached, "1")
	// No runner binary resolves on any host: nothing this witness does may
	// reach a spawn.
	t.Cleanup(proc.SetInstallPathCandidates(func() []string { return nil }))
	t.Setenv("ITERION_BIN", "")
	t.Setenv("PATH", t.TempDir())
	if !DetachedEnabled() {
		t.Fatal("premise: the detached lane is not the one a launch takes")
	}
	refusesThePin(t, svc, st, botPath, "pinned-detached")
}

// A launch over the pipeline cap is parked and compiled when its slot frees,
// or from a spec rebuilt without its pin after a restart: the refusal comes
// before the queue takes it.
func TestLaunch_TheParkedLaneRefusesAPin(t *testing.T) {
	t.Setenv(envDetached, "0")
	svc, st, botPath := newPinService(t, WithMaxConcurrentPipelines(1))
	if admitted, _ := svc.pipelineQueue.admitOrEnqueue("occupier", LaunchSpec{}); !admitted {
		t.Fatal("premise: the occupier did not take the only slot")
	}
	refusesThePin(t, svc, st, botPath, "pinned-parked")
	if _, parked := svc.pipelineQueue.positions()["pinned-parked"]; parked {
		t.Fatal("the refused launch is parked in the pipeline queue")
	}

	// The lane is the parked one: the same launch with no pin waits there.
	res, err := svc.Launch(context.Background(), LaunchSpec{FilePath: botPath, RunID: "unpinned-parked"})
	if err != nil {
		t.Fatalf("launch with no pin over the cap: %v", err)
	}
	if res.QueuePosition == 0 {
		t.Fatalf("premise: a launch over the cap was not parked: %+v", res)
	}
}

// The identity a pin names is the program's, not the entry file's: a bundle's
// preset and a fragment move it, the path it is read under does not.
func TestLaunchProgramHash_IsTheIdentityOfTheWholeProgram(t *testing.T) {
	write := func(t *testing.T, root string, files map[string]string) string {
		t.Helper()
		for rel, body := range files {
			p := filepath.Join(root, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return root
	}
	hashOf := func(t *testing.T, files map[string]string) string {
		t.Helper()
		dir := write(t, t.TempDir(), files)
		// The cloud shape: the entry inline, the bundle materialized.
		h, err := LaunchProgramHash("bots/x/main.bot", files["main.bot"], dir, "")
		if err != nil {
			t.Fatalf("LaunchProgramHash: %v", err)
		}
		return h
	}
	main := "import \"lib/nodes.bot\"\n\nworkflow w:\n  entry: done\n"
	base := map[string]string{
		"manifest.yaml":   "name: x\n",
		"main.bot":        main,
		"lib/nodes.bot":   "prompt p:\n  hello\n",
		"presets/prod.md": "---\nvars: {}\n---\nfocus\n",
	}
	with := func(rel, body string) map[string]string {
		out := map[string]string{}
		for k, v := range base {
			out[k] = v
		}
		out[rel] = body
		return out
	}
	h := hashOf(t, base)
	if again := hashOf(t, base); again != h {
		t.Fatalf("the same program materialized twice hashes %q then %q", h, again)
	}
	if moved := hashOf(t, with("lib/nodes.bot", "prompt p:\n  goodbye\n")); moved == h {
		t.Fatalf("a fragment's edit did not move the identity")
	}
	if moved := hashOf(t, with("presets/prod.md", "---\nvars: {}\n---\nanother focus\n")); moved == h {
		t.Fatalf("a preset file's edit did not move the identity")
	}
}
