package bots

import (
	"os"
	"strings"
	"testing"
)

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
