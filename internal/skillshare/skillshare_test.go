package skillshare_test

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/internal/skillshare"
)

// repoRoot is where this package sits relative to the module root.
const repoRoot = "../.."

// The gate. Every skill several bundles carry is declared, every mirror reads
// its source, every variant still diverges, and no declared copy has moved
// away from its reviewed pin. It rides the required `test` check, so a drifted
// skill fails the same run as a broken unit test and no separate CI job can be
// forgotten.
func TestSharedSkillsAgreeWithTheirFamilies(t *testing.T) {
	findings, err := skillshare.Repo(repoRoot).Check()
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) > 0 {
		var b strings.Builder
		for _, f := range findings {
			b.WriteString("\n  " + f.String())
		}
		t.Fatalf("%d shared skills disagree with their declared families — run `task skills:sync`, or `task skills:bless` for a reviewed edit:%s", len(findings), b.String())
	}
}

// Every declared copy must RESOLVE to a file the gate then reads. Read skips a
// path that does not exist, so a family whose members have moved would shrink
// the corpus the gate covers while the gate itself stayed green — the "it
// proved one of three" failure. The completeness half (a shared skill no family
// declares) is the gate's own finding, proved in TestTheSharingGateBites; this
// is the other direction, and it counts against the declaration rather than
// against a number somebody has to remember to bump.
func TestEveryDeclaredCopyResolvesToAFileTheGateReads(t *testing.T) {
	entries, err := skillshare.Repo(repoRoot).Read()
	if err != nil {
		t.Fatal(err)
	}
	read := map[string]bool{}
	perFamily := map[string]int{}
	for _, e := range entries {
		read[e.Path] = true
		perFamily[e.Family]++
	}
	families := skillshare.Families()
	if len(families) == 0 {
		t.Fatal("the declaration is empty — the gate covers nothing")
	}
	for _, f := range families {
		for _, bundle := range f.Members() {
			if !read[f.Path(bundle)] {
				t.Errorf("family %s declares %s but Read() produced no entry for %s — that copy is outside the gate",
					f.Name, bundle, f.Path(bundle))
			}
		}
		if perFamily[f.Name] < 2 {
			t.Errorf("family %s resolved to %d copies — a family of one is not shared, and guards nothing",
				f.Name, perFamily[f.Name])
		}
	}
}

