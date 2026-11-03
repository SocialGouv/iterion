package docsguard

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// docs/references/dsl-authoring-probe.md is a protocol meant to be RUN as
// written: each spec tells an agent which file to open and which structure to
// edit, and its numbers are only comparable to an earlier probe's when the
// spec asked the same thing. A spec citing a tree that has moved is therefore
// not a stale sentence, it is an unrunnable protocol — spec 2 opened
// `bots/review-pr/main.bot` as "about 1 600 lines" and asked for a diff
// touching only that file, which lot 3's `import` split in four and made
// impossible to satisfy while the page still read as current.
//
// So a spec that opens a file of this repository declares its targets in one
// machine-readable place — a `file | lines | holds` table — and this guard
// reads them against the tree: the path exists, the count is within
// authoringProbeCountSlack of `wc -l`, and every structure a `holds` cell
// names is still in that file. A spec's `break` table gets the same
// treatment: the text each break edits must be a literal of one of the spec's
// targets, which is what a break aimed at a construct its file does not carry
// fails on (a quoted ref in a `command:`, in a file with no tool node).
//
// Scope, the seam's own doctrine: the SPEC sections are guidance and are in
// reach; the dated measured rows below them report what was observed on their
// date, and an observation is not guidance. The scoping is by section, since
// both live in one page.
//
// What it leaves out, deliberately — a guard that misses is acceptable and one
// that lies is not: it reads the page as these tables are written and is no
// parser of markdown. A target stated in a sentence rather than a table goes
// unchecked, except for a line count, which is refused in prose precisely
// because prose is the form spec 2 rotted in.

const authoringProbePagePath = "docs/references/dsl-authoring-probe.md"

// authoringProbeSpecsWithoutATarget are the specs that open no file of this
// repository: spec 1 asks for a bot written from scratch. Each is re-checked
// against the page's headings, so a renamed section fails rather than
// silently widening the exemption.
var authoringProbeSpecsWithoutATarget = []string{"Spec 1 — creation"}

// authoringProbeCountSlack is how far a claimed line count may sit from the
// file's own: a quarter of the claim, never under 100 lines. The page says
// "about", and a guard firing on an ordinary edit to a 1 300-line bot would be
// tuned off within a week — while 1 600 claimed against 720 real, the drift
// this guard exists for, is caught by a factor of two.
func authoringProbeCountSlack(claimed int) int { return max(100, claimed/4) }

var (
	authoringProbeSpecHeadingRe = regexp.MustCompile(`^### Spec `)
	authoringProbeHeadingRe     = regexp.MustCompile(`^#{1,3} `)
	authoringProbeCodeSpanRe    = regexp.MustCompile("`([^`]+)`")
	// A code span spelling a path of this repository: slash-separated, no
	// space. A span holding a glob is not a path and is left alone.
	authoringProbePathRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+(?:/[A-Za-z0-9_.*-]+)+$`)
	// A line count stated next to a path, within one sentence of it — the
	// form "about 1 600 lines" was written in.
	authoringProbeProseCountRe = regexp.MustCompile("`([A-Za-z0-9_.-]+(?:/[A-Za-z0-9_.*-]+)+)`[^.;]{0,80}?[0-9][0-9 \\x{00a0}]* lines")
)

type authoringProbeSpec struct {
	heading string
	body    string
}

// authoringProbeSpecs splits the page into its `### Spec …` sections, each
// running to the next heading of any level. A `#` inside a fenced block is
// shell, not a heading.
func authoringProbeSpecs(page string) []authoringProbeSpec {
	var specs []authoringProbeSpec
	var cur *authoringProbeSpec
	var body strings.Builder
	fenced := false
	flush := func() {
		if cur != nil {
			cur.body = body.String()
			specs = append(specs, *cur)
			cur = nil
			body.Reset()
		}
	}
	for _, line := range strings.Split(page, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			fenced = !fenced
		} else if !fenced && authoringProbeHeadingRe.MatchString(line) {
			flush()
			if authoringProbeSpecHeadingRe.MatchString(line) {
				cur = &authoringProbeSpec{heading: strings.TrimSpace(line)}
			}
			continue
		}
		if cur != nil {
			body.WriteString(line)
			body.WriteString("\n")
		}
	}
	flush()
	return specs
}

type authoringProbeTarget struct {
	path  string
	lines int
	holds []string
}

// authoringProbeTables returns the markdown tables of a section as rows of
// trimmed cells: a table is a run of lines opening with `|`.
func authoringProbeTables(body string) [][][]string {
	var tables [][][]string
	var rows [][]string
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "|") {
			if len(rows) > 0 {
				tables = append(tables, rows)
				rows = nil
			}
			continue
		}
		cells := strings.Split(strings.Trim(trimmed, "|"), "|")
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		rows = append(rows, cells)
	}
	if len(rows) > 0 {
		tables = append(tables, rows)
	}
	return tables
}

