package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

func fileAnswerStore(t *testing.T, runID string) store.RunStore {
	t.Helper()
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	if _, err := s.CreateRun(context.Background(), runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	return s
}

func TestResolveFileAnswerFlags_AttachesLocalFile(t *testing.T) {
	ctx := context.Background()
	s := fileAnswerStore(t, "cli-run")

	src := filepath.Join(t.TempDir(), "theme.mp3")
	if err := os.WriteFile(src, []byte("id3 fake audio"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	answers := map[string]any{
		"music":    "@" + src,
		"approved": "true",
	}
	if err := resolveFileAnswerFlags(ctx, s, "cli-run", "gate_music", answers); err != nil {
		t.Fatalf("resolveFileAnswerFlags: %v", err)
	}

	desc, ok := answers["music"].(map[string]any)
	if !ok {
		t.Fatalf("music = %T, want a descriptor map", answers["music"])
	}
	// Same naming as the HTTP path, so a bot's
	// {{attachments.gate_music.music}} works regardless of which
	// surface answered the gate.
	if desc["attachment"] != "gate_music.music" {
		t.Errorf("attachment = %v, want gate_music.music", desc["attachment"])
	}
	if desc["filename"] != "theme.mp3" {
		t.Errorf("filename = %v, want theme.mp3", desc["filename"])
	}
	if sz, _ := desc["size"].(int64); sz == 0 {
		t.Errorf("size = %v, want the byte length computed from the stored bytes", desc["size"])
	}
	if sha, _ := desc["sha256"].(string); sha == "" {
		t.Error("sha256 missing from the descriptor")
	}
	if answers["approved"] != "true" {
		t.Errorf("non-file answers must pass through untouched, got %v", answers["approved"])
	}

	// The bytes really are on the run.
	rc, _, err := s.OpenAttachment(ctx, "cli-run", "gate_music.music")
	if err != nil {
		t.Fatalf("OpenAttachment: %v", err)
	}
	rc.Close()
}

func TestResolveFileAnswerFlags_EscapedAtStaysLiteral(t *testing.T) {
	ctx := context.Background()
	s := fileAnswerStore(t, "cli-esc")

	answers := map[string]any{"note": "@@channel please review"}
	if err := resolveFileAnswerFlags(ctx, s, "gate", "cli-esc", answers); err != nil {
		t.Fatalf("resolveFileAnswerFlags: %v", err)
	}
	if answers["note"] != "@channel please review" {
		t.Errorf("note = %v, want the unescaped literal", answers["note"])
	}
}

func TestResolveFileAnswerFlags_MissingFileIsAClearError(t *testing.T) {
	ctx := context.Background()
	s := fileAnswerStore(t, "cli-missing")

	answers := map[string]any{"music": "@/nonexistent/nope.mp3"}
	err := resolveFileAnswerFlags(ctx, s, "cli-missing", "gate", answers)
	if err == nil {
		t.Fatal("expected an error for a missing file")
	}
	// The operator must be able to tell WHICH flag was wrong.
	if got := err.Error(); !strings.Contains(got, "music") || !strings.Contains(got, "nope.mp3") {
		t.Errorf("error %q should name both the answer key and the path", got)
	}
}

func TestResolveFileAnswerFlags_PlainAnswersAreUntouched(t *testing.T) {
	ctx := context.Background()
	s := fileAnswerStore(t, "cli-plain")

	answers := map[string]any{"approved": "true", "notes": "looks good"}
	if err := resolveFileAnswerFlags(ctx, s, "cli-plain", "gate", answers); err != nil {
		t.Fatalf("resolveFileAnswerFlags: %v", err)
	}
	if answers["approved"] != "true" || answers["notes"] != "looks good" {
		t.Errorf("answers were altered: %+v", answers)
	}
	list, _ := s.ListAttachments(ctx, "cli-plain")
	if len(list) != 0 {
		t.Errorf("no attachment should have been created, got %d", len(list))
	}
}
