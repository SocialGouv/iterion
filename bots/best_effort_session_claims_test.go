package bots

import (
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// `session: inherit_if_available` is best-effort BY CONTRACT: the executor
// marks the resolved id droppable and degrades to a fresh session rather than
// failing the node (pkg/backend/model/executor_build_task.go,
// applySessionContinuity; pkg/runtime/resume.go). So a bundle that tells its
// model — or its reader — that the conversation IS remembered promises a
// channel no route owes, and the turn that lands on a cold pod answers from a
// memory it does not have without saying so.
//
// Two guards, deliberately of different kinds:
//
//   - the one below discovers its SUBJECT mechanically — every catalogue LLM
//     node whose session mode is best-effort, and, inside its own prompts,
//     every paragraph that claims a memory of earlier turns — and requires
//     THAT paragraph to hedge it. The verdict is a VOCABULARY check, so it is
//     an alert and not a proof: a prompt can promise a memory in words this
//     list does not hold. It is the half that bites where it matters, in the
//     text the model reads;
//   - TestNoPageClaimsABestEffortSessionIsRemembered scans the page class for
//     the phrasings this catalogue actually carried. A blacklist, by nature
//     incomplete, and worth its line because the claim travels by copy-paste:
//     the plan phase's version of it crossed four bundles that way
//     (refutedSessionClaims).

// bestEffortSessionModes are the session modes the engine may silently drop.
// `fresh` carries nothing and promises nothing, so a prompt mentioning a
// session under it is a different defect (a claim with no mechanism at all),
// held by the page guard below.
var bestEffortSessionModes = map[ir.SessionMode]bool{
	ir.SessionInheritIfAvailable: true,
	ir.SessionPersist:            true,
}

// sessionMemoryClaims are the readings that make a paragraph speak about the
// model's own MEMORY of earlier turns — the trigger of the guard below. Not
// the bare word "session": a prompt that says "open the session yourself"
// (app-dev/interviewer) or hands the model a "Session context:" block
// (copilot/reflect) claims no memory, and a trigger that caught them would
// make this guard about conversational framing instead of about what the
// engine promises.
var sessionMemoryClaims = []string{
	"you remember",
	"remember the",
	"remembers",
	"recall",
	"persists across",
	"carries this conversation",
	"earlier in this session",
	"the whole conversation",
	"your memory",
}

// bestEffortHedges are the readings that tell the model the memory may not be
// there. One is enough — the point is that the claim is hedged, not that it
// is hedged in a fixed sentence.
var bestEffortHedges = []string{
	"best-effort",
	"best effort",
	"may be gone",
	"may be empty",
	"may have been lost",
	"if the session was restarted",
	"when its backing state is gone",
	"whose backing session is gone",
	"no session is promised",
	"carries no conversation",
	"starts fresh",
	"runs the turn fresh",
	"runs fresh",
}

// TestEveryBestEffortSessionPromptHedgesItsMemory holds the text the MODEL
// reads, paragraph by paragraph: the hedge has to sit WITH the claim. Scoping
// it to the whole prompt would let an unrelated "may be empty" three
// paragraphs away answer for a promise — measured: stripping
// copilot/copi's own hedge left the node green under a whole-prompt reading,
// because another field's sentence carried the word.
func TestEveryBestEffortSessionPromptHedgesItsMemory(t *testing.T) {
	var subjects []string
	bestEffortNodes := 0
	for _, sw := range shippedWorkflows(t) {
		ids := make([]string, 0, len(sw.wf.Nodes))
		for id := range sw.wf.Nodes {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			agent, isAgent := sw.wf.Nodes[id].(*ir.AgentNode)
			if !isAgent || !bestEffortSessionModes[agent.Session] {
				continue
			}
			bestEffortNodes++
			f := agent.GetLLMFields()
			for _, name := range []string{f.SystemPrompt, f.UserPrompt} {
				p := sw.wf.Prompts[name]
				if p == nil {
					continue
				}
				for _, para := range promptParagraphs(p.Body) {
					low := strings.ToLower(para)
					claim, claims := firstReading(low, sessionMemoryClaims)
					if !claims {
						continue
					}
					subjects = append(subjects, sw.prefix+id+" ("+name+")")
					if _, hedged := firstReading(low, bestEffortHedges); hedged {
						continue
					}
					t.Errorf("%s%s runs session: %s — best-effort, which the engine may drop — and %s claims %q in a paragraph that never says the memory may be gone. "+
						"A turn that lands without its backing state then answers from a memory it does not have. Hedge it where it is claimed (one of %v), or declare a session mode the engine honours:\n%s",
						sw.prefix, id, agent.Session, name, claim, bestEffortHedges, indentBlock(para))
				}
			}
		}
	}
	// Two floors: a walk that finds no best-effort node at all, and a
	// vocabulary check whose trigger stopped matching anything, both report
	// the same green as a guard that passed.
	if bestEffortNodes == 0 {
		t.Fatal("no best-effort-session LLM node in the catalogue — this guard's class is gone, or the node walk broke")
	}
	if len(subjects) == 0 {
		t.Fatalf("%d best-effort-session node(s) walked and NO paragraph claims a memory: the trigger vocabulary %v no longer matches the catalogue, so this guard judges nothing",
			bestEffortNodes, sessionMemoryClaims)
	}
	t.Logf("%d best-effort-session node(s); %d paragraph(s) claiming a memory of earlier turns: %v", bestEffortNodes, len(subjects), subjects)
}

// refutedBestEffortSessionPromises are the phrasings this catalogue carried
// that promise the resumed session's memory as a fact. Lower-cased; matched
// against the page and against its collapsed reading, so a claim an author
// wrapped across two lines is still one phrase.
var refutedBestEffortSessionPromises = []string{
	"session persists across turns",
	"so you remember the whole conversation",
	"remembers the whole conversation",
	"the inherited model session carries this conversation",
	"the conversation has real memory",
	"remembers the standing conversation",
}

// TestNoPageClaimsABestEffortSessionIsRemembered is the same claim one level
// out: the pages a reader and a model are handed. Over the class, not over
// the bundle that carried it — the plan phase's version of this claim crossed
// four bundles by copy-paste, and the next bundle to grow a chat loop will be
// written by copying one of them.
func TestNoPageClaimsABestEffortSessionIsRemembered(t *testing.T) {
	if len(refutedBestEffortSessionPromises) == 0 {
		t.Fatal("the refused list is empty: this guard would pass every page")
	}
	for _, page := range pagesAReaderAndAModelRead(t) {
		raw, err := os.ReadFile(page)
		if err != nil {
			t.Fatalf("read %s: %v", page, err)
		}
		low, collapsed := strings.ToLower(string(raw)), collapsedPage(string(raw))
		for _, claim := range refutedBestEffortSessionPromises {
			if !strings.Contains(low, claim) && !strings.Contains(collapsed, claim) {
				continue
			}
			t.Errorf("%s claims %q — the session it speaks of is resumed best-effort, and the engine drops it rather than failing the node when its backing state is gone. "+
				"Say what holds on every route: the session is resumed when it can be, and the turn runs fresh on its own input when it cannot", page, claim)
		}
	}
}

// promptParagraphs cuts a prompt body the way its reader sees it: blocks
// separated by a blank line, each read as one run of words so a sentence the
// author wrapped across lines is one phrase again. A body with no blank line
// is one paragraph.
func promptParagraphs(body string) []string {
	var out []string
	var cur []string
	flush := func() {
		if len(cur) == 0 {
			return
		}
		out = append(out, strings.Join(cur, " "))
		cur = nil
	}
	for _, line := range strings.Split(body, "\n") {
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		cur = append(cur, strings.TrimSpace(line))
	}
	flush()
	return out
}

// firstReading returns the first of readings present in s, so a report quotes
// the reading that fired instead of the whole list.
func firstReading(s string, readings []string) (string, bool) {
	for _, r := range readings {
		if strings.Contains(s, r) {
			return r, true
		}
	}
	return "", false
}

// indentBlock quotes a paragraph inside a test failure, wrapped so the report
// stays readable for a bullet list run into one line.
func indentBlock(para string) string {
	const width = 72
	var b strings.Builder
	line := "   "
	for _, word := range strings.Fields(para) {
		if len(line)+1+len(word) > width {
			b.WriteString(line)
			b.WriteString("\n")
			line = "   "
		}
		line += " " + word
	}
	b.WriteString(line)
	return b.String()
}
