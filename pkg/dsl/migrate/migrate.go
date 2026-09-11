// Package migrate rewrites a `.bot` file from syntax profile 1 to profile 2
// (ADR-098) surgically: only the bytes whose MEANING changes between the
// two profiles are re-emitted — a `dsl: 2` header goes in, the profile-1
// strict-escape directive comes out, every quoted literal that holds a
// backslash is re-spelled from its profile-1 value — and everything else,
// comments, blank lines, order, indentation, a BOM, the line endings, is
// byte-identical. The parser drops the comments inside a block and the
// unparser hoists the ones at the top, so a rewrite through them would
// erase a bot's documentation; this one never reads the text back through
// the writer.
//
// The result is proven before it is handed back: both texts parse clean,
// and read as the same document — the span-free JSON mirror of the AST,
// compared after the profile and the paragraph breaks that profile 2 keeps
// in a named prompt are set aside — and the file's catalogue identity (its
// `## ---` frontmatter, read on the ORIGINAL bytes) is unchanged. A named
// prompt whose rendering changes under profile 2 (it held blank lines) is
// reported by name; it is what migrating means, and the caller may refuse
// it (Options.StrictPrompts). `project_root:` has no profile-2 form: the
// file is refused with the remedy, never rewritten by guesswork.
package migrate

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unparse"
)

// Options tunes a migration.
type Options struct {
	// To is the target profile; 0 means parser.MaxProfile.
	To int
	// StrictPrompts refuses a file in which a named prompt's rendering
	// changes (a paragraph break profile 2 keeps), instead of reporting it.
	StrictPrompts bool
}

// Change is one rewritten place of the text.
type Change struct {
	// Kind is "header" (the `dsl:` line inserted or replaced), "directive"
	// (a strict-escape directive line removed) or "literal" (a quoted
	// string re-spelled).
	Kind string
	Line int
	// From and To are the text before and after, for the report.
	From, To string
}

// PromptChange is a named prompt whose rendering changes under the target
// profile: it holds blank lines, which profile 1 dropped and profile 2
// keeps.
type PromptChange struct {
	Name       string
	Line       int
	BlankLines int
}

// Result is a migration's outcome; nothing has been written.
type Result struct {
	Name     string
	Original []byte
	Migrated []byte
	// Changed reports whether Migrated differs from Original.
	Changed bool
	Changes []Change
	Prompts []PromptChange
}

// ErrRefused marks a file the migration will not rewrite; the message says
// why and what to do.
var ErrRefused = errors.New("migration refused")

