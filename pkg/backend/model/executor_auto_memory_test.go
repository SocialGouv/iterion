package model

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/automemory"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/knowledge"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/memory"
)

// applyAutoMemory is what actually decides whether a node gets a MEMORY.md,
// so these tests exercise IT rather than the resolver it calls — a correct
// resolver wired to nothing is the failure mode worth catching.
func applyFor(t *testing.T, e *ClawExecutor, f backendFields, backend string, task *delegate.Task) func() {
	t.Helper()
	if e.logger == nil {
		e.logger = iterlog.Nop()
	}
	return e.applyAutoMemory(context.Background(), task, f, backend)
}

func TestApplyAutoMemory_OffLeavesNoDirectory(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())
	e := &ClawExecutor{botID: "tester"}
	task := delegate.Task{WorkDir: t.TempDir(), StoreDir: t.TempDir()}

	sync := applyFor(t, e, backendFields{id: "n"}, delegate.BackendClaudeCode, &task)
	defer sync()

	if task.AutoMemoryDir != "" {
		t.Errorf("default must be off, got AutoMemoryDir = %q", task.AutoMemoryDir)
	}
}

func TestApplyAutoMemory_OnMaterialisesADirectory(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())
	e := &ClawExecutor{botID: "tester", wfAutoMemory: "on"}
	task := delegate.Task{WorkDir: t.TempDir(), StoreDir: t.TempDir()}

	sync := applyFor(t, e, backendFields{id: "n"}, delegate.BackendClaudeCode, &task)
	defer sync()

	if task.AutoMemoryDir == "" {
		t.Fatal("auto_memory: on must hand the backend a directory")
	}
	if !filepath.IsAbs(task.AutoMemoryDir) {
		t.Errorf("the directory must be absolute (it is passed into a container): %q", task.AutoMemoryDir)
	}
	if info, err := os.Stat(task.AutoMemoryDir); err != nil || !info.IsDir() {
		t.Errorf("the directory must exist before the backend runs: %v", err)
	}
}

// A node's own `off` must beat the workflow's `on` — the per-node opt-out is
// the whole point of having the field on the node.
func TestApplyAutoMemory_NodeOverridesWorkflow(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())
	e := &ClawExecutor{botID: "tester", wfAutoMemory: "on"}
	task := delegate.Task{WorkDir: t.TempDir(), StoreDir: t.TempDir()}

	sync := applyFor(t, e, backendFields{id: "n", autoMemory: "off"}, delegate.BackendClaudeCode, &task)
	defer sync()

	if task.AutoMemoryDir != "" {
		t.Errorf("node auto_memory: off must win over the workflow, got %q", task.AutoMemoryDir)
	}
}

// Materialising a directory for a backend that ignores the field would write
// files nobody ever reads.
func TestApplyAutoMemory_SkipsBackendsThatIgnoreIt(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())
	for _, backend := range []string{delegate.BackendKimi, delegate.BackendGrok, delegate.BackendCodex} {
		t.Run(backend, func(t *testing.T) {
			e := &ClawExecutor{botID: "tester", wfAutoMemory: "on"}
			task := delegate.Task{WorkDir: t.TempDir(), StoreDir: t.TempDir()}
			sync := applyFor(t, e, backendFields{id: "n"}, backend, &task)
			defer sync()
			if task.AutoMemoryDir != "" {
				t.Errorf("%s does not consume MEMORY.md; got %q", backend, task.AutoMemoryDir)
			}
		})
	}
}

