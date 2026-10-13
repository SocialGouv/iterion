package delegate

import (
	"fmt"
	"os"
	"strings"

	"github.com/SocialGouv/iterion/pkg/llmroute"
)

// The routing-handoff render (ADR-121 § Delivery 2): how a task carrying
// a sealed handoff prompts the harness that takes over. The composer is
// PURE — it reads (UserPrompt, Handoff, HandoffMode) and produces the
// wire payload; it never writes back into the task, whose cached instance
// a later rung re-renders.
//
// reuse: the node CONTINUES — a continuation preamble, the transcript,
// then the original prompt as the task statement.
// restart: the node starts over — the original prompt FIRST,
// byte-preserved, then the transcript as a reference section.
//
// A task without a handoff returns UserPrompt unchanged: the
// byte-identity anchor for every run that never crossed.

// HandoffPrompt returns the user prompt this task dispatches with. The
// transcript is read from the Handoff path at render time; a file that
// cannot be read is SAID in the section (the event already named the
// path — silence would overstate what the incoming harness received).
func (t *Task) HandoffPrompt() string {
	if t.Handoff == "" || t.HandoffMode == "" {
		return t.UserPrompt
	}
	switch t.HandoffMode {
	case llmroute.CrossHarnessReuse:
		return t.HandoffSection() + "\n---\n\nThe task:\n\n" + t.UserPrompt
	default:
		return t.UserPrompt + t.HandoffSection()
	}
}

// HandoffSection renders the handoff's own text - everything that is NOT
// the task's original prompt: reuse = the continuation preamble plus the
// transcript; restart = the reference section with its separator. Empty
// when the task carries no handoff. A backend that renders the prompt
// through a structured surface (claw's content blocks) appends THIS as
// its own block instead of re-deriving a delta by slicing the composed
// text: reuse does not keep the prompt as a prefix, so a prefix slice
// would chop the preamble mid-word (revi R-finding on #2237).
func (t *Task) HandoffSection() string {
	if t.Handoff == "" || t.HandoffMode == "" {
		return ""
	}
	transcript, err := os.ReadFile(t.Handoff)
	body := string(transcript)
	if err != nil {
		body = fmt.Sprintf("[handoff transcript unreadable: %v]", err)
	}
	ref := fmt.Sprintf("## Routing handoff (%s)\n\nSealed transcript of the previous harness's work on this node - %s:\n\n%s",
		t.HandoffMode, t.Handoff, body)
	switch t.HandoffMode {
	case llmroute.CrossHarnessReuse:
		return "CONTINUATION: another agent started this task; the work it already did follows; continue from where it left off instead of starting over.\n\n" + ref
	default:
		return "\n\n---\n\n" + ref
	}
}

// HandoffExtra returns the text a structured-surface backend appends as
// its own block beside the task's content blocks: the composed prompt
// with the original prompt removed ONCE, wherever it sat. Removing by
// position instead of slicing a prefix is the whole point — restart
// composes prompt-first (the removal trims the head, leaving the handoff
// section and any schema suffix), reuse composes prompt-last (the
// removal trims the tail, leaving the continuation preamble and the
// transcript intact).
func HandoffExtra(composed, original string) string {
	extra := composed
	if original != "" {
		if i := strings.Index(extra, original); i >= 0 {
			extra = extra[:i] + extra[i+len(original):]
		}
	}
	return extra
}
