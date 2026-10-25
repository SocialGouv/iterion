package bots

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/mdcode"
	"github.com/SocialGouv/iterion/pkg/bundle"
)

// The markers pkg/botregistry/catalog.go splices between. Spelled here
// because they are the test's subject: the guard must know where the
// generated region starts and ends to say which lines the TEMPLATE owns.
// Both lookups t.Fatal when a marker is missing, so a rename reddens here
// instead of silently emptying the comparison.
const (
	catalogBeginMarker = "<!-- ITERION:CATALOG:GENERATED:BEGIN -->"
	catalogEndMarker   = "<!-- ITERION:CATALOG:GENERATED:END -->"
	// catalogGeneratedFile is the name the splice writes beside the template,
	// under skills/.
	catalogGeneratedFile = "iterion-bot-catalog.md"
)

// linkDestination matches the destination of an inline markdown link, the
// one part of a spliced line the copy is expected to differ on: the generated
// file lives one directory below its template, so pkg/botregistry re-anchors
// every relative target it carries.
var linkDestination = regexp.MustCompile(`\]\([^)\n]*\)`)

// lineIndexOf returns the index of the first line equal to marker once
// trimmed, or -1.
func lineIndexOf(lines []string, marker string) int {
	for i, line := range lines {
		if strings.TrimSpace(line) == marker {
			return i
		}
	}
	return -1
}

// isCodeLine reports whether mdcode.Mask blanked the WHOLE line — the
// reading "this line is inside, or is, a fenced code block". A line of
// prose carrying an inline span is blanked in part and is not code by this
// measure; a line that was already blank is not code either.
func isCodeLine(masked, source string) bool {
	return strings.TrimSpace(source) != "" && strings.TrimSpace(masked) == ""
}

// codeReading walks body once and returns its lines beside isCodeLine's
// verdict for each.
//
// The walk is internal/mdcode.Lines — the rule the mask itself reasons over,
// and the rule a renderer breaks lines on. A gate splitting on line feeds
// alone pairs a mask line with a source line it did not come from the moment
// a file carries a lone carriage return: that break is on the page, so the
// swallowed text reads here as ONE prose line, and the gate reports the
// corruption it exists to catch as sound.
func codeReading(t *testing.T, body string) (lines []string, code []bool) {
	t.Helper()
	lines = mdcode.Lines(body)
	masked := mdcode.Lines(mdcode.Mask(body))
	// Mask keeps every byte offset, so the two walks see the same lines. A
	// mismatch would silently compare a verdict against another line.
	if len(masked) != len(lines) {
		t.Fatalf("the mask has %d lines and the source %d — the two walks disagree about where the lines are", len(masked), len(lines))
	}
	code = make([]bool, len(lines))
	for i := range lines {
		code[i] = isCodeLine(masked[i], lines[i])
	}
	return lines, code
}