// The end-to-end property at the applyAutoMemory level. It hands RepoRoot in
// by hand, which is exactly why it is NOT sufficient on its own — see
// TestExecuteBackend_BracketsTheDispatch, which drives the real path and is
// what caught RepoRoot never being forwarded outside a `memory:` block.
func TestApplyAutoMemory_PersistsAcrossRuns(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())
	repo := t.TempDir()

	run := func(write string) string {
		e := &ClawExecutor{botID: "tester", wfAutoMemory: "on"}
		// A fresh WorkDir each time is the worktree/pod case: nothing but the
		// store connects the two runs.
		task := delegate.Task{WorkDir: t.TempDir(), RepoRoot: repo, StoreDir: t.TempDir()}
		sync := applyFor(t, e, backendFields{id: "n"}, delegate.BackendClaw, &task)
		if task.AutoMemoryDir == "" {
			t.Fatal("no directory materialised")
		}
		existing, _ := os.ReadFile(filepath.Join(task.AutoMemoryDir, "MEMORY.md"))
		if write != "" {
			if err := os.WriteFile(filepath.Join(task.AutoMemoryDir, "MEMORY.md"), []byte(write), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		sync()
		return string(existing)
	}

	if got := run("- learned: the flag is --force"); got != "" {
		t.Errorf("first run should start empty, got %q", got)
	}
	if got := run(""); !strings.Contains(got, "the flag is --force") {
		t.Errorf("second run did not see the first run's memory, got %q", got)
	}
}

// The space is keyed on the REPO, not the per-run workdir. Without this a
// `worktree: auto` bot fingerprints a fresh worktree every run and the memory
// is always empty — the very defect that makes each backend's own default
// unusable here.
func TestAutoMemorySpaceRef_KeyedOnRepoNotWorktree(t *testing.T) {
	e := &ClawExecutor{botID: "tester"}
	repo := t.TempDir()
	a := e.autoMemorySpaceRef(context.Background(), delegate.Task{WorkDir: t.TempDir(), RepoRoot: repo})
	b := e.autoMemorySpaceRef(context.Background(), delegate.Task{WorkDir: t.TempDir(), RepoRoot: repo})
	if a.ID() != b.ID() {
		t.Errorf("two worktrees of the same repo resolved to different spaces:\n%s\n%s", a.ID(), b.ID())
	}

	other := e.autoMemorySpaceRef(context.Background(), delegate.Task{WorkDir: t.TempDir(), RepoRoot: t.TempDir()})
	if other.ID() == a.ID() {
		t.Error("different repos must not share a memory space")
	}

	// A different bot must not read this bot's notes.
	elsewhere := (&ClawExecutor{botID: "other-bot"}).autoMemorySpaceRef(
		context.Background(), delegate.Task{WorkDir: t.TempDir(), RepoRoot: repo})
	if elsewhere.ID() == a.ID() {
		t.Error("bot visibility is not isolating: two bots share one space")
	}

	// The reserved name must be the one the mirror and `iterion memory` agree on.
	if a.Name != automemory.SpaceName {
		t.Errorf("space name = %q, want %q", a.Name, automemory.SpaceName)
	}
	// And it must not collide with an ordinary `memory:` scope of the same name.
	if a.ID() == memory.LegacyBotRef(repo, automemory.SpaceName).ID() {
		t.Error("the auto-memory space collides with a legacy memory: scope")
	}
}

// claw maintains MEMORY.md with ordinary file tools, so a node that restricts
// `tools:` must still be granted them — otherwise the prompt instructs the
// model to keep a file it cannot open.
func TestAssembleEffectiveTools_GrantsFileToolsForAutoMemory(t *testing.T) {
	e := &ClawExecutor{botID: "tester", wfAutoMemory: "on"}
	f := backendFields{id: "n", tools: []string{"bash"}}

	got := e.assembleEffectiveTools(f, delegate.BackendClaw, nil, false)
	for _, want := range []string{"read_file", "write_file", "list_files"} {
		if !slices.Contains(got, want) {
			t.Errorf("auto_memory: on must grant %q to a restricted claw node, got %v", want, got)
		}
	}

	// Off must not widen the node's tool set behind the author's back.
	off := (&ClawExecutor{botID: "tester"}).assembleEffectiveTools(f, delegate.BackendClaw, nil, false)
	if slices.Contains(off, "write_file") {
		t.Errorf("auto_memory off must not grant file tools, got %v", off)
	}
}

// autoMemStubBackend records the task it received and lets a test act as the agent
// would — writing into the directory it was handed.
type autoMemStubBackend struct {
	sawDir   string
	write    string
	readBack string
}

func (s *autoMemStubBackend) Execute(_ context.Context, task delegate.Task) (delegate.Result, error) {
	s.sawDir = task.AutoMemoryDir
	if task.AutoMemoryDir != "" {
		// Read while the directory is alive: it belongs to this node and is
		// removed as soon as the node's edits are folded back.
		body, _ := os.ReadFile(filepath.Join(task.AutoMemoryDir, "MEMORY.md"))
		s.readBack = string(body)
		if s.write != "" {
			_ = os.WriteFile(filepath.Join(task.AutoMemoryDir, "MEMORY.md"), []byte(s.write), 0o600)
		}
	}
	return delegate.Result{Output: map[string]any{"ok": true}}, nil
}

// The composition, not the parts: applyAutoMemory can be perfect and still
// reach no run if executeBackend stops calling it. This drives the real
// dispatch path — the backend sees the directory, and what it writes there is
// in the store once the node returns.
//
// It also states the CROSS-BACKEND claim the shared directory exists for: a
// claude_code node's notes are read by a later claw node of the same bot. Each
// backend's own auto-memory store would keep them apart.
//
// This is the test that caught task.RepoRoot only being forwarded for nodes
// declaring a `memory:` block — with it empty, the space keyed on the per-run
// WorkDir and every run started from an empty memory. The applyAutoMemory
// tests above missed it because they hand RepoRoot in themselves.
func TestExecuteBackend_BracketsTheDispatch(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())
	repo := t.TempDir()

	run := func(backend, write string) *autoMemStubBackend {
		stub := &autoMemStubBackend{write: write}
		reg := delegate.NewRegistry()
		reg.Register(backend, stub)
		e := &ClawExecutor{
			logger:          iterlog.Nop(),
			botID:           "tester",
			wfAutoMemory:    "on",
			backendRegistry: reg,
			defaultBackend:  backend,
			schemas:         map[string]*ir.Schema{},
		}
		e.SetRepoRoot(repo)
		e.SetWorkDir(t.TempDir()) // a fresh worktree each run

		node := &ir.AgentNode{}
		node.ID = "n"
		node.Backend = backend
		node.Model = "test-model" // pinned so the host-credential detector stays out of it

		if _, err := e.executeBackend(context.Background(), node, map[string]any{}); err != nil {
			t.Fatalf("executeBackend: %v", err)
		}
		return stub
	}

	first := run(delegate.BackendClaudeCode, "- learned: the flag is --force")
	if first.sawDir == "" {
		t.Fatal("the backend was never handed a memory directory — the bracket is not wired")
	}
	if first.readBack != "" {
		t.Errorf("the first run should start from an empty memory, saw %q", first.readBack)
	}

	// A different backend, in a different worktree: only the store connects them.
	second := run(delegate.BackendClaw, "")
	if !strings.Contains(second.readBack, "the flag is --force") {
		t.Errorf("a claw node did not see the claude_code node's memory: %q", second.readBack)
	}
}

