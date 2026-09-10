package bots

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRebaselineDoctrineTeachesQualification pins the doctrine a lot reads
// before it asks for a re-baseline. It is a documentary test and claims
// nothing about a run: what it prevents is a rewrite of these two files
// that DROPS one of the three readings or MOVES the guidance out of the
// section that gives it its meaning — which is how guidance decays. It
// cannot catch a rewrite that keeps the phrases and inverts the advice;
// only a reader does. Whitespace is normalised first, so a reflow of the
// same words is not a failure.
//
// The gap it closes was measured: a lot met seventeen moved references,
// filed ONE request for all of them, and blocked. Three causes were hiding
// inside — an order no ORDER BY decided (an artefact: make the product
// deterministic, re-record once), a collation-dependent sort (ambiguous: an
// overlay reproduces the wanted order), and a login that stopped working
// under a case-sensitive engine (a betrayed intention: repair the product,
// the reference stays). Acting the request as filed would have recorded the
// lost login as contract and created a second reference set per engine.
func TestRebaselineDoctrineTeachesQualification(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	// Whitespace-insensitive: a reflow of the same words must not fail, and
	// a phrase that straddles a line break must still be found.
	flat := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	read := func(rel string) string {
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		return string(b)
	}
	// section returns the body of a markdown heading, so guidance that is
	// moved out of the section giving it its meaning reads as lost — the
	// qualification is doctrine of "Never touch the oracle", not a fifth way
	// a lot goes green.
	section := func(doc, heading string) string {
		i := strings.Index(doc, heading)
		if i < 0 {
			t.Fatalf("heading %q not found", heading)
		}
		body := doc[i+len(heading):]
		if j := strings.Index(body, "\n## "); j > 0 {
			body = body[:j]
		}
		return flat(body)
	}

	skill := read("bots/modernize/skills/modernize-lots.md")
	oracle := section(skill, "\n## Never touch the oracle\n")
	for _, want := range []struct{ what, phrase string }{
		{"the qualification step itself", "Qualify the divergence before you ask"},
		{"the betrayed-intention class", "betrayed intention"},
		{"the artefact class", "unspecified artefact"},
		{"the ambiguous class", "ambiguous"},
		{"making the product deterministic", "deterministic"},
		{"the overlay for the ambiguous class", "overlay"},
		{"the one-reference-set rule", "one reference set"},
		{"its teeth (the second set is what is refused)", "never one per engine, per platform, per environment"},
		{"that a repaired intention keeps its reference", "the reference does not move"},
		{"that a determinism fix is still announced", "still announced, in a written request"},
		{"one request per red episode, not one per path", "One request per red episode"},
		{"why splitting is refused", "makes every one of them mismatch"},
		{"the stop when two classes remain", "that is a stop"},
		{"that the actable grain is the class, not the path", "grain that can ever be acted is the CLASS, never the path"},
	} {
		if !strings.Contains(oracle, want.phrase) {
			t.Errorf("the \"Never touch the oracle\" doctrine no longer teaches %s (looked for %q)", want.what, want.phrase)
		}
	}

	// The prompt is what an agent reads when it never opens the skill: the
	// pointer and the two product-side gestures must survive there too.
	bot := read("bots/modernize/main.bot")
	system := bot
	if i := strings.Index(bot, "prompt campaign_system:"); i >= 0 {
		system = bot[i:]
		if j := strings.Index(system, "\nprompt "); j > 0 {
			system = system[:j]
		}
	} else {
		t.Fatal("prompt campaign_system not found in bots/modernize/main.bot")
	}
	system = flat(system)
	for _, want := range []struct{ what, phrase string }{
		{"the qualification duty", "QUALIFYING it is"},
		{"the product-side repair", "repaired in the product and its reference stays"},
		{"the determinism gesture", "made deterministic in the product"},
		{"that a determinism fix is still announced", "are still announced"},
		{"one request per red episode", "ONE written request per red episode"},
		{"the per-path classification inside it", "classified path by path inside it"},
		{"the pointer to the doctrine", "modernize-lots"},
	} {
		if !strings.Contains(system, want.phrase) {
			t.Errorf("campaign_system no longer carries %s (looked for %q)", want.what, want.phrase)
		}
	}
	// The rule it refines must still be there: qualifying never licenses
	// editing the net.
	if !strings.Contains(system, "Never touch the oracle") ||
		!strings.Contains(system, "REGRESSION YOU CAUSED") {
		t.Error("campaign_system lost the rule qualification refines: the net stays off-limits")
	}
}