// TestGeneratedCatalogPreservesTheTemplateCodeReading fails when the
// generated catalogue reads the hand-authored template's lines differently
// from the template itself — which is what a fenced block OPENED inside the
// generated region and never closed does to everything after it.
//
// The region between the markers is rendered from the manifests; the text
// before and after it is spliced verbatim. So every line the template owns
// must be code in the copy exactly where it is code in the template. A
// manifest description word-wrapped onto a ```fence line breaks that: the
// copy's fence state at the END marker is "open", and the template's prose
// below reads as code until the next bare delimiter.
//
// "Code" is internal/mdcode's reading, which is CommonMark's block phase
// for the fence-relevant subset: the container stack — list items with
// their content columns, block quotes — the indented code block, the
// interruption and lazy-continuation rules, behind
// internal/mdcode.FenceScanner. The shapes it still reads by approximation
// are stated in that package (HTML blocks, the setext underline, the GFM
// table's paragraph-state effect, tab reconstruction): a region of
// any shape the statement lists satisfies this guard and is open on the
// page. Named there rather than copied here: a copy drifts from the
// statement it copies, and a stale copy is what tells a reader the class is
// closed.
func TestGeneratedCatalogPreservesTheTemplateCodeReading(t *testing.T) {
	// Every catalog template, floored per root: a walk that finds nothing is
	// a green gate over 3500 committed lines of routing table — the shape a
	// rename of the template would leave behind. Both router bundles ship
	// one, and the floor saying so lives on the root (catalogTemplateRoot).
	templates := catalogTemplates(t)
	for _, tmpl := range templates {
		genPath := filepath.Join(filepath.Dir(tmpl), "skills", catalogGeneratedFile)
		tmplBody, err := os.ReadFile(tmpl)
		if err != nil {
			t.Fatalf("read catalog template %s: %v", tmpl, err)
		}
		genBody, err := os.ReadFile(genPath)
		if err != nil {
			t.Fatalf("read generated catalog %s: %v", genPath, err)
		}
		tmplLines, tmplCode := codeReading(t, string(tmplBody))
		genLines, genCode := codeReading(t, string(genBody))

		tb, te := lineIndexOf(tmplLines, catalogBeginMarker), lineIndexOf(tmplLines, catalogEndMarker)
		gb, ge := lineIndexOf(genLines, catalogBeginMarker), lineIndexOf(genLines, catalogEndMarker)
		if tb < 0 || te < 0 {
			t.Fatalf("%s: catalog markers missing (begin=%d end=%d)", tmpl, tb, te)
		}
		if gb < 0 || ge < 0 {
			t.Fatalf("%s: catalog markers missing (begin=%d end=%d)", genPath, gb, ge)
		}
		// The head is spliced byte-for-byte, so the markers sit at the same
		// index; the tail keeps its length. Either mismatch means the splice
		// itself changed and the line alignment below would compare unrelated
		// lines.
		if tb != gb {
			t.Fatalf("%s: BEGIN marker at line %d, %s has it at line %d — the head is not spliced verbatim", tmpl, tb+1, genPath, gb+1)
		}
		// Comparing a CODE READING of the head would prove nothing: the
		// splice copies those bytes. So the bytes are the claim — every byte
		// but a link DESTINATION, which the copy is meant to rewrite: it
		// sits one directory deeper than the template, so a relative target
		// above the BEGIN marker has to climb one more level. Blanking the
		// destinations is what keeps this guard from reddening on an
		// ordinary editorial link, with a message blaming the splice.
		head := linkDestination.ReplaceAllString(strings.Join(tmplLines[:tb+1], "\n"), "](…)")
		genHead := linkDestination.ReplaceAllString(strings.Join(genLines[:gb+1], "\n"), "](…)")
		if head != genHead {
			t.Errorf("%s: the text ABOVE the BEGIN marker is not the template's own — the splice no longer copies the head verbatim (a link destination is the one thing it may rewrite)", genPath)
		}
		if len(tmplLines)-te != len(genLines)-ge {
			t.Fatalf("%s: tail is %d lines, %s tail is %d — the tail is not spliced verbatim", tmpl, len(tmplLines)-te, genPath, len(genLines)-ge)
		}

		var mismatches []string
		compare := func(ti, gi int) {
			if tmplCode[ti] == genCode[gi] {
				return
			}
			reading := "prose in the template, CODE in the generated copy"
			if tmplCode[ti] {
				reading = "code in the template, PROSE in the generated copy"
			}
			mismatches = append(mismatches, fmt.Sprintf("template line %d / generated line %d: %s — %q", ti+1, gi+1, reading, tmplLines[ti]))
		}
		for i := te; i < len(tmplLines); i++ {
			compare(i, ge+(i-te))
		}
		if len(mismatches) > 0 {
			shown := mismatches
			if len(shown) > 5 {
				shown = shown[:5]
			}
			t.Errorf("%s: %d line(s) the template owns read differently in the generated copy — a fenced block opened in the generated region and never closed:\n  %s",
				genPath, len(mismatches), strings.Join(shown, "\n  "))
		}
	}
}

// The gate above compares a code READING, and a reading is only as good as
// the lines it is read over. This is that half, on its own: the corruption
// the gate exists to catch, written with the line ending a walk on line
// feeds alone cannot see.
//
// A lone carriage return is a line break for the renderer that serves these
// files (internal/mdcode.Lines, measured there), so the delimiter after one
// sits at column 0 and opens a block that swallows the owning bot's routing
// ritual. Split on line feeds, the whole thing is ONE line that no mask
// blanks whole — the gate's verdict is "prose", over text the page shows as
// code.
func TestTheGatesCodeReadingBreaksLinesWhereTheRendererDoes(t *testing.T) {
	const body = "prose the template owns\r```\rthe ritual: NEVER invent a bot name\r"
	lines, code := codeReading(t, body)

	// Four lines: the three the carriage returns delimit and the empty one
	// the trailing terminator leaves.
	if len(lines) != 4 {
		t.Fatalf("the gate reads %d line(s) in %q, the renderer reads 4 — a walk that cannot see a carriage return compares a mask against the wrong line", len(lines), body)
	}
	if code[0] {
		t.Errorf("line 1 %q reads as code; it is the template's own prose, above the delimiter", lines[0])
	}
	for _, i := range []int{1, 2} {
		if !code[i] {
			t.Errorf("line %d %q reads as PROSE; the delimiter opened a block the text never closes, so the page shows it as code", i+1, lines[i])
		}
	}
}

