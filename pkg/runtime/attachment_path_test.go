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

	// A workflow that opts into a sandbox by image, which
	// resolveSandboxSpec resolves as active without touching a daemon.
	e := &Engine{
		store: s,
		workflow: &ir.Workflow{
			Sandbox: &ir.SandboxSpec{Mode: "inline", Image: "ghcr.io/example/img:tag"},
		},
	}
	if !e.sandboxWillBeActive() {
		t.Fatal("test fixture does not resolve to an active sandbox; adjust the spec")
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