// automemory spells the backend names literally so it stays a leaf the DSL
// compiler can import. This is what keeps that spelling honest.
func TestSupportsBackend_MatchesDelegateConstants(t *testing.T) {
	for _, backend := range []string{delegate.BackendClaudeCode, delegate.BackendClaw, delegate.BackendPi} {
		if !automemory.SupportsBackend(backend) {
			t.Errorf("automemory.SupportsBackend(%q) = false — the literal name drifted from the delegate constant", backend)
		}
	}
	for _, backend := range []string{delegate.BackendKimi, delegate.BackendGrok, delegate.BackendCodex} {
		if automemory.SupportsBackend(backend) {
			t.Errorf("automemory.SupportsBackend(%q) = true, but nothing wires MEMORY.md for it", backend)
		}
	}
	// claude_code has auto-memory of its own and is only pointed at the
	// directory; claw and pi have none, so the section iterion renders IS the
	// mechanism. Getting this backwards either describes it twice or not at all.
	if automemory.NeedsPromptSection(delegate.BackendClaudeCode) {
		t.Error("claude_code has native auto-memory — rendering the section too states it twice")
	}
	for _, backend := range []string{delegate.BackendClaw, delegate.BackendPi} {
		if !automemory.NeedsPromptSection(backend) {
			t.Errorf("%s has no auto-memory of its own — without the section the directory is never mentioned", backend)
		}
	}
}

