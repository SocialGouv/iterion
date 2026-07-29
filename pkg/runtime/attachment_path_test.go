package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// writeTestAttachment persists one attachment on a run and returns its
// canonical record (StorageRef included).
func writeTestAttachment(t *testing.T, s store.RunStore, runID, name, filename, body string) store.AttachmentRecord {
	t.Helper()
	ctx := context.Background()
	rec := store.AttachmentRecord{Name: name, OriginalFilename: filename, MIME: "audio/mpeg"}
	if err := s.WriteAttachment(ctx, runID, rec, strings.NewReader(body)); err != nil {
		t.Fatalf("WriteAttachment(%s): %v", name, err)
	}
	list, err := s.ListAttachments(ctx, runID)
	if err != nil {
		t.Fatalf("ListAttachments: %v", err)
	}
	for _, a := range list {
		if a.Name == name {
			return a
		}
	}
	t.Fatalf("attachment %q not found after write", name)
	return store.AttachmentRecord{}
}

// Without a sandbox the agent runs on the host, so the host path is the
// correct one.
func TestAttachmentPathIsHostPathWithoutSandbox(t *testing.T) {
	s := tmpStore(t)
	ctx := context.Background()
	if _, err := s.CreateRun(ctx, "att-host", "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	rec := writeTestAttachment(t, s, "att-host", "gate.music", "track.mp3", "bytes")

	e := &Engine{store: s, workflow: &ir.Workflow{}}
	got := e.attachmentPath(rec)

	if !strings.HasPrefix(got, s.Root()) {
		t.Errorf("path = %q, want a path under the store root %q", got, s.Root())
	}
	if !strings.HasSuffix(got, "track.mp3") {
		t.Errorf("path = %q, want it to end at the file", got)
	}
	if strings.Contains(got, attachmentsContainerPath) {
		t.Errorf("path = %q, must not be the container path on a non-sandboxed run", got)
	}
}

// Under a sandbox the host path does not exist inside the container —
// handing it to an agent yields an ENOENT the agent then improvises
// around. The bind-mount path is the only openable one.
func TestAttachmentPathIsContainerPathUnderSandbox(t *testing.T) {
	s := tmpStore(t)
	ctx := context.Background()
	if _, err := s.CreateRun(ctx, "att-sandbox", "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	rec := writeTestAttachment(t, s, "att-sandbox", "gate.music", "track.mp3", "bytes")

	// The sandbox has booted and the attachments bind landed — the state
	// startSandbox records. Asserted through that recorded state rather
	// than through a spec, because the spec is exactly what must NOT
	// decide this (see the degraded-run test below).
	e := &Engine{
		store: s,
		workflow: &ir.Workflow{
			Sandbox: &ir.SandboxSpec{Mode: "inline", Image: "ghcr.io/example/img:tag"},
		},
		sandboxSettled:          true,
		attachmentsContainerDir: attachmentsContainerPath,
	}

	got := e.attachmentPath(rec)
	want := attachmentsContainerPath + "/gate.music/track.mp3"
	if got != want {
		t.Errorf("path = %q, want %q", got, want)
	}
	if strings.HasPrefix(got, s.Root()) {
		t.Errorf("path = %q leaks the host store root into the container", got)
	}
}

// A sandbox-by-default run on a host with no container runtime degrades
// to unsandboxed (resolveAndStartSandbox emits sandbox_skipped and
// returns no sandbox), and a driver that drops host bind mounts
// (kubernetes) leaves the attachments dir unmounted. The workflow spec
// still says "sandboxed" in both cases, so deciding from the spec hands
// every agent a container path the run cannot open. What actually
// happened must win.
func TestAttachmentPathFallsBackToHostWhenSandboxDegraded(t *testing.T) {
	s := tmpStore(t)
	ctx := context.Background()
	if _, err := s.CreateRun(ctx, "att-degraded", "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	rec := writeTestAttachment(t, s, "att-degraded", "gate.music", "track.mp3", "bytes")

	e := &Engine{
		store: s,
		workflow: &ir.Workflow{
			Sandbox: &ir.SandboxSpec{Mode: "inline", Image: "ghcr.io/example/img:tag"},
		},
		// startSandbox ran and produced no attachments mount.
		sandboxSettled:          true,
		attachmentsContainerDir: "",
	}

	got := e.attachmentPath(rec)
	if strings.Contains(got, attachmentsContainerPath) {
		t.Errorf("path = %q, want the host path: this run is NOT in a container", got)
	}
	if !strings.HasPrefix(got, s.Root()) || !strings.HasSuffix(got, "track.mp3") {
		t.Errorf("path = %q, want a host path under %q ending at the file", got, s.Root())
	}
}

// The engine fills in `path` on the descriptors the HTTP layer left
// path-less, for both the declared-field and the ad-hoc shapes.
func TestResolveFileAnswersFillsPaths(t *testing.T) {
	s := tmpStore(t)
	ctx := context.Background()
	if _, err := s.CreateRun(ctx, "att-answers", "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	writeTestAttachment(t, s, "att-answers", "gate.music", "track.mp3", "bytes")
	writeTestAttachment(t, s, "att-answers", "gate.attachment-1", "sketch.png", "png")

	e := &Engine{store: s, workflow: &ir.Workflow{}}
	answers := map[string]any{
		"approved": true,
		"music":    map[string]any{"attachment": "gate.music", "filename": "track.mp3"},
		answerUploadsKey: []any{
			map[string]any{"attachment": "gate.attachment-1", "filename": "sketch.png"},
		},
	}
	e.resolveFileAnswers(ctx, "att-answers", answers)

	music := answers["music"].(map[string]any)
	p, _ := music["path"].(string)
	if p == "" || !strings.HasSuffix(p, "track.mp3") {
		t.Errorf("music path = %q, want a resolved path ending in track.mp3", p)
	}
	adhoc := answers[answerUploadsKey].([]any)[0].(map[string]any)
	ap, _ := adhoc["path"].(string)
	if ap == "" || !strings.HasSuffix(ap, "sketch.png") {
		t.Errorf("ad-hoc path = %q, want a resolved path ending in sketch.png", ap)
	}
	if answers["approved"] != true {
		t.Error("non-file answers must pass through untouched")
	}
}

// A descriptor naming an attachment the run does not have must resolve
// to NO path — never to an arbitrary filesystem location derived from
// client-supplied text.
func TestResolveFileAnswersIgnoresUnknownAttachment(t *testing.T) {
	s := tmpStore(t)
	ctx := context.Background()
	if _, err := s.CreateRun(ctx, "att-unknown", "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	e := &Engine{store: s, workflow: &ir.Workflow{}}
	answers := map[string]any{
		"music": map[string]any{"attachment": "../../etc/passwd"},
	}
	e.resolveFileAnswers(ctx, "att-unknown", answers)

	if p, present := answers["music"].(map[string]any)["path"]; present {
		t.Errorf("path was resolved for an unknown attachment: %v", p)
	}
}

// {{attachments.<name>}} must resolve for every node a RESUMED run
// executes. rs.attachments was populated only by runInitState, on the
// launch path, so both resume paths left it empty and every
// `attachments.*` reference silently resolved to nothing. A gate upload
// exists ONLY after a resume, so that reference form — the one
// docs/human-in-the-loop.md documents — was dead on arrival, and
// launch-time attachments silently degraded on any resumed run.
func TestResumeRebuildStateLoadsAttachments(t *testing.T) {
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "att-resume"
	if _, err := s.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	writeTestAttachment(t, s, runID, "gate.music", "track.mp3", "bytes")

	r, err := s.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	r.WorkDir = t.TempDir()
	if err := s.SaveRun(ctx, r); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}

	e := New(&ir.Workflow{
		Name:    "wf",
		Entry:   "n",
		Nodes:   map[string]ir.Node{"n": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "n"}}},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}, s, newStubExecutor())

	cp := &store.Checkpoint{NodeID: "n"}
	rs, cleanup, rbErr := e.resumeRebuildState(ctx, r, cp, map[string]map[string]any{}, map[string]int{})
	if rbErr != nil {
		t.Fatalf("resumeRebuildState: %v", rbErr)
	}
	if cleanup != nil {
		defer cleanup()
	}

	info, ok := rs.attachments["gate.music"]
	if !ok {
		t.Fatalf("rs.attachments is missing the run's attachment; got %v", rs.attachments)
	}
	if info.Path == "" {
		t.Error("attachment info carries no path — nodes cannot open it")
	}
}

// The sandbox forecast behind attachmentPath reads e.repoRoot, which
// restoreRunEnv only sets later (inside resumeRebuildState). Left empty,
// the default `sandbox: auto` resolves to "not applicable (outside a git
// repository)" and gate uploads are stamped with a host path — moments
// before startSandbox, handed r.RepoRoot, containerises the run.
func TestSeedRepoRootForResume(t *testing.T) {
	t.Run("prefers the run's recorded repo root", func(t *testing.T) {
		e := &Engine{}
		e.seedRepoRootForResume(&store.Run{RepoRoot: "/repos/app", WorkDir: "/repos/app/wt"})
		if e.repoRoot != "/repos/app" {
			t.Errorf("repoRoot = %q, want /repos/app", e.repoRoot)
		}
	})

	t.Run("never overwrites an already-restored value", func(t *testing.T) {
		e := &Engine{repoRoot: "/already/set"}
		e.seedRepoRootForResume(&store.Run{RepoRoot: "/repos/other"})
		if e.repoRoot != "/already/set" {
			t.Errorf("repoRoot = %q, want the existing value untouched", e.repoRoot)
		}
	})

	t.Run("tolerates a run with neither", func(t *testing.T) {
		e := &Engine{}
		e.seedRepoRootForResume(&store.Run{})
		_ = e.repoRoot // derived from the workspace; only must not panic
	})
}