// bundleManifestRoot and exampleManifestRoot are the two manifest roots of
// the shipped corpus. The recursive walk this replaces SkipDir'd a missing
// examples/, and its one floor counted BOTH trees together — a rename that
// stopped examples/ matching left this guard green over bots/ alone, which
// still cleared the total. catalogRootFiles floors each root on its own. A
// bundle nested one level deeper than today's flat layout is the routing
// guard's business — TestEveryRoutableBotIsAShippedWorkflow forces its
// workflows into the corpus or names them — so a manifest whose bundle
// escapes these roots cannot escape that guard either.
var (
	bundleManifestRoot  = catalogRoot{"*/manifest.yaml", 30, "every bundle manifest under bots/"}
	exampleManifestRoot = catalogRoot{"../examples/*/manifest.yaml", 2, "every example bundle's manifest"}
)

func bundleManifests(t *testing.T) []string {
	return append(catalogRootFiles(t, bundleManifestRoot), catalogRootFiles(t, exampleManifestRoot)...)
}

// TestBotManifestProseLeavesNoFenceOpen fails when a shipped bundle's
// description or when_to_use opens a fenced code block it never closes.
// The catalogue generator escapes such an opener so the artefact stays
// sound whatever a bundle ships, but a manifest of THIS tree must not need
// that repair: the field is word-wrapped prose, and a line that begins with
// a fence marker is a wrap accident, not a code block anyone meant.
func TestBotManifestProseLeavesNoFenceOpen(t *testing.T) {
	manifests := bundleManifests(t)
	// The named bundle is the one whose description carried this defect — a
	// walk that misses it is answering about a tree nobody ships. The floors
	// live on the two roots (bundleManifestRoot/exampleManifestRoot).
	if !slices.Contains(manifests, filepath.Join("review-pr", "manifest.yaml")) {
		t.Fatalf("the walk did not reach review-pr/manifest.yaml; it found %v", manifests)
	}
	for _, path := range manifests {
		m, err := bundle.LoadManifest(path)
		if err != nil {
			t.Errorf("%s: load manifest: %v", path, err)
			continue
		}
		if m == nil {
			continue
		}
		// The two fields the card splices as BLOCKS of their own: no line of
		// them may leave a fenced block open. The lines are the ones a
		// renderer reads (internal/mdcode.Lines) — a field whose break is a
		// lone carriage return has that break on the page, and a gate
		// splitting on line feeds alone would pass the very manifest that
		// corrupts the catalogue.
		for _, field := range []struct{ name, text string }{
			{"description", m.Description},
			{"when_to_use", m.WhenToUse},
		} {
			var fence mdcode.FenceScanner
			openedAt := 0
			for i, line := range mdcode.Lines(field.text) {
				was := fence.Open()
				fence.Code(line)
				if !was && fence.Open() {
					openedAt = i + 1
				}
			}
			if fence.Open() {
				t.Errorf("%s: %s line %d opens a fenced code block the field never closes — rewrap the prose so no line begins with a fence marker", path, field.name, openedAt)
			}
		}
		// And the values the card splices INSIDE a line — a persona table
		// cell, a card heading, a bullet. A line break in one of them ends
		// the line the generator wrote and puts the rest of the value at
		// column 0, where a delimiter run opens a block over the cards
		// below. The generator collapses them (internal/mdcode.Inline);
		// a manifest of THIS tree must not need that either, and the list
		// tracks what pkg/botregistry.renderCatalogCard embeds.
		inline := []struct{ name, text string }{
			{"name", m.Name},
			{"display_name", m.DisplayName},
			{"category", m.Category},
		}
		for i, v := range m.Triggers {
			inline = append(inline, struct{ name, text string }{fmt.Sprintf("triggers[%d]", i), v})
		}
		for i, v := range m.Tags {
			inline = append(inline, struct{ name, text string }{fmt.Sprintf("tags[%d]", i), v})
		}
		for i, v := range m.Capabilities {
			inline = append(inline, struct{ name, text string }{fmt.Sprintf("capabilities[%d]", i), v})
		}
		for _, field := range inline {
			if strings.ContainsAny(field.text, "\r\n") {
				t.Errorf("%s: %s carries a line break (%q) — the catalogue splices it inside a line, where the rest of the value would land at column 0", path, field.name, field.text)
			}
		}
	}
}