// Parity across the three: every supported backend must actually be told
// where its memory is, by whichever channel it understands.
func TestApplyAutoMemory_EveryBackendIsToldWhereItsMemoryIs(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())
	for _, backend := range []string{delegate.BackendClaudeCode, delegate.BackendClaw, delegate.BackendPi} {
		t.Run(backend, func(t *testing.T) {
			e := &ClawExecutor{botID: "tester", wfAutoMemory: "on"}
			task := delegate.Task{WorkDir: t.TempDir(), StoreDir: t.TempDir()}
			sync := applyFor(t, e, backendFields{id: "n"}, backend, &task)
			defer sync()

			if task.AutoMemoryDir == "" {
				t.Fatal("no directory materialised")
			}
			if automemory.NeedsPromptSection(backend) {
				if !strings.Contains(task.AutoMemoryPrompt, task.AutoMemoryDir) {
					t.Errorf("%s needs the prompt section and it does not name the directory: %q",
						backend, task.AutoMemoryPrompt)
				}
				if !strings.Contains(task.AutoMemoryPrompt, "MEMORY.md") {
					t.Errorf("%s prompt section does not mention MEMORY.md: %q", backend, task.AutoMemoryPrompt)
				}
				// And it must actually reach the model.
				if !strings.Contains((delegate.Task{AutoMemoryPrompt: task.AutoMemoryPrompt}).BuildSystemPrompt(), "# Auto memory") {
					t.Error("the section is not appended by BuildSystemPrompt")
				}
			} else if task.AutoMemoryPrompt != "" {
				t.Errorf("%s has native auto-memory; the section would state it twice: %q",
					backend, task.AutoMemoryPrompt)
			}
		})
	}
}

// Parallel branches must not eat each other's notes.
//
// A Mirror owns its directory — Hydrate makes the directory match the space,
// deleting whatever else is there. When every branch of a fan_out_all shared
// one directory, the second branch's Hydrate deleted the note the first
// branch's agent had just written and not yet synced: the note never reached
// the store, and nothing warned. Reproduced end to end before the fix.
func TestApplyAutoMemory_ParallelBranchesKeepTheirNotes(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())
	repo, storeDir, workDir := t.TempDir(), t.TempDir(), t.TempDir()
	e := &ClawExecutor{logger: iterlog.Nop(), botID: "tester", wfAutoMemory: "on"}

	// Interleave exactly as two concurrent branches do: both hydrate, both
	// write, both sync. The first branch's write lands BEFORE the second
	// hydrates — the ordering that used to destroy it.
	newBranch := func(name string) (*delegate.Task, func()) {
		task := &delegate.Task{WorkDir: workDir, RepoRoot: repo, StoreDir: storeDir}
		sync := e.applyAutoMemory(context.Background(), task, backendFields{id: name}, delegate.BackendClaw)
		if task.AutoMemoryDir == "" {
			t.Fatalf("%s got no memory directory", name)
		}
		return task, sync
	}

	taskA, syncA := newBranch("a")
	if err := os.WriteFile(filepath.Join(taskA.AutoMemoryDir, "a.md"), []byte("A's work"), 0o600); err != nil {
		t.Fatal(err)
	}
	taskB, syncB := newBranch("b")
	if taskA.AutoMemoryDir == taskB.AutoMemoryDir {
		t.Fatalf("both branches share one mirror directory (%s) — each Hydrate deletes the other's work",
			taskA.AutoMemoryDir)
	}
	if err := os.WriteFile(filepath.Join(taskB.AutoMemoryDir, "b.md"), []byte("B's work"), 0o600); err != nil {
		t.Fatal(err)
	}
	syncA()
	syncB()

	// A third node reads what the run actually banked.
	taskC, syncC := newBranch("c")
	defer syncC()
	for name, want := range map[string]string{"a.md": "A's work", "b.md": "B's work"} {
		got, err := os.ReadFile(filepath.Join(taskC.AutoMemoryDir, name))
		if err != nil {
			t.Errorf("%s never reached the store: %v", name, err)
			continue
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

// The per-node directory is a cache of the space, not state. Leaving one
// behind per node per run would accumulate under a root shared by every run
// on the host.
func TestApplyAutoMemory_MirrorDirIsRemovedAfterSync(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())
	e := &ClawExecutor{logger: iterlog.Nop(), botID: "tester", wfAutoMemory: "on"}
	task := &delegate.Task{WorkDir: t.TempDir(), RepoRoot: t.TempDir(), StoreDir: t.TempDir()}

	sync := e.applyAutoMemory(context.Background(), task, backendFields{id: "n"}, delegate.BackendClaw)
	dir := task.AutoMemoryDir
	if dir == "" {
		t.Fatal("no directory materialised")
	}
	sync()
	if _, err := os.Stat(dir); err == nil {
		t.Errorf("the mirror directory outlived the node: %s", dir)
	}
}

// A target repo commits <WorkDir>/.iterion/auto-memory/<digest> as a symlink.
// The digest is derivable from public facts (repo path + bot id).
func TestApplyAutoMemory_RefusesASymlinkedSpaceDir(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())
	work := t.TempDir()
	target := t.TempDir() // where the attacker points

	e := &ClawExecutor{logger: iterlog.Nop(), botID: "victim-bot", wfAutoMemory: "on"}
	task := delegate.Task{WorkDir: work, RepoRoot: work}

	// Learn the digest the executor will use, then plant the symlink.
	ref := e.autoMemorySpaceRef(context.Background(), task)
	root, _ := task.StateDir("auto-memory")
	digest := knowledge.ChecksumHex([]byte(ref.ID()))[:16]
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, digest)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	fresh := &ClawExecutor{logger: iterlog.Nop(), botID: "victim-bot", wfAutoMemory: "on"}
	tk := delegate.Task{WorkDir: work, RepoRoot: work}
	sync := fresh.applyAutoMemory(context.Background(), &tk, backendFields{id: "n"}, delegate.BackendClaw)
	defer sync()

	entries, _ := os.ReadDir(target)
	if len(entries) > 0 {
		t.Errorf("REDIRECTED: iterion wrote into the attacker's directory: %v", entries[0].Name())
	}
	if tk.AutoMemoryDir != "" {
		real, _ := filepath.EvalSymlinks(tk.AutoMemoryDir)
		realTarget, _ := filepath.EvalSymlinks(target)
		if len(real) >= len(realTarget) && real[:len(realTarget)] == realTarget {
			t.Errorf("the node's mirror resolved under the attacker's path: %s", real)
		}
	}
}