// A gate that cannot redden is not a gate, and it has to redden for EVERY way
// the tree can disagree — a guard whose generator silently repairs the defect
// before the assertion runs is the classic green no-op. Each subtest breaks one
// thing in a synthetic catalogue and demands the finding name the right file.
func TestTheSharingGateBites(t *testing.T) {
	t.Run("a clean synthetic catalogue has no findings", func(t *testing.T) {
		cat := syntheticCatalog(t)
		assertNoFindings(t, cat)
	})

	t.Run("a drifted mirror reddens, naming the mirror and its source", func(t *testing.T) {
		cat := syntheticCatalog(t)
		writeTreeFile(t, cat.Root, "bots/beta/skills/shared.md", "# Shared\n\nAn edit nobody copied to the source.\n")
		f := findingFor(t, mustCheck(t, cat), "bots/beta/skills/shared.md")
		if !strings.Contains(f.Problem, "drifted from its source") || !strings.Contains(f.Problem, "bots/alpha/skills/shared.md") {
			t.Fatalf("the finding must name the drift and the source it owes: %q", f.Problem)
		}
		if f.Family != "shared.md" {
			t.Fatalf("the finding must name the family, got %q", f.Family)
		}
	})

	t.Run("a variant collapsed onto its source reddens, carrying the lost reason", func(t *testing.T) {
		cat := syntheticCatalog(t)
		writeTreeFile(t, cat.Root, "bots/gamma/skills/shared.md", readTreeFile(t, cat.Root, "bots/alpha/skills/shared.md"))
		f := findingFor(t, mustCheck(t, cat), "bots/gamma/skills/shared.md")
		if !strings.Contains(f.Problem, "declare it a mirror instead") {
			t.Fatalf("a variant that no longer diverges must be reported as a mirror to declare: %q", f.Problem)
		}
		if !strings.Contains(f.Problem, "the gamma delta") {
			t.Fatalf("the finding must carry the declared reason, so a reader sees what was lost: %q", f.Problem)
		}
	})

	// The reason variants are pinned at all: no generator rewrites a variant,
	// so without a pin an edit to one is invisible. This is the assertion that
	// keeps "declared variant" from meaning "exempt forever".
	t.Run("an unreviewed edit to a VARIANT reddens against its pin", func(t *testing.T) {
		cat := syntheticCatalog(t)
		writeTreeFile(t, cat.Root, "bots/gamma/skills/shared.md", "# Shared\n\nthe gamma delta, quietly rewritten.\n")
		f := findingFor(t, mustCheck(t, cat), "bots/gamma/skills/shared.md")
		if !strings.Contains(f.Problem, "task skills:bless") {
			t.Fatalf("an unreviewed edit to a variant must point at the re-pin gesture: %q", f.Problem)
		}
	})

	t.Run("an edit to the SOURCE reddens against the pins even after a sync", func(t *testing.T) {
		cat := syntheticCatalog(t)
		writeTreeFile(t, cat.Root, "bots/alpha/skills/shared.md", "# Shared\n\nA source edit.\n")
		if _, err := cat.Sync(); err != nil {
			t.Fatal(err)
		}
		// The sync repaired the mirror, so no drift finding stands; the pins
		// are what still report the change for review.
		findings := mustCheck(t, cat)
		for _, want := range []string{"bots/alpha/skills/shared.md", "bots/beta/skills/shared.md"} {
			if f := findingFor(t, findings, want); !strings.Contains(f.Problem, "task skills:bless") {
				t.Fatalf("%s must be reported against its pin: %q", want, f.Problem)
			}
		}
	})

	t.Run("a declared copy that is gone reddens", func(t *testing.T) {
		cat := syntheticCatalog(t)
		if err := os.Remove(filepath.Join(cat.Root, filepath.FromSlash("bots/beta/skills/shared.md"))); err != nil {
			t.Fatal(err)
		}
		gittest.Run(t, cat.Root, "add", "-A")
		f := findingFor(t, mustCheck(t, cat), "bots/beta/skills/shared.md")
		if !strings.Contains(f.Problem, "unreadable") && !strings.Contains(f.Problem, "does not carry that file") {
			t.Fatalf("a missing declared copy must be reported: %q", f.Problem)
		}
	})

	// Completeness. A new duplicated skill that no family covers is exactly
	// the hole this package closes, so landing one must not be quiet.
	t.Run("a skill two bundles carry that no family declares reddens", func(t *testing.T) {
		cat := syntheticCatalog(t)
		writeTreeFile(t, cat.Root, "bots/alpha/skills/newcomer.md", "# Newcomer\n\nFirst copy.\n")
		writeTreeFile(t, cat.Root, "bots/beta/skills/newcomer.md", "# Newcomer\n\nSecond copy.\n")
		gittest.Run(t, cat.Root, "add", "-A")
		f := findingFor(t, mustCheck(t, cat), "bots/*/skills/newcomer.md")
		if !strings.Contains(f.Problem, "no family declares it") {
			t.Fatalf("an undeclared shared skill must be reported: %q", f.Problem)
		}
		if !strings.Contains(f.Problem, "alpha") || !strings.Contains(f.Problem, "beta") {
			t.Fatalf("the finding must name the bundles that carry it: %q", f.Problem)
		}
	})

	t.Run("a new bundle joining a declared family without being a member reddens", func(t *testing.T) {
		cat := syntheticCatalog(t)
		writeTreeFile(t, cat.Root, "bots/delta/skills/shared.md", "# Shared\n\nA fourth copy nobody declared.\n")
		gittest.Run(t, cat.Root, "add", "-A")
		f := findingFor(t, mustCheck(t, cat), "bots/delta/skills/shared.md")
		if !strings.Contains(f.Problem, "without being a member of its family") {
			t.Fatalf("an undeclared member must be reported: %q", f.Problem)
		}
	})

	// An untracked draft is invisible, the way an untracked doc does not stale
	// a map: a gate must not report whatever a local experiment left behind.
	t.Run("an untracked duplicate does not redden", func(t *testing.T) {
		cat := syntheticCatalog(t)
		writeTreeFile(t, cat.Root, "bots/alpha/skills/draft.md", "# Draft\n\nNot ready.\n")
		writeTreeFile(t, cat.Root, "bots/beta/skills/draft.md", "# Draft\n\nNot ready either.\n")
		assertNoFindings(t, cat)
	})
}

