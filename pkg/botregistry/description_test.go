package botregistry

import "testing"

func TestLeadingCommentDescription_SkipsDecorationAndFilenameHeader(t *testing.T) {
	raw := []byte(`## ─────────────────────────────
## board_smoke.bot
##
## End-to-end smoke for the bot capabilities pipeline.
## Second sentence of the paragraph.

agent a:
`)
	got := leadingCommentDescription(raw, "board_smoke.bot")
	want := "End-to-end smoke for the bot capabilities pipeline. Second sentence of the paragraph."
	if got != want {
		t.Fatalf("description = %q, want %q", got, want)
	}
}

func TestLeadingCommentDescription_PlainParagraph(t *testing.T) {
	raw := []byte("## A simple bot.\n## Does one thing.\n\nagent a:\n")
	if got := leadingCommentDescription(raw, "simple.bot"); got != "A simple bot. Does one thing." {
		t.Fatalf("description = %q", got)
	}
}

func TestLeadingCommentDescription_AllDecoration(t *testing.T) {
	raw := []byte("## ====\n## ----\n\nagent a:\n")
	if got := leadingCommentDescription(raw, "x.bot"); got != "" {
		t.Fatalf("description = %q, want empty", got)
	}
}
