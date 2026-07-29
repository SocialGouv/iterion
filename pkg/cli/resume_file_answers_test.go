package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/store"
)

// compileSourceForTest compiles inline .bot source to an IR workflow.
func compileSourceForTest(t *testing.T, src string) *ir.Workflow {
	t.Helper()
	pr := parser.Parse("test.bot", src)
	for _, d := range pr.Diagnostics {
		t.Fatalf("parse: %s", d.Error())
	}
	res := ir.Compile(pr.File)
	for _, d := range res.Diagnostics {
		if d.Severity == ir.SeverityError {
			t.Fatalf("compile: %s", d.Message)
		}
	}
	return res.Workflow
}

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
	if err := resolveFileAnswerFlags(ctx, s, "cli-run", "gate_music", map[string]bool{"music": true}, answers); err != nil {
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
	if err := resolveFileAnswerFlags(ctx, s, "cli-esc", "gate", map[string]bool{"note": true}, answers); err != nil {
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
	err := resolveFileAnswerFlags(ctx, s, "cli-missing", "gate", map[string]bool{"music": true}, answers)
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
	if err := resolveFileAnswerFlags(ctx, s, "cli-plain", "gate", map[string]bool{"music": true}, answers); err != nil {
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

// The '@' convention must not leak onto answers the schema never
// declared as files: --answer / --answers-file is a machine-fed
// interface, and a chat mention, an npm scope or a `@v1.2` ref
// legitimately starts with '@'. Rewriting those into a file open breaks
// callers that never opted into anything.
func TestResolveFileAnswerFlags_NonFileFieldKeepsLeadingAt(t *testing.T) {
	ctx := context.Background()
	s := fileAnswerStore(t, "cli-scope")

	answers := map[string]any{
		"note":  "@channel please review",
		"scope": "@acme/toolkit",
	}
	// `note` and `scope` are plain strings in the gate's schema.
	if err := resolveFileAnswerFlags(ctx, s, "cli-scope", "gate", map[string]bool{"music": true}, answers); err != nil {
		t.Fatalf("resolveFileAnswerFlags: %v", err)
	}
	if answers["note"] != "@channel please review" {
		t.Errorf("note = %v, want the verbatim string", answers["note"])
	}
	if answers["scope"] != "@acme/toolkit" {
		t.Errorf("scope = %v, want the verbatim string", answers["scope"])
	}
}

// fileAnswerFields is what scopes the convention, so it must read the
// paused node's OWN output schema and pick out only `file` fields.
func TestFileAnswerFields_ReadsPausedNodeSchema(t *testing.T) {
	src := `prompt ask_track:
  Upload the track.

schema gate_out:
  music: file
  notes: string

human gate_music:
  instructions: ask_track
  output: gate_out
  interaction: human

workflow file_gate:
  entry: gate_music
  gate_music -> done
`
	wf := compileSourceForTest(t, src)

	got := fileAnswerFields(wf, "gate_music")
	if !got["music"] {
		t.Errorf("music should be recognised as a file field, got %v", got)
	}
	if got["notes"] {
		t.Errorf("notes is a string field and must not be file-scoped, got %v", got)
	}
	if len(fileAnswerFields(wf, "nope")) != 0 {
		t.Error("an unknown node id must yield no file scope")
	}
}