// The generator half: Sync writes a drifted mirror back from its source, says
// which path it rewrote, and leaves a clean tree alone. Muting its write — the
// mutation this test exists to catch — fails the first case here AND leaves the
// drift finding standing in the gate above.
func TestTheGeneratorWritesEveryMirrorFromItsSource(t *testing.T) {
	cat := syntheticCatalog(t)
	const mirror = "bots/beta/skills/shared.md"
	writeTreeFile(t, cat.Root, mirror, "# Shared\n\nDrift the generator owes a repair.\n")

	written, err := cat.Sync()
	if err != nil {
		t.Fatal(err)
	}
	if len(written) != 1 || written[0] != mirror {
		t.Fatalf("Sync rewrote %v, want exactly [%s] — a generator that reports no write did not make one", written, mirror)
	}
	if got, want := readTreeFile(t, cat.Root, mirror), readTreeFile(t, cat.Root, "bots/alpha/skills/shared.md"); got != want {
		t.Fatalf("the mirror still does not read its source after a sync:\n%s", got)
	}

	again, err := cat.Sync()
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("a second sync rewrote %v — the generator is not idempotent and every `task gen` would dirty the tree", again)
	}

	// A variant is never written: that is what makes its pin the only way an
	// edit to it is seen.
	if body := readTreeFile(t, cat.Root, "bots/gamma/skills/shared.md"); !strings.Contains(body, "the gamma delta") {
		t.Fatalf("Sync flattened a declared variant onto its source — the delta is gone:\n%s", body)
	}
}

// Bless pins what the tree reads, and the pins it writes are the ones the gate
// accepts: a re-pin over a reviewed edit must make the gate green, or the
// gesture the findings point at does not work.
func TestBlessRepinsTheTreeAndSatisfiesTheGate(t *testing.T) {
	cat := syntheticCatalog(t)
	writeTreeFile(t, cat.Root, "bots/gamma/skills/shared.md", "# Shared\n\nthe gamma delta, reviewed and rewritten.\n")
	if findings, err := cat.Check(); err != nil || len(findings) == 0 {
		t.Fatalf("the edit must redden before it is blessed, or this test proves nothing: findings=%v err=%v", findings, err)
	}
	if _, err := cat.Bless(); err != nil {
		t.Fatal(err)
	}
	assertNoFindings(t, cat)
}

// Declaration sanity: a family that names a bundle twice, or lists its own
// source as a mirror, would make one of the two roles unreachable.
func TestEveryFamilyIsWellFormed(t *testing.T) {
	names := map[string]bool{}
	for _, f := range skillshare.Families() {
		if !strings.HasSuffix(f.Name, ".md") {
			t.Errorf("family %q is not a markdown file name", f.Name)
		}
		if names[f.Name] {
			t.Errorf("family %q is declared twice — the second declaration would never be read", f.Name)
		}
		names[f.Name] = true
		if f.Source == "" {
			t.Errorf("family %q declares no source", f.Name)
		}
		if len(f.Mirrors)+len(f.Variants) == 0 {
			t.Errorf("family %q has a source and nothing else — it is not shared", f.Name)
		}
		seen := map[string]bool{}
		for _, b := range f.Members() {
			if seen[b] {
				t.Errorf("family %q names bundle %q twice", f.Name, b)
			}
			seen[b] = true
		}
		for _, v := range f.Variants {
			if len(v.Reason) < 40 {
				t.Errorf("family %q variant %q gives no usable reason (%q) — a reader porting a shared fix needs to know what this copy holds",
					f.Name, v.Bundle, v.Reason)
			}
			for _, placeholder := range []string{"TODO", "FIXME", "n/a", "unknown", "see above"} {
				if strings.Contains(v.Reason, placeholder) {
					t.Errorf("family %q variant %q defers its reason to %q — a declared variant freezes the drift, so the reason is the review",
						f.Name, v.Bundle, placeholder)
				}
			}
		}
	}
}

