package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

func TestAvailableReviewMediaListsSafePassiveAttachmentsFromRunFiles(t *testing.T) {
	s := tmpStore(t)
	ctx := context.Background()
	if _, err := s.CreateRun(ctx, "media-run", "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	rfs := store.AsRunFilesStore(s)
	dir, err := rfs.EnsureRunFilesDir(ctx, "media-run")
	if err != nil {
		t.Fatalf("EnsureRunFilesDir: %v", err)
	}
	for name, body := range map[string]string{
		"renders/cover.png":  "png",
		"audio/theme.mp3":    "mp3",
		"clip.webm":          "webm",
		"review/plan.json":   "{}",
		"review/brief.md":    "# Brief",
		"review/notes.txt":   "notes",
		"review/table.csv":   "a,b",
		"review/config.yaml": "key: value",
		"review/config.yml":  "key: value",
		"review/spec.pdf":    "%PDF",
		"active.svg":         "svg",
		"active.html":        "html",
		"archive.zip":        "zip",
	} {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", name, err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("WriteFile(%s): %v", name, err)
		}
	}
	// A symlink with a media extension must never enter the manifest.
	_ = os.Symlink(filepath.Join(dir, "notes.txt"), filepath.Join(dir, "linked.png"))

	eng := New(minimalReviewWorkflow(), s, newStubExecutor())
	rs := eng.newRunState("media-run", nil)
	rs.ctx = ctx
	got := eng.availableReviewMedia(rs)

	if len(got) != 10 {
		t.Fatalf("available attachments = %+v, want 10 passive preview files", got)
	}
	want := map[string]struct{ kind, mime string }{
		"audio/theme.mp3":    {"audio", "audio/mpeg"},
		"clip.webm":          {"video", "video/webm"},
		"renders/cover.png":  {"image", "image/png"},
		"review/plan.json":   {"data", "application/json"},
		"review/brief.md":    {"doc", "text/markdown"},
		"review/notes.txt":   {"doc", "text/plain"},
		"review/table.csv":   {"data", "text/csv"},
		"review/config.yaml": {"data", "application/yaml"},
		"review/config.yml":  {"data", "application/yaml"},
		"review/spec.pdf":    {"doc", "application/pdf"},
	}
	for _, ref := range got {
		expected, ok := want[ref.Path]
		if !ok {
			t.Errorf("unexpected review attachment %+v", ref)
			continue
		}
		if ref.RunID != "media-run" || ref.Kind != expected.kind || ref.MIME != expected.mime {
			t.Errorf("review attachment = %+v, want run=media-run kind=%s mime=%s", ref, expected.kind, expected.mime)
		}
	}
}

func TestReviewMediaTypeRejectsActiveAndUnknownFiles(t *testing.T) {
	for _, path := range []string{
		"active.svg",
		"active.html",
		"active.htm",
		"script.js",
		"archive.zip",
		"no-extension",
	} {
		t.Run(path, func(t *testing.T) {
			if kind, mime, ok := reviewMediaType(path); ok {
				t.Fatalf("reviewMediaType(%q) = (%q, %q, true), want rejected", path, kind, mime)
			}
		})
	}
}

func TestNormalizeReviewMediaRefsValidatesAgainstManifest(t *testing.T) {
	available := []store.ReviewMediaRef{
		{RunID: "run-1", Path: "renders/final.png", Kind: "image", MIME: "image/png", Size: 42},
		{RunID: "run-1", Path: "sound/final.wav", Kind: "audio", MIME: "audio/wav", Size: 84},
	}
	raw := []any{
		map[string]any{
			"path": "renders/final.png", "caption": "  Validate the final cover  ",
			"kind": "video", "mime": "text/html", "size": 999999,
		},
		map[string]any{"path": "renders/final.png", "caption": "duplicate"},
		map[string]any{"path": "sound/final.wav", "run_id": "foreign-run", "caption": "foreign"},
		map[string]any{"path": "../run.json", "caption": "traversal"},
		map[string]any{"path": "https://example.test/movie.mp4", "caption": "url"},
		map[string]any{"path": "missing.mp4", "caption": "invented"},
	}

	got := normalizeReviewMediaRefs(raw, available)
	if len(got) != 1 {
		t.Fatalf("normalized media = %+v, want one valid deduplicated ref", got)
	}
	ref := got[0]
	if ref.Path != "renders/final.png" || ref.RunID != "run-1" || ref.Kind != "image" || ref.MIME != "image/png" || ref.Size != 42 {
		t.Errorf("manifest metadata was not authoritative: %+v", ref)
	}
	if ref.Caption != "Validate the final cover" {
		t.Errorf("caption = %q, want trimmed caption", ref.Caption)
	}
}