// Bytes migrates one file's bytes. name is used for diagnostics only.
func Bytes(name string, src []byte, opts Options) (*Result, error) {
	to := opts.To
	if to == 0 {
		to = parser.MaxProfile
	}
	if to != 2 {
		return nil, fmt.Errorf("%w: this build migrates to profile 2 only, not %d", ErrRefused, to)
	}
	res := &Result{Name: name, Original: src}

	norm := normalize(src)
	before := parser.Parse(name, norm.text)
	if errs := parseErrors(before.Diagnostics); len(errs) > 0 {
		return nil, fmt.Errorf("%w: %s does not parse in its own profile — fix it first: %s", ErrRefused, name, strings.Join(errs, "; "))
	}
	if before.File.EffectiveProfile() >= to {
		// Already there: nothing to rewrite, and nothing to prove.
		res.Migrated = src
		return res, nil
	}
	if line := projectRootLine(before.File); line > 0 {
		return nil, fmt.Errorf("%w: %s:%d uses `project_root:`, which profile 2 removed and has no mechanical replacement (`visibility:` is a different axis, C171): keep the file in profile 1, or redesign the memory scope first", ErrRefused, name, line)
	}

	edits, changes := planEdits(norm.text, name, before.File, to)
	res.Changes = changes
	migratedNorm := applyEdits(norm.text, edits)
	res.Migrated = norm.mapBack(src, edits, migratedNorm)
	res.Changed = !bytes.Equal(res.Migrated, src)

	// The oracle: the two texts read as the same document.
	after := parser.Parse(name, normalize(res.Migrated).text)
	if errs := parseErrors(after.Diagnostics); len(errs) > 0 {
		return nil, fmt.Errorf("%w: the migrated %s does not parse in profile %d (a defect of the migration, not of the file): %s", ErrRefused, name, to, strings.Join(errs, "; "))
	}
	if after.File.EffectiveProfile() != to {
		return nil, fmt.Errorf("%w: the migrated %s reads as profile %d, not %d", ErrRefused, name, after.File.EffectiveProfile(), to)
	}
	if why := sameDocument(before.File, after.File); why != "" {
		return nil, fmt.Errorf("%w: the migration would change the program of %s: %s", ErrRefused, name, why)
	}
	if !sameCatalogueIdentity(src, res.Migrated) {
		return nil, fmt.Errorf("%w: the migration would change the catalogue identity of %s (its `## ---` frontmatter)", ErrRefused, name)
	}
	res.Prompts = promptChanges(after.File, before.File)
	if opts.StrictPrompts && len(res.Prompts) > 0 {
		names := make([]string, 0, len(res.Prompts))
		for _, p := range res.Prompts {
			names = append(names, fmt.Sprintf("%s (line %d, %d blank lines)", p.Name, p.Line, p.BlankLines))
		}
		return nil, fmt.Errorf("%w: %d prompt(s) of %s keep their paragraph breaks under profile 2 and would reach the model differently: %s — migrate without --strict-prompts to accept it", ErrRefused, len(res.Prompts), name, strings.Join(names, ", "))
	}
	return res, nil
}

// ---- planning ----

// edit replaces the normalised text in [start, end) with repl; start and
// end are byte offsets into the normalised text.
type edit struct {
	start, end int
	repl       string
}

