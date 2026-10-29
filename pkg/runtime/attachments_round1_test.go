package runtime

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Round 1 of the adversarial loop on #2086, the runtime side.

// copyPods starts pods like podDriver, on a driver that copies host
// directories in instead of binding them: the kubernetes shape.
type copyPods struct {
	*podDriver
	seeds []sandbox.SeedDir
}

func (d *copyPods) Capabilities() sandbox.Capabilities {
	return sandbox.Capabilities{SupportsImage: true, SupportsMounts: true, SupportsHostDirCopy: true}
}

func (d *copyPods) Start(ctx context.Context, prepared sandbox.PreparedSpec, info sandbox.RunInfo) (sandbox.Run, error) {
	d.seeds = info.SeedDirs
	return d.podDriver.Start(ctx, prepared, info)
}

func attachedRun(t *testing.T, s store.RunStore, runID string) store.AttachmentRecord {
	t.Helper()
	ctx := context.Background()
	if _, err := s.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteAttachment(ctx, runID, store.AttachmentRecord{Name: "spec", OriginalFilename: "spec.md", MIME: "text/markdown"}, strings.NewReader("# spec")); err != nil {
		t.Fatal(err)
	}
	recs, err := s.ListAttachments(ctx, runID)
	if err != nil || len(recs) != 1 {
		t.Fatalf("ListAttachments = %v, %v", recs, err)
	}
	return recs[0]
}

// A copying driver that STARTS hands the nodes the container path — the
// landed directory is a fact, like a bind's — on a store with a root and on
// one without (materialised, then seeded).
func TestACopyingDriverHandsNodesTheContainerPath(t *testing.T) {
	t.Setenv("ITERION_MODE", "local")
	t.Setenv("ITERION_HOME", t.TempDir())
	fs := tmpStore(t)
	for _, tc := range []struct {
		name  string
		store store.RunStore
	}{{"a store with a root", fs}, {"a store with no root", noRootStore{fs}}} {
		t.Run(tc.name, func(t *testing.T) {
			rec := attachedRun(t, fs, "run-copy-"+strings.ReplaceAll(tc.name, " ", "-"))
			d := &copyPods{podDriver: &podDriver{root: t.TempDir()}}
			e := New(scratchWorkflow(), tc.store, newStubExecutor(), WithLogger(iterlog.Nop()), WithWorkDir(t.TempDir()),
				WithSandboxDrivers(map[string]sandbox.DriverConstructor{"docker": func() (sandbox.Driver, error) { return d, nil }}))
			cleanup, err := e.startSandbox(context.Background(), "run-copy-"+strings.ReplaceAll(tc.name, " ", "-"), e.workDir, "", nil)
			if err != nil {
				t.Fatalf("startSandbox: %v", err)
			}
			defer cleanup()
			if len(d.seeds) != 1 || d.seeds[0].ContainerPath != attachmentsContainerPath || !d.seeds[0].ReadOnly {
				t.Fatalf("Start was handed seeds %+v", d.seeds)
			}
			if e.attachmentsContainerDir != attachmentsContainerPath {
				t.Fatalf("the landed directory is %q, want %q", e.attachmentsContainerDir, attachmentsContainerPath)
			}
			if p := e.attachmentPath(rec); p != attachmentsContainerPath+"/spec/spec.md" {
				t.Fatalf("attachmentPath = %q, want the container path", p)
			}
		})
	}
}

// The drop event, on a copying driver, does not list the directory a seed
// carries; it says so under `carried`.
func TestTheDropEventDoesNotListWhatASeedCarries(t *testing.T) {
	p := canaryParams(t)
	attachTo(t, p.AttachmentsHostDir, "spec", "spec.md")
	events, err := startSandboxOn(t, &seedCapturingDriver{copy: true}, p)
	if !errors.Is(err, errSeedStop) {
		t.Fatalf("err = %v", err)
	}
	dropped := capturedEventsOf(events, store.EventSandboxHostBindsDropped)
	if len(dropped) != 1 {
		t.Fatalf("sandbox_host_binds_dropped emitted %d times, want 1", len(dropped))
	}
	if want := []string{"/iterion/artifact-files"}; !reflect.DeepEqual(dropped[0]["mounts"], want) {
		t.Fatalf("the drop event lists %v, want %v (the attachments are carried)", dropped[0]["mounts"], want)
	}
	if want := []string{attachmentsContainerPath}; !reflect.DeepEqual(dropped[0]["carried"], want) {
		t.Fatalf("the drop event carries %v, want %v", dropped[0]["carried"], want)
	}
}