func TestNormalizeReviewMediaRefsBoundsCountAndCaption(t *testing.T) {
	available := make([]store.ReviewMediaRef, 0, 15)
	raw := make([]any, 0, 15)
	longCaption := strings.Repeat("é", maxReviewMediaCaption+20)
	for i := 0; i < 15; i++ {
		path := "renders/" + string(rune('a'+i)) + ".png"
		available = append(available, store.ReviewMediaRef{
			RunID: "run-1", Path: path, Kind: "image", MIME: "image/png",
		})
		raw = append(raw, map[string]any{"path": path, "caption": longCaption})
	}

	got := normalizeReviewMediaRefs(raw, available)
	if len(got) != maxReviewMediaPerTurn {
		t.Fatalf("normalized count = %d, want cap %d", len(got), maxReviewMediaPerTurn)
	}
	if n := len([]rune(got[0].Caption)); n != maxReviewMediaCaption {
		t.Errorf("caption runes = %d, want %d", n, maxReviewMediaCaption)
	}
}

func TestDoPausePersistsValidatedReviewMediaOnCheckpoint(t *testing.T) {
	s := tmpStore(t)
	ctx := context.Background()
	if _, err := s.CreateRun(ctx, "media-pause", "review_test", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	dir, err := store.AsRunFilesStore(s).EnsureRunFilesDir(ctx, "media-pause")
	if err != nil {
		t.Fatalf("EnsureRunFilesDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "candidate.mp4"), []byte("video"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	eng := New(minimalReviewWorkflow(), s, newStubExecutor())
	rs := eng.newRunState("media-pause", nil)
	rs.ctx = ctx
	questions := map[string]any{
		reviewMediaRefsKey: []any{
			map[string]any{"path": "candidate.mp4", "caption": "Validate motion and timing"},
			map[string]any{"path": "../run.json", "caption": "must be dropped"},
		},
	}
	if err := eng.doPause(rs, "gate", questions, map[string]any{"instructions": "Review it"}, pauseInfo{}); err != nil {
		t.Fatalf("doPause: %v", err)
	}
	run, err := s.LoadRun(ctx, "media-pause")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if run.Checkpoint == nil || len(run.Checkpoint.InteractionMedia) != 1 {
		t.Fatalf("checkpoint media = %+v", run.Checkpoint)
	}
	ref := run.Checkpoint.InteractionMedia[0]
	if ref.RunID != "media-pause" || ref.Path != "candidate.mp4" || ref.Kind != "video" || ref.Caption != "Validate motion and timing" {
		t.Errorf("checkpoint media ref = %+v", ref)
	}
	if _, leaked := questions[reviewMediaRefsKey]; leaked {
		t.Error("raw media_refs remained in the pause questions")
	}
	if _, leaked := run.Checkpoint.InteractionQuestions[reviewMediaRefsKey]; leaked {
		t.Error("raw media_refs leaked into checkpoint questions")
	}
	interaction, err := s.LoadInteraction(ctx, "media-pause", run.Checkpoint.InteractionID)
	if err != nil {
		t.Fatalf("LoadInteraction: %v", err)
	}
	if _, leaked := interaction.Questions[reviewMediaRefsKey]; leaked {
		t.Error("raw media_refs leaked into persisted interaction questions")
	}
}

func TestReviewMediaForPauseHonorsEmptyCompanionSelection(t *testing.T) {
	s := tmpStore(t)
	ctx := context.Background()
	if _, err := s.CreateRun(ctx, "media-empty", "review_test", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	dir, err := store.AsRunFilesStore(s).EnsureRunFilesDir(ctx, "media-empty")
	if err != nil {
		t.Fatalf("EnsureRunFilesDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "candidate.mp4"), []byte("video"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	eng := New(minimalReviewWorkflow(), s, newStubExecutor())
	rs := eng.newRunState("media-empty", nil)
	rs.ctx = ctx
	questions := map[string]any{
		reviewMediaRefsKey: []any{map[string]any{"path": "candidate.mp4"}},
	}
	eventExtra := map[string]any{"media": []store.ReviewMediaRef(nil)}
	if got := eng.reviewMediaForPause(rs, questions, eventExtra); len(got) != 0 {
		t.Fatalf("explicit empty companion selection = %+v, want no media", got)
	}
}

func TestDoPausePersistsGuidedReviewStateOnCheckpoint(t *testing.T) {
	s := tmpStore(t)
	ctx := context.Background()
	if _, err := s.CreateRun(ctx, "guided-review-pause", "review_test", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	eng := New(minimalReviewWorkflow(), s, newStubExecutor())
	rs := eng.newRunState("guided-review-pause", nil)
	rs.ctx = ctx
	turns := []store.InteractionTurn{
		{Role: "companion", Content: "Test the rendered clip."},
		{Role: "human", Content: "Playback is smooth."},
	}
	extra := map[string]any{
		"review":         true,
		"instructions":   "Check playback.",
		"posture":        "human_required",
		"merge_strategy": "squash",
		"merge_into":     "main",
		"max_turns":      5,
		"review_url":     "https://review.example.test/42",
		"verdict":        map[string]any{"decision": "approved", "confidence": "high"},
	}
	if err := eng.doPause(rs, "gate", nil, extra, pauseInfo{Turns: turns}); err != nil {
		t.Fatalf("doPause: %v", err)
	}

	run, err := s.LoadRun(ctx, "guided-review-pause")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if run.Checkpoint == nil || run.Checkpoint.InteractionReview == nil {
		t.Fatalf("checkpoint guided review state = %+v", run.Checkpoint)
	}
	got := run.Checkpoint.InteractionReview
	if got.Posture != "human_required" || got.MergeStrategy != "squash" ||
		got.MergeInto != "main" || got.MaxTurns != 5 ||
		got.ReviewURL != "https://review.example.test/42" {
		t.Errorf("guided review metadata = %+v", got)
	}
	if len(got.Turns) != 2 || got.Turns[0].Content != "Test the rendered clip." ||
		got.Turns[1].Content != "Playback is smooth." {
		t.Errorf("guided review turns = %+v", got.Turns)
	}
	if got.Verdict["decision"] != "approved" || got.Verdict["confidence"] != "high" {
		t.Errorf("guided review verdict = %+v", got.Verdict)
	}
}

type recordingRunFilesUploader struct {
	store.RunStore
	uploads int
}

func (s *recordingRunFilesUploader) UploadRunFiles(context.Context, string) (int, error) {
	s.uploads++
	return 1, nil
}

func TestFlushReviewMediaForPauseUsesCloudBridge(t *testing.T) {
	wrapped := &recordingRunFilesUploader{RunStore: tmpStore(t)}
	eng := New(minimalReviewWorkflow(), wrapped, newStubExecutor())
	rs := eng.newRunState("media-run", nil)
	rs.ctx = context.Background()

	eng.flushReviewMediaForPause(rs, []store.ReviewMediaRef{{RunID: "media-run", Path: "clip.mp4", Kind: "video"}})
	if wrapped.uploads != 1 {
		t.Fatalf("uploads = %d, want one pre-pause durable flush", wrapped.uploads)
	}
	eng.flushReviewMediaForPause(rs, nil)
	if wrapped.uploads != 1 {
		t.Fatalf("empty media unexpectedly flushed: uploads=%d", wrapped.uploads)
	}
}
