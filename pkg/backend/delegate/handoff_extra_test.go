package delegate

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/llmroute"
)

// TestHandoffExtra_removesThePromptOnce: the block text a structured
// surface appends is the composed prompt minus the original prompt,
// wherever it sat — a prefix slice would chop reuse's preamble mid-word
// (revi R-finding on #2237).
// Mutant: prefix-slice instead of once-removal → the reuse case reds.
func TestHandoffExtra_removesThePromptOnce(t *testing.T) {
	const prompt = "do the thing, carefully"
	section := (&Task{Handoff: "x", HandoffMode: llmroute.CrossHarnessReuse, UserPrompt: prompt}).HandoffSection()

	// Restart: prompt first — the removal trims the head, the section and
	// the suffix survive.
	restart := prompt + section + "SUFFIX"
	if got := HandoffExtra(restart, prompt); got != section+"SUFFIX" {
		t.Fatalf("restart extra = %q, want section+suffix", got)
	}

	// Reuse: prompt LAST — the removal trims the tail, the preamble and
	// the transcript survive INTACT (a prefix slice would start mid-word).
	reuse := section + "\n---\n\nThe task:\n\n" + prompt
	got := HandoffExtra(reuse, prompt)
	if !strings.HasPrefix(got, "CONTINUATION:") {
		t.Fatalf("reuse extra lost the preamble: %q", got)
	}
	if !strings.Contains(got, "The task:") {
		t.Fatalf("reuse extra lost its separator: %q", got)
	}

	// No handoff, schema suffix only: today's behavior, byte-same.
	if got := HandoffExtra(prompt+"SUFFIX", prompt); got != "SUFFIX" {
		t.Fatalf("suffix-only extra = %q, want SUFFIX", got)
	}
	// The original prompt is not there at all: the composition survives.
	if got := HandoffExtra(section, prompt); !strings.Contains(got, "CONTINUATION:") {
		t.Fatalf("unrelated composition mangled: %q", got)
	}
}
