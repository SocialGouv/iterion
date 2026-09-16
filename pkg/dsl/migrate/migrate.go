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
	"github.com/SocialGouv/iterion/pkg/dsl/internal/rewrite"
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

	norm := rewrite.Normalize(src)
	before := parser.Parse(name, norm.Text)
	if errs := parseErrors(before.Diagnostics); len(errs) > 0 {
		return nil, fmt.Errorf("%w: %s does not parse in its own profile — fix it first: %s", ErrRefused, name, strings.Join(errs, "; "))
	}
	if before.File.EffectiveProfile() >= to {
		// Already there: nothing to rewrite, and nothing to prove.
		res.Migrated = src
		return res, nil
	}
	toks := parser.NewLexer(name, norm.Text).All()
	if line := projectRootLine(before.File, toks); line > 0 {
		return nil, fmt.Errorf("%w: %s:%d uses `project_root:`, which profile 2 removed and has no mechanical replacement (`visibility:` is a different axis, C171): keep the file in profile 1, or redesign the memory scope first", ErrRefused, name, line)
	}

	edits, changes := planEdits(norm.Text, toks, before.File, to)
	res.Changes = changes
	if _, err := rewrite.Apply(norm.Text, edits); err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrRefused, name, err)
	}
	res.Migrated = norm.MapBack(src, edits)
	res.Changed = !bytes.Equal(res.Migrated, src)

	// The oracle: the two texts read as the same document.
	after := parser.Parse(name, rewrite.Normalize(res.Migrated).Text)
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
// planEdits lists the rewrites: the header, the directive comments, the
// quoted literals that hold a backslash. Every edit covers only what it
// changes — the header's value, a comment's own span, a literal — so no two
// can overlap: a comment shares its line with the header or with a literal
// often enough.
func planEdits(norm string, toks []parser.Token, f *ast.File, to int) ([]rewrite.Edit, []Change) {
	var edits []rewrite.Edit
	var changes []Change
	lines := rewrite.LineStarts(norm)
	runeToByte := rewrite.RuneByteOffsets(norm)

	// The header: replace the value of an explicit `dsl: 1` — the number
	// alone, so a comment after it stays — or insert one before the first
	// significant line.
	pre := parser.ReadPreamble(norm)
	eol := "\n"
	header := fmt.Sprintf("dsl: %d", to)
	if pre.HeaderLine > 0 {
		start, end := rewrite.LineSpan(norm, lines, pre.HeaderLine)
		if valueEnd := headerValueEnd(toks, pre.HeaderLine, runeToByte); valueEnd > start {
			edits = append(edits, rewrite.Edit{Start: start, End: valueEnd, Repl: header})
			changes = append(changes, Change{Kind: "header", Line: pre.HeaderLine, From: norm[start:valueEnd], To: header})
		} else {
			edits = append(edits, rewrite.Edit{Start: start, End: end, Repl: header + eol})
			changes = append(changes, Change{Kind: "header", Line: pre.HeaderLine, From: strings.TrimRight(norm[start:end], "\n"), To: header})
		}
	} else {
		at, line := firstSignificantLine(norm, lines)
		repl := header + eol + eol
		if at == len(norm) {
			// Nothing significant follows: the header closes the file, on
			// a line of its own even when the last comment has no newline.
			repl = header + eol
			if at > 0 && norm[at-1] != '\n' {
				repl = eol + repl
			}
		}
		edits = append(edits, rewrite.Edit{Start: at, End: at, Repl: repl})
		changes = append(changes, Change{Kind: "header", Line: line, To: header})
	}

	// The directive: the top-level comments the parser refuses under
	// profile 2 (E042) — the same set, so the migrated file parses — and
	// nothing else: a comment inside a block is never a directive and is
	// never touched. A comment alone on its line takes the line with it; one
	// after code leaves the code and its newline in place.
	for _, c := range f.Comments {
		if !parser.IsStrictEscapeDirective(c.Text) {
			continue
		}
		lineStart, lineEnd := rewrite.LineSpan(norm, lines, c.Span.Start.Line)
		cstart := lineStart + rewrite.ColumnByte(norm[lineStart:lineEnd], c.Span.Start.Column)
		if strings.TrimSpace(norm[lineStart:cstart]) == "" {
			edits = append(edits, rewrite.Edit{Start: lineStart, End: lineEnd, Repl: ""})
			changes = append(changes, Change{Kind: "directive", Line: c.Span.Start.Line, From: strings.TrimRight(norm[lineStart:lineEnd], "\n")})
			continue
		}
		for cstart > lineStart && (norm[cstart-1] == ' ' || norm[cstart-1] == '\t') {
			cstart--
		}
		cend := lineEnd
		if cend > cstart && norm[cend-1] == '\n' {
			cend--
		}
		edits = append(edits, rewrite.Edit{Start: cstart, End: cend, Repl: ""})
		changes = append(changes, Change{Kind: "directive", Line: c.Span.Start.Line, From: strings.TrimSpace(norm[cstart:cend])})
	}

	for _, t := range toks {
		switch t.Type {
		case parser.TokenString:
			start, end := runeToByte[t.Offset], runeToByte[t.End]
			if start >= end || norm[start] != '"' {
				continue // a raw string or a block scalar: no escape to re-spell
			}
			if !strings.Contains(norm[start:end], `\`) {
				continue // the same text in both profiles
			}
			repl := unparse.QuoteStrict(t.Value)
			edits = append(edits, rewrite.Edit{Start: start, End: end, Repl: repl})
			changes = append(changes, Change{Kind: "literal", Line: t.Line, From: norm[start:end], To: repl})
		}
	}
	sort.SliceStable(edits, func(i, j int) bool { return edits[i].Start < edits[j].Start })
	return edits, changes
}

// headerValueEnd is the byte offset just past the number of the `dsl: N`
// header on line, 0 when the tokens do not show `dsl`, `:`, an integer there.
func headerValueEnd(toks []parser.Token, line int, runeToByte []int) int {
	for i, t := range toks {
		if t.Type != parser.TokenDSL || t.Line != line {
			continue
		}
		if i+2 < len(toks) && toks[i+1].Type == parser.TokenColon && toks[i+2].Type == parser.TokenInt && toks[i+2].End < len(runeToByte) {
			return runeToByte[toks[i+2].End]
		}
		return 0
	}
	return 0
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
// when none: on an agent's or a judge's memory block, in a group too. The
// block carries no span per property, so the property's own line is the
// first `project_root` token at or after the block's.
func projectRootLine(f *ast.File, toks []parser.Token) int {
	line := func(m *ast.MemoryBlock) int {
		if m == nil || m.ProjectRoot == nil {
			return 0
		}
		for _, t := range toks {
			if t.Type == parser.TokenProjectRoot && t.Line >= m.Span.Start.Line {
				return t.Line
			}
		}
		return m.Span.Start.Line
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