// The pins are a committed artifact: a line that cannot be parsed back, or a
// path pinned twice, would let a second pin silently win.
func TestThePinsRoundTrip(t *testing.T) {
	entries, err := skillshare.Repo(repoRoot).Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < 2 {
		t.Fatalf("read %d declared copies — the declaration is not being read", len(entries))
	}
	pins, err := skillshare.Parse(skillshare.Format(entries))
	if err != nil {
		t.Fatal(err)
	}
	if len(pins) != len(entries) {
		t.Fatalf("formatting %d entries parsed back as %d pins", len(entries), len(pins))
	}
	for _, e := range entries {
		if pins[e.Path] != e.Pin() {
			t.Errorf("%s round-tripped as %q, want %q", e.Path, pins[e.Path], e.Pin())
		}
	}
	const zero = "0000000000000000000000000000000000000000000000000000000000000000"
	for _, bad := range []string{
		"mirror deadbeef bots/a/skills/x.md",
		"sideways " + zero + " bots/a/skills/x.md",
		"mirror " + zero + " bots/a/skills/x.md\nsource " + zero + " bots/a/skills/x.md",
		"mirror " + zero,
	} {
		if _, err := skillshare.Parse([]byte(bad)); err == nil {
			t.Errorf("Parse accepted %q", bad)
		}
	}
}

// syntheticCatalog writes a miniature catalogue and declares it the way the
// real one is declared: one family with a source, a mirror and a variant, plus
// a skill only one bundle carries.
func syntheticCatalog(t *testing.T) skillshare.Catalog {
	t.Helper()
	root := t.TempDir()
	const body = "# Shared\n\nThe source of truth.\n"
	writeTreeFile(t, root, "bots/alpha/skills/shared.md", body)
	writeTreeFile(t, root, "bots/beta/skills/shared.md", body)
	writeTreeFile(t, root, "bots/gamma/skills/shared.md", body+"\nPlus the gamma delta.\n")
	writeTreeFile(t, root, "bots/alpha/skills/solo.md", "# Solo\n\nOnly alpha carries this.\n")
	gittest.Run(t, root, "init")
	gittest.Run(t, root, "add", ".")
	cat := skillshare.Catalog{
		Root: root,
		Families: []skillshare.Family{{
			Name:    "shared.md",
			Source:  "alpha",
			Mirrors: []string{"beta"},
			Variants: []skillshare.Variant{{
				Bundle: "gamma",
				Reason: "carries the gamma delta, which the source must not: a long enough reason to pass the declaration's own floor",
			}},
		}},
	}
	if _, err := cat.Bless(); err != nil {
		t.Fatal(err)
	}
	return cat
}

func writeTreeFile(t *testing.T, root, rel, body string) {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readTreeFile(t *testing.T, root, rel string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func mustCheck(t *testing.T, cat skillshare.Catalog) []skillshare.Finding {
	t.Helper()
	findings, err := cat.Check()
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) == 0 {
		t.Fatal("the gate found nothing — it cannot redden for this defect")
	}
	return findings
}

func assertNoFindings(t *testing.T, cat skillshare.Catalog) {
	t.Helper()
	findings, err := cat.Check()
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		var got []string
		for _, f := range findings {
			got = append(got, f.String())
		}
		sort.Strings(got)
		t.Fatalf("want no findings, got:\n  %s", strings.Join(got, "\n  "))
	}
}

// findingFor returns the finding for a path, failing with every finding it did
// see — a subtest that matched the wrong path proves nothing about the defect
// it broke.
func findingFor(t *testing.T, findings []skillshare.Finding, path string) skillshare.Finding {
	t.Helper()
	for _, f := range findings {
		if f.Path == path {
			return f
		}
	}
	var got []string
	for _, f := range findings {
		got = append(got, f.Path)
	}
	t.Fatalf("no finding for %s; the gate reported %v", path, got)
	return skillshare.Finding{}
}