// planEdits lists the rewrites: the header, the directive lines, the
// quoted literals that hold a backslash.
func planEdits(norm, name string, f *ast.File, to int) ([]edit, []Change) {
	var edits []edit
	var changes []Change
	lines := lineStarts(norm)
	toks := parser.NewLexer(name, norm).All()
	runeToByte := runeByteOffsets(norm)

	// The header: replace an explicit `dsl: 1`, or insert one before the
	// first significant line.
	pre := parser.ReadPreamble(norm)
	eol := "\n"
	header := fmt.Sprintf("dsl: %d", to)
	if pre.HeaderLine > 0 {
		start, end := lineSpan(norm, lines, pre.HeaderLine)
		edits = append(edits, edit{start, end, header + eol})
		changes = append(changes, Change{Kind: "header", Line: pre.HeaderLine, From: strings.TrimRight(norm[start:end], "\n"), To: header})
	} else {
		at, line := firstSignificantLine(norm, lines)
		edits = append(edits, edit{at, at, header + eol + eol})
		changes = append(changes, Change{Kind: "header", Line: line, To: header})
	}

	for _, t := range toks {
		switch t.Type {
		case parser.TokenComment:
			if !parser.IsStrictEscapeDirective(t.Value) {
				continue
			}
			start, end := lineSpan(norm, lines, t.Line)
			edits = append(edits, edit{start, end, ""})
			changes = append(changes, Change{Kind: "directive", Line: t.Line, From: strings.TrimRight(norm[start:end], "\n")})
		case parser.TokenString:
			start, end := runeToByte[t.Offset], runeToByte[t.End]
			if start >= end || norm[start] != '"' {
				continue // a raw string or a block scalar: no escape to re-spell
			}
			if !strings.Contains(norm[start:end], `\`) {
				continue // the same text in both profiles
			}
			repl := unparse.QuoteStrict(t.Value)
			edits = append(edits, edit{start, end, repl})
			changes = append(changes, Change{Kind: "literal", Line: t.Line, From: norm[start:end], To: repl})
		}
	}
	sort.SliceStable(edits, func(i, j int) bool { return edits[i].start < edits[j].start })
	return edits, changes
}

// applyEdits rewrites text; edits are sorted by start and never overlap.
func applyEdits(text string, edits []edit) string {
	var b strings.Builder
	at := 0
	for _, e := range edits {
		b.WriteString(text[at:e.start])
		b.WriteString(e.repl)
		at = e.end
	}
	b.WriteString(text[at:])
	return b.String()
}

// ---- normalisation and the way back to the original bytes ----

// normalized is the text the lexer reads — BOM stripped, CRLF folded —
// with what it takes to map an offset back to the original bytes.
type normalized struct {
	text string
	bom  int   // bytes of BOM removed at the start
	crlf []int // offsets, in text, of the '\n' that had a '\r' before it
}

func normalize(src []byte) normalized {
	n := normalized{}
	s := string(src)
	if strings.HasPrefix(s, "\ufeff") {
		n.bom = len("\ufeff")
		s = s[n.bom:]
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\r' && i+1 < len(s) && s[i+1] == '\n' {
			n.crlf = append(n.crlf, b.Len())
			continue // the '\n' that follows is written on the next turn
		}
		b.WriteByte(s[i])
	}
	n.text = b.String()
	return n
}

// origOffset maps an offset of the normalised text to the original bytes:
// the BOM, and one '\r' per folded newline strictly before it, are added
// back — so an offset ON a folded '\n' maps to its '\r', and a range ending
// there keeps the file's own line ending.
func (n normalized) origOffset(o int) int {
	k := sort.SearchInts(n.crlf, o) // folded newlines strictly before o
	return o + n.bom + k
}

// mapBack rebuilds the migrated file on the ORIGINAL bytes: everything
// outside the edited ranges is copied as it was (BOM and line endings
// included), each edit's replacement is written in place, and a
// replacement's own line endings follow the file's.
func (n normalized) mapBack(src []byte, edits []edit, _ string) []byte {
	eol := "\n"
	if len(n.crlf) > 0 {
		eol = "\r\n"
	}
	var out bytes.Buffer
	at := 0
	for _, e := range edits {
		s, t := n.origOffset(e.start), n.origOffset(e.end)
		out.Write(src[at:s])
		out.WriteString(strings.ReplaceAll(e.repl, "\n", eol))
		at = t
	}
	out.Write(src[at:])
	return out.Bytes()
}

// ---- oracles ----

// sameDocument reports why two parses are not the same document, or "":
// the span-free JSON mirror of each, once the profile is set aside and the
// named prompts of the later profile are brought back to the earlier
// profile's canonical form (the paragraph breaks it dropped are the one
// reading that changes, and are reported apart).
func sameDocument(before, after *ast.File) string {
	a, b := *before, *after
	a.Profile, b.Profile = 0, 0
	// The directive lines are the one comment the migration removes; every
	// other comment must still be there.
	a.Comments = nil
	for _, c := range before.Comments {
		if !parser.IsStrictEscapeDirective(c.Text) {
			a.Comments = append(a.Comments, c)
		}
	}
	b.Prompts = make([]*ast.PromptDecl, len(after.Prompts))
	for i, p := range after.Prompts {
		q := *p
		if !q.Inline {
			q.Body = parser.CanonicalPromptBody(q.Body)
		}
		b.Prompts[i] = &q
	}
	ja, err := ast.MarshalFile(&a)
	if err != nil {
		return "cannot compare: " + err.Error()
	}
	jb, err := ast.MarshalFile(&b)
	if err != nil {
		return "cannot compare: " + err.Error()
	}
	if bytes.Equal(ja, jb) {
		return ""
	}
	return firstDifference(ja, jb)
}

// sameCatalogueIdentity reports whether two texts carry the same `## ---`
// frontmatter, read on their ORIGINAL bytes as the catalogue reads it (a
// frontmatter behind a BOM is invisible, and must stay so). No edit of
// today's migration touches a frontmatter line, so this is the guard that
// outlives the edit rules: an edit kind added later that did would be
// refused here, not discovered on the board.
func sameCatalogueIdentity(before, after []byte) bool {
	return reflect.DeepEqual(bundle.ParseFrontmatter(before), bundle.ParseFrontmatter(after))
}

// firstDifference names the first line at which two JSON documents differ.
func firstDifference(a, b []byte) string {
	la, lb := strings.Split(string(a), "\n"), strings.Split(string(b), "\n")
	for i := 0; i < len(la) && i < len(lb); i++ {
		if la[i] != lb[i] {
			return fmt.Sprintf("before: %s | after: %s", strings.TrimSpace(la[i]), strings.TrimSpace(lb[i]))
		}
	}
	return fmt.Sprintf("the documents have %d and %d lines", len(la), len(lb))
}

// promptChanges lists the named prompts whose body is not its own
// profile-1 canonical form: what profile 2 now keeps. The line is the
// prompt's in the ORIGINAL text, the one the author is looking at.
func promptChanges(after, before *ast.File) []PromptChange {
	lineBefore := make(map[string]int, len(before.Prompts))
	for _, p := range before.Prompts {
		lineBefore[p.Name] = p.Span.Start.Line
	}
	var out []PromptChange
	for _, p := range after.Prompts {
		if p.Inline {
			continue
		}
		if canon := parser.CanonicalPromptBody(p.Body); canon != p.Body {
			blank := strings.Count(p.Body, "\n") - strings.Count(canon, "\n")
			out = append(out, PromptChange{Name: p.Name, Line: lineBefore[p.Name], BlankLines: blank})
		}
	}
	return out
}

// projectRootLine is the line of the first `project_root:` of the file, 0
// when none: on an agent's or a judge's memory block, in a group too.
func projectRootLine(f *ast.File) int {
	line := func(m *ast.MemoryBlock) int {
		if m != nil && m.ProjectRoot != nil {
			return m.Span.Start.Line
		}
		return 0
	}
	var agents []*ast.AgentDecl
	var judges []*ast.JudgeDecl
	agents = append(agents, f.Agents...)
	judges = append(judges, f.Judges...)
	for _, g := range f.Groups {
		agents = append(agents, g.Agents...)
		judges = append(judges, g.Judges...)
	}
	for _, a := range agents {
		if l := line(a.Memory); l > 0 {
			return l
		}
	}
	for _, j := range judges {
		if l := line(j.Memory); l > 0 {
			return l
		}
	}
	return 0
}

// ---- text geometry ----

func parseErrors(diags []parser.Diagnostic) []string {
	var out []string
	for _, d := range diags {
		if d.Severity == parser.SeverityError {
			out = append(out, d.Error())
		}
	}
	return out
}

// lineStarts is the byte offset of each line's first byte (line 1 at 0).
func lineStarts(text string) []int {
	starts := []int{0}
	for i := 0; i < len(text); i++ {
		if text[i] == '\n' {
			starts = append(starts, i+1)
		}
	}
	return starts
}

// lineSpan is the byte range of a 1-based line, its newline included.
func lineSpan(text string, starts []int, line int) (int, int) {
	start := starts[line-1]
	end := len(text)
	if line < len(starts) {
		end = starts[line]
	}
	return start, end
}

// firstSignificantLine is the byte offset and 1-based number of the first
// line that is neither blank nor a comment — where the header goes.
func firstSignificantLine(text string, starts []int) (int, int) {
	for i, start := range starts {
		end := len(text)
		if i+1 < len(starts) {
			end = starts[i+1]
		}
		ln := strings.TrimRight(text[start:end], "\n")
		if strings.TrimSpace(ln) == "" {
			continue
		}
		if strings.HasPrefix(strings.TrimLeft(ln, " "), "#") {
			continue
		}
		return start, i + 1
	}
	return len(text), len(starts)
}

// runeByteOffsets maps a rune index of text to its byte offset (one more
// entry than runes, for an exclusive end).
func runeByteOffsets(text string) []int {
	out := make([]int, 0, len(text)+1)
	for i := range text {
		out = append(out, i)
	}
	return append(out, len(text))
}