// ctxAwareStore refuses writes on a cancelled context, the way the Mongo
// driver does. The FS store ignores ctx entirely, which is why no local test
// sees this.
type ctxAwareStore struct{ knowledge.MemoryStore }

func (s ctxAwareStore) WriteDocument(ctx context.Context, ref knowledge.SpaceRef, in knowledge.DocumentInput) (knowledge.DocumentMeta, error) {
	if err := ctx.Err(); err != nil {
		return knowledge.DocumentMeta{}, err
	}
	return s.MemoryStore.WriteDocument(ctx, ref, in)
}

// A run that ends early is exactly when its notes matter most: an operator
// Cancel, a runner drain, a timeout all cancel the node's context, and the
// cloud store honours cancellation. Syncing on that context discarded
// everything the agent had written — invisible locally, because the
// filesystem store ignores the context entirely.
func TestApplyAutoMemory_SurvivesACancelledRun(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())
	e := &ClawExecutor{logger: iterlog.Nop(), botID: "tester", wfAutoMemory: "on",
		autoMemStore: ctxAwareStore{memory.DefaultFSStore()}}
	repo := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	task := delegate.Task{WorkDir: t.TempDir(), RepoRoot: repo, StoreDir: t.TempDir()}
	sync := e.applyAutoMemory(ctx, &task, backendFields{id: "n"}, delegate.BackendClaw)
	if err := os.WriteFile(filepath.Join(task.AutoMemoryDir, "MEMORY.md"), []byte("- hard-won"), 0o600); err != nil {
		t.Fatal(err)
	}
	cancel() // operator hits Cancel / the pod drains / the timeout fires
	sync()

	// A later run must still see the note.
	e2 := &ClawExecutor{logger: iterlog.Nop(), botID: "tester", wfAutoMemory: "on",
		autoMemStore: ctxAwareStore{memory.DefaultFSStore()}}
	t2 := delegate.Task{WorkDir: t.TempDir(), RepoRoot: repo, StoreDir: t.TempDir()}
	s2 := e2.applyAutoMemory(context.Background(), &t2, backendFields{id: "n"}, delegate.BackendClaw)
	defer s2()
	if _, err := os.Stat(filepath.Join(t2.AutoMemoryDir, "MEMORY.md")); err != nil {
		t.Errorf("MEMORY LOST on a cancelled run: %v", err)
	}
}