// A bind's target under docker's other spellings is still a dropped target.
func TestADroppedBindIsReportedUnderEverySpellingOfItsTarget(t *testing.T) {
	for _, mount := range []string{
		"type=bind,source=/host/x,dst=/opt/x",
		"type=bind,source=/host/x,destination=/opt/x",
	} {
		spec := &sandbox.Spec{Mounts: []string{mount}}
		report := dropUnsupportedHostBinds(spec, "", nil)
		if !reflect.DeepEqual(report.Targets, []string{"/opt/x"}) {
			t.Fatalf("%q: report targets %v, want [/opt/x]", mount, report.Targets)
		}
	}
}

// In a claw prompt an image attachment is an image block, served by URL
// when no copy exists: not a path, not refused. A CLI backend reads the
// path itself, and a tool's command is a path wherever it runs.
func TestAnImageAttachmentInAClawPromptNeedsNoPath(t *testing.T) {
	w := &ir.Workflow{
		Attachments: map[string]*ir.Attachment{"logo": {Name: "logo", Type: ir.AttachmentImage}},
		Prompts:     map[string]*ir.Prompt{"see": {Name: "see", Body: "look {{attachments.logo}}", TemplateRefs: refsOf(t, "look {{attachments.logo}}")}},
	}
	td := &model.TemplateData{Attachments: map[string]model.AttachmentInfo{"logo": {Name: "logo", Path: ""}}}
	agent := func(backend string) *ir.AgentNode {
		n := &ir.AgentNode{BaseNode: ir.BaseNode{ID: "ask"}}
		n.UserPrompt = "see"
		n.Backend = backend
		return n
	}
	if err := admitAttachmentPaths(agent("claw"), w, td, nil); err != nil {
		t.Fatalf("a claw prompt naming an image attachment was refused: %v", err)
	}
	if err := admitAttachmentPaths(agent("claude_code"), w, td, nil); err == nil {
		t.Fatal("a CLI backend's prompt naming an image attachment with no path was admitted")
	}
	tool := &ir.ToolNode{BaseNode: ir.BaseNode{ID: "scan"}, CommandRefs: refsOf(t, "cat {{attachments.logo}}")}
	if err := admitAttachmentPaths(tool, w, td, nil); err == nil {
		t.Fatal("a tool naming an image attachment's path with no path was admitted")
	}
}

// A sandbox that fails to start after the materialisation takes the
// directory with it.
func TestAFailedSandboxStartRemovesTheMaterialisedDirectory(t *testing.T) {
	t.Setenv("ITERION_MODE", "local")
	fs := tmpStore(t)
	// The materialised directory lands under TMPDIR: a directory of this
	// test's, so its absence after the failed start is read on the disk.
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	attachedRun(t, fs, "run-failed-start")
	d := &specCapturingDriver{hostBinds: true}
	e := New(scratchWorkflow(), noRootStore{fs}, newStubExecutor(), WithLogger(iterlog.Nop()), WithWorkDir(t.TempDir()),
		WithSandboxDrivers(map[string]sandbox.DriverConstructor{"docker": func() (sandbox.Driver, error) { return d, nil }}))
	_, err := e.startSandbox(context.Background(), "run-failed-start", e.workDir, "", nil)
	if !errors.Is(err, errCanaryStop) {
		t.Fatalf("err = %v, want the driver's refusal at Prepare", err)
	}
	if e.attachmentsLocalDir != "" {
		t.Fatalf("the materialised directory %q is still announced after the failed start", e.attachmentsLocalDir)
	}
	if !strings.Contains(strings.Join(d.captured.Mounts, " "), "source="+tmp+string(os.PathSeparator)+"iterion-attachments-") {
		t.Fatalf("no materialised directory under %s was bound — the disk check below would be vacuous: %v", tmp, d.captured.Mounts)
	}
	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "iterion-attachments-") {
			t.Fatalf("the materialised directory %s survived the failed start on disk", entry.Name())
		}
	}
}

// The store did not hand the attachments over: the run parks resumable
// under its own code, not the driver's refusal.
func TestAnUnfetchableAttachmentParksTheRunResumable(t *testing.T) {
	cause := attachmentsUnfetchable("run-x", "fetch attachment spec", errors.New("the bucket is unreachable"))
	status, msg, code := setupFailureStatus(context.Background(), "sandbox start", cause)
	if status != store.RunStatusFailedResumable || code != store.FailureAttachmentsUnfetchable {
		t.Fatalf("status %s code %s, want failed_resumable %s", status, code, store.FailureAttachmentsUnfetchable)
	}
	if strings.Count(msg, "the bucket is unreachable") != 1 {
		t.Fatalf("the cause is said %d times in %q, want once", strings.Count(msg, "the bucket is unreachable"), msg)
	}
	if setupFailureRemedy(cause, code).Hint == "" {
		t.Fatal("the resumable failure carries no remedy")
	}
}
