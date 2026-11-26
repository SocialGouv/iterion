package catalogclaims

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/sweepwalk"
)

// refutedReuseClaims are claims about the reuse pre-check that measurement
// refutes. It refuses a script carrying bots' hatchMarker, and one whose every
// non-comment line is, whole, a set of shell options, a plain echo or printf, a
// bare truth or a zero exit — nothing else. The limit cases of
// bots' TestVerifyProbeNeverReusesAnEmptyGate run #1707's own one-liner, a punctuated
// message, a two-line message, a redirect, a heredoc, a pipeline, a `cd`, a
// comment trailing the exit and a message quoting a substitution through every
// discovered pre-check and measure
// fresh:true, so a page promising that NO empty gate is ever reused sends its
// reader — and the unattended-merge decision that rests on these gates — to a
// green that certifies nothing. Widening the allowlist widens this list's
// premise, not the list.
// Matched against a case-folded copy of the page: the vocabulary these claims
// are made of is written in CAPS at the carriers ("A RECOGNISED EMPTY GATE is
// never REUSED"), so a scanner over the raw bytes let the claim through in the
// voice the pages actually use. The qualified forms ("a RECOGNISED empty gate",
// "the hatch the prescription asks for") are true and must stay sayable, so the
// claims below carry their article: `an empty gate is never reused` is the
// absolute one, `a recognised empty gate is never reused` is not.
var refutedReuseClaims = []string{
	"never an empty gate",
	"never reuses an empty gate",
	"no empty gate is reused",
	"an empty gate is never reused",
	"the empty gate is never reused",
	"empty gate can no longer latch",
	"any empty gate is",
	"every empty gate",
	"cannot become the verdict",
	"never becomes the run",
}

// TestNoPageClaimsEveryEmptyGateIsRefused scans what a reader actually reads —
// the bundle READMEs, every verify skill, the bot sources and the ratchet page
// — for a claim the pre-check does not honour. The claim travelled by
// copy-paste across this class once already, so the guard is over the class and
// not over the file that carried it.
func TestNoPageClaimsEveryEmptyGateIsRefused(t *testing.T) {
	root := sweepwalk.ModuleRoot(t)
	pages := append(pagesAReaderAndAModelRead(t, root), filepath.Join(root, "docs", "improvement-ratchet.md"))
	for _, page := range pages {
		body, err := os.ReadFile(page)
		if err != nil {
			t.Fatalf("read %s: %v", page, err)
		}
		low := strings.ToLower(string(body))
		for _, claim := range refutedReuseClaims {
			if strings.Contains(low, claim) {
				t.Errorf("%s claims %q — the pre-check refuses the marked hatch and a body of "+
					"whole-line no-ops, and keeps the reuse for every line it cannot judge; say "+
					"what it refuses, or a reader trusts a green that certifies nothing",
					page, claim)
			}
		}
	}
}