// authoringProbeSpans returns the code spans of a cell, in order.
func authoringProbeSpans(cell string) []string {
	var spans []string
	for _, m := range authoringProbeCodeSpanRe.FindAllStringSubmatch(cell, -1) {
		spans = append(spans, m[1])
	}
	return spans
}

// authoringProbeCount reads a line count written with the page's thousands
// spacing ("1 329"). A cell holding no number reads as -1, which the guard
// reports rather than passing.
func authoringProbeCount(cell string) int {
	digits := strings.Map(func(r rune) rune {
		switch r {
		case ' ', ' ', ',', '_':
			return -1
		}
		return r
	}, strings.TrimSpace(cell))
	n, err := strconv.Atoi(digits)
	if err != nil {
		return -1
	}
	return n
}

// authoringProbeTargets reads the `file | lines | holds` tables of a spec
// section. A table headed otherwise is not one — which is how a dated
// measured table stays out of reach even when one lands inside a section.
func authoringProbeTargets(body string) []authoringProbeTarget {
	var targets []authoringProbeTarget
	for _, rows := range authoringProbeTables(body) {
		if len(rows[0]) < 3 || rows[0][0] != "file" {
			continue
		}
		for _, row := range rows[1:] {
			if len(row) < 3 {
				continue
			}
			spans := authoringProbeSpans(row[0])
			if len(spans) == 0 {
				continue
			}
			targets = append(targets, authoringProbeTarget{
				path:  spans[0],
				lines: authoringProbeCount(row[1]),
				holds: authoringProbeSpans(row[2]),
			})
		}
	}
	return targets
}

// authoringProbeBreaks reads the `break | the edit | code` table of a spec
// section: the first code span of the edit cell is the text that break edits.
func authoringProbeBreaks(body string) []string {
	var breaks []string
	for _, rows := range authoringProbeTables(body) {
		if len(rows[0]) < 2 || rows[0][0] != "break" {
			continue
		}
		for _, row := range rows[1:] {
			if len(row) < 2 {
				continue
			}
			if spans := authoringProbeSpans(row[1]); len(spans) > 0 {
				breaks = append(breaks, spans[0])
			}
		}
	}
	return breaks
}

// authoringProbePaths returns the repository paths a section's code spans
// spell, tables included.
func authoringProbePaths(body string) []string {
	var paths []string
	for _, span := range authoringProbeSpans(body) {
		if strings.Contains(span, "*") || !authoringProbePathRe.MatchString(span) {
			continue
		}
		paths = append(paths, span)
	}
	return paths
}

// authoringProbeProseCounts returns the line counts a section states in prose
// next to a path — table rows excluded, since the tables are where this guard
// reads a count. Each hit is returned as the sentence fragment that carries
// it, so the failure prints what to move.
func authoringProbeProseCounts(body string) []string {
	var prose []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "|") {
			continue
		}
		prose = append(prose, line)
	}
	flat := strings.Join(strings.Fields(strings.Join(prose, " ")), " ")
	return authoringProbeProseCountRe.FindAllString(flat, -1)
}

