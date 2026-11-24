package ast

import "testing"

// An unfilled block — what a canvas creates and does not complete — is read
// as ABSENT, so the document saves and reads back the same. `sandbox: {}` has
// done this since it shipped, with a comment saying why; `codemode: {}` was a
// verbatim field copy that took the shape and left the rule, so the writer
// could not render what the reader accepted and `unparse.Verify` refused to
// save the document.
func TestAnUnfilledCodemodeBlockReadsAsAbsent(t *testing.T) {
	t.Parallel()
	if got := codemodeBlockFromJSON(&jsonCodemodeBlock{}); got != nil {
		t.Errorf("an unfilled block decoded to %+v, want nil (absent)", got)
	}
	// And the same rule the sandbox twin states next door: a block with
	// fields but no mode is the block form, which the parser reads as `on`.
	got := codemodeBlockFromJSON(&jsonCodemodeBlock{ToolCalls: 128})
	if got == nil {
		t.Fatalf("a block with a bound decoded to nil")
	}
	if got.Mode != "on" {
		t.Errorf("Mode = %q, want \"on\": the transport must apply the default the parser applies, or a document and its re-parse are two programs", got.Mode)
	}
	if got.ToolCalls != 128 {
		t.Errorf("ToolCalls = %d, want 128", got.ToolCalls)
	}
	// A mode written out is kept as written — the normalisation fills a
	// hole, it does not overwrite a choice.
	if got := codemodeBlockFromJSON(&jsonCodemodeBlock{Mode: "off", ToolCalls: 128}); got.Mode != "off" {
		t.Errorf("Mode = %q, want \"off\" kept as written", got.Mode)
	}
	if got := codemodeBlockFromJSON(&jsonCodemodeBlock{Mode: "off"}); got == nil || got.Mode != "off" {
		t.Errorf("a mode-only block decoded to %+v, want the short form kept", got)
	}
}