func TestAuthoringProbeSpecsNameTargetsThatExist(t *testing.T) {
	root := filepath.Join("..", "..")
	page := forgeProseSource(t, "docs", "references", "dsl-authoring-probe.md")
	specs := authoringProbeSpecs(page)
	if len(specs) == 0 {
		t.Fatalf("%s carries no `### Spec` section — the protocol's specs were renamed or moved; move this guard with them rather than leave every spec unchecked", authoringProbePagePath)
	}
	exempt := func(heading string) bool {
		return slices.ContainsFunc(authoringProbeSpecsWithoutATarget, func(name string) bool {
			return strings.Contains(heading, name)
		})
	}
	for _, name := range authoringProbeSpecsWithoutATarget {
		if !slices.ContainsFunc(specs, func(s authoringProbeSpec) bool { return strings.Contains(s.heading, name) }) {
			t.Fatalf("no section of %s is headed %q, which this guard exempts from declaring a target table — drop the exemption rather than leave it fictional", authoringProbePagePath, name)
		}
	}
	if !authoringProbeProseCountRe.MatchString("Open `bots/review-pr/main.bot` (about 1 600 lines). Add") {
		t.Fatalf("the matcher no longer reads the stale form it exists to refuse (a count stated next to a path in prose) — every spec below is unchecked on it")
	}

	checked := 0
	for _, spec := range specs {
		targets := authoringProbeTargets(spec.body)
		if len(targets) == 0 && !exempt(spec.heading) {
			t.Errorf("%s: %q declares no `file | lines | holds` table — a spec that opens a file of this repository names it there, so a refactor turns this guard red instead of rotting the protocol", authoringProbePagePath, spec.heading)
		}
		declared := make(map[string]string, len(targets))
		claimed := make(map[string]bool, len(targets))
		for _, target := range targets {
			claimed[target.path] = true
			path := filepath.Join(root, filepath.FromSlash(target.path))
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Errorf("%s: %q opens `%s`: %v — the spec's target moved; re-point the spec at the file that holds the structure, as the protocol cannot be run on a path that is not there", authoringProbePagePath, spec.heading, target.path, err)
				continue
			}
			source := string(raw)
			declared[target.path] = source
			lines := strings.Count(source, "\n")
			if !strings.HasSuffix(source, "\n") {
				lines++
			}
			switch {
			case target.lines < 0:
				t.Errorf("%s: %q gives `%s` no line count — the second column of the target table is the count, which is what tells a reader the file is still the size the spec was written for", authoringProbePagePath, spec.heading, target.path)
			case abs(target.lines-lines) > authoringProbeCountSlack(target.lines):
				t.Errorf("%s: %q claims `%s` is %d lines; it is %d (slack %d) — the spec was written for a file of another size, so its bounds and its "+
					"\"touches only\" items no longer describe the edit it asks for", authoringProbePagePath, spec.heading, target.path, target.lines, lines, authoringProbeCountSlack(target.lines))
			}
			if len(target.holds) == 0 {
				t.Errorf("%s: %q names `%s` without one structure it holds — the third column is what the spec rests on, and an empty one leaves the spec unchecked", authoringProbePagePath, spec.heading, target.path)
			}
			for _, holds := range target.holds {
				checked++
				if !strings.Contains(source, holds) {
					t.Errorf("%s: %q says `%s` holds `%s`, which is no longer in that file — the structure the spec tells the agent to edit moved; re-point the spec at where it lives", authoringProbePagePath, spec.heading, target.path, holds)
				}
			}
		}
		for _, path := range authoringProbePaths(spec.body) {
			if claimed[path] {
				continue
			}
			if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(path))); err != nil {
				t.Errorf("%s: %q names `%s`, which is not in the tree (%v) — a spec cites a path an agent is meant to open", authoringProbePagePath, spec.heading, path, err)
			}
		}
		for _, hit := range authoringProbeProseCounts(spec.body) {
			t.Errorf("%s: %q states a line count in prose — %q — where nothing reads it; move it to the spec's `file | lines | holds` table, which this guard holds against the tree", authoringProbePagePath, spec.heading, hit)
		}
		for _, edit := range authoringProbeBreaks(spec.body) {
			if len(declared) == 0 {
				break
			}
			checked++
			sited := false
			for _, source := range declared {
				if strings.Contains(source, edit) {
					sited = true
					break
				}
			}
			if !sited {
				t.Errorf("%s: %q breaks `%s`, which is in none of the spec's target files — a break with no site cannot be injected, and the checklist counting its diagnostic can never be met", authoringProbePagePath, spec.heading, edit)
			}
		}
	}
	if checked == 0 {
		t.Fatalf("%s: no structure of any spec was read against the tree — the target tables were dropped or renamed, and this guard proved nothing", authoringProbePagePath)
	}
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// TestTheAuthoringProbeReadersRefuseAPageThatLostTheFact.
//
// The guard above rests half of its verdict on readers of the page's own
// tables. A reader answering the same whatever it is handed would make that
// half decorative — and the specs would keep their sentences the day a
// section is renamed, a table re-headed, or a count moved back into prose.
func TestTheAuthoringProbeReadersRefuseAPageThatLostTheFact(t *testing.T) {
	const page = "# Probe\n\n## The three specs\n\n### Spec 1 — creation\n\nWrite it from scratch.\n\n### Spec 2 — edition\n\n| file | lines | holds |\n|---|---|---|\n| `a/b.bot` | 1 329 | `router fan:`, `mode: fan_out_all` |\n\n| break | the edit | code |\n|---|---|---|\n| a property renamed | `merge_strategy: squash` becomes `merge_mode:` | E012 |\n\n## Baseline and targets\n\n| Probe | Model | Lines |\n|---|---|---|\n| 2026-09-09 | opus | 331 |\n"
	specs := authoringProbeSpecs(page)
	if len(specs) != 2 {
		t.Fatalf("specs misread: %d, want 2 (%+v)", len(specs), specs)
	}
	if specs[0].heading != "### Spec 1 — creation" || specs[1].heading != "### Spec 2 — edition" {
		t.Errorf("spec headings misread: %q, %q", specs[0].heading, specs[1].heading)
	}
	if strings.Contains(specs[1].body, "2026-09-09") {
		t.Errorf("a spec's body ran past its section into the dated rows: %q", specs[1].body)
	}
	if specs := authoringProbeSpecs("# Probe\n\n## Protocol\n\n1. Fresh session.\n"); len(specs) != 0 {
		t.Errorf("sections read from a page with no spec: %+v", specs)
	}
	if specs := authoringProbeSpecs("# Probe\n\n```sh\n### Spec 9 — inside a fence\n```\n"); len(specs) != 0 {
		t.Errorf("a heading inside a fenced block was read as a spec: %+v", specs)
	}

	if got := authoringProbeTargets(specs[0].body); got != nil {
		t.Errorf("targets read from a spec that declares no table: %+v", got)
	}
	targets := authoringProbeTargets(specs[1].body)
	if len(targets) != 1 || targets[0].path != "a/b.bot" || targets[0].lines != 1329 ||
		!slices.Equal(targets[0].holds, []string{"router fan:", "mode: fan_out_all"}) {
		t.Fatalf("target misread: %+v", targets)
	}
	// The dated measured table is out of reach even when it sits in a spec
	// section: an observation is not guidance.
	dated := "| Probe | Model | Lines |\n|---|---|---|\n| 2026-09-09 | `claude-opus-4-8` | 331 |\n"
	if got := authoringProbeTargets(dated); got != nil {
		t.Errorf("a dated measured table was read as a target table: %+v", got)
	}
	if got := authoringProbeTargets("| path | lines | holds |\n|---|---|---|\n| `a/b.bot` | 12 | `x` |\n"); got != nil {
		t.Errorf("a table headed `path` was read as a target table: %+v", got)
	}

	if got := authoringProbeBreaks(specs[1].body); !slices.Equal(got, []string{"merge_strategy: squash"}) {
		t.Errorf("break sites misread: %+v", got)
	}
	if got := authoringProbeBreaks(specs[0].body); got != nil {
		t.Errorf("break sites read from a spec with no break table: %+v", got)
	}
	if got := authoringProbeBreaks(dated); got != nil {
		t.Errorf("break sites read from a dated measured table: %+v", got)
	}

	for _, path := range []string{"bots/review-pr/main.bot", "examples/review-merge-gate.bot", "docs/dsl.md"} {
		if got := authoringProbePaths("open `" + path + "` and read it"); !slices.Equal(got, []string{path}) {
			t.Errorf("a path was not read from a code span: %q -> %+v", path, got)
		}
	}
	for _, notAPath := range []string{"release_readiness", "await: wait_all", "run.*", "git diff --stat", "iterion validate --json", "prompts/*.md", "model: anthropic/claude-sonnet-4-6", "{{outputs.x}}"} {
		if got := authoringProbePaths("the spec names `" + notAPath + "` here"); got != nil {
			t.Errorf("a span that spells no repository path was read as one: %q -> %+v", notAPath, got)
		}
	}

	if got := authoringProbeProseCounts("Open `bots/review-pr/main.bot` (about 1 600 lines). Add a lens."); len(got) != 1 {
		t.Errorf("a count stated in prose next to a path was not refused: %+v", got)
	}
	if got := authoringProbeProseCounts("Open `bots/review-pr/main.bot`\n(about 1 600 lines). Add a lens."); len(got) != 1 {
		t.Errorf("a count straddling a wrap was not refused: %+v", got)
	}
	for _, fine := range []string{
		"| `bots/review-pr/main.bot` | 720 | `workflow review_pr:` |",
		"the diff touches those four files and nothing else, and adds under 120 lines",
		"Open the `bots/review-pr` program. Since lot 3's `import` it is four files.",
		"the mid-size model read 1 113 lines, which is not a token count",
	} {
		if got := authoringProbeProseCounts(fine); got != nil {
			t.Errorf("a count this guard has no claim on was refused: %q -> %+v", fine, got)
		}
	}

	if got := authoringProbeCount("1 329"); got != 1329 {
		t.Errorf("the page's thousands spacing misread: %d", got)
	}
	if got := authoringProbeCount("about"); got != -1 {
		t.Errorf("a cell holding no number read as a count: %d", got)
	}
	if got, want := authoringProbeCountSlack(1600), 400; got != want {
		t.Errorf("slack for a 1 600-line claim: %d, want %d", got, want)
	}
	if got, want := authoringProbeCountSlack(123), 100; got != want {
		t.Errorf("slack for a small file: %d, want %d (the floor)", got, want)
	}
}
