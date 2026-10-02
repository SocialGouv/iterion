package unparse

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/workflowfile"
)

// directiveLine is the 1-based line of the first strict-escape directive
// written as a comment of text, 0 when there is none.
func directiveLine(text string) int {
	line := 0
	for l := range strings.Lines(text) {
		line++
		if c, ok := workflowfile.CommentText(l); ok && parser.IsStrictEscapeDirective(c) {
			return line
		}
	}
	return 0
}

// Verify reports whether text — the output of Unparse(f) — reads back as the
// program f: it parses without an error and compiles to the same workflow
// with the same diagnostics (ir.SameProgram). A caller that writes the text
// somewhere the program is later read from (the studio's save path) refuses
// on an error rather than write a file that means something else than the
// document it came from.
//
// Prompt bodies are compared in the lexer's canonical form
// (parser.CanonicalPromptBody): a canvas body with a paragraph break or a
// trailing newline has no other written form, and every reader of the file
// gets the canonical one — so that is the program the document IS, and the
// guard refuses only what would actually change. A body the syntax cannot
// carry at all (parser.CheckPromptBody: a first line indented deeper than a
// later one) IS such a change — its nearest written form de-indents it —
// and is refused by name before anything is compared.
func Verify(f *ast.File, text string) error {
	for _, p := range f.Prompts {
		if p.Inline {
			continue // written as a quoted string: every body has that form
		}
		if err := parser.CheckPromptBody(p.Body); err != nil {
			return fmt.Errorf("prompt %q cannot be written as .bot source: %v", p.Name, err)
		}
	}
	// A fallback route is written under its name; one with no name — the
	// canvas's route before it is named — has no written form, and the
	// writer leaves it out, which the comparison below would report as a
	// node that differs. Said by name instead.
	for _, a := range f.Agents {
		if err := checkFallbackNames(a.Fallbacks); err != nil {
			return fmt.Errorf("agent %q cannot be written as .bot source: %v", a.Name, err)
		}
	}
	for _, j := range f.Judges {
		if err := checkFallbackNames(j.Fallbacks); err != nil {
			return fmt.Errorf("judge %q cannot be written as .bot source: %v", j.Name, err)
		}
	}
	// A group's members go through the same writers.
	for _, g := range f.Groups {
		for _, a := range g.Agents {
			if err := checkFallbackNames(a.Fallbacks); err != nil {
				return fmt.Errorf("group %q, agent %q cannot be written as .bot source: %v", g.Name, a.Name, err)
			}
		}
		for _, j := range g.Judges {
			if err := checkFallbackNames(j.Fallbacks); err != nil {
				return fmt.Errorf("group %q, judge %q cannot be written as .bot source: %v", g.Name, j.Name, err)
			}
		}
	}
	// A contract's names are written bare and its JSON values in the text's
	// own syntax: a name that is not an identifier, or a number the lexer
	// never reads (a sign, an exponent), has no written form — refused here
	// by name, before the re-parse below refuses the text at a place the
	// author never wrote.
	for _, c := range f.Contracts {
		if err := checkContract(c); err != nil {
			return fmt.Errorf("contract %q cannot be written as .bot source: %v", contractName(c), err)
		}
	}
	f = canonicalPrompts(f)
	if err := reread(f, text); err != nil {
		return explainWindow(f, text, err)
	}
	return nil
}

// explainWindow appends to a failure of the re-read the one cause the text
// itself shows: the strict-escape directive written where profile 1 does not
// read it — among the first lines of the file and before its first
// non-comment line, a frozen rule (parser.Preamble.StrictEscape). The
// frontmatter is comments and goes above the directive, where the catalog
// reader wants it, so a long one pushes the directive out of that window:
// it is in the text and not in effect, and every escape of the text reads
// as its two characters. A text that reads the same all the same — no value
// needed the strict form — is not refused for it: the note explains a
// failure, it never makes one.
func explainWindow(f *ast.File, text string, err error) error {
	if f.EffectiveProfile() > ast.DefaultProfile {
		return err
	}
	line := directiveLine(text)
	if line == 0 || parser.ReadPreamble(text).StrictEscape {
		return err
	}
	return fmt.Errorf("%w; the strict-escape directive is written at line %d, where profile 1 does not read it (the directive is read among the first lines of the file only, and %d lines of comments — the frontmatter — stand above it): shorten the frontmatter, or write the file in profile 2, which reads standard escapes without a directive", err, line, line-1)
}

// reread parses text again and reports the first way it fails to be the
// program f: it does not parse, reads as another profile, compiles to
// another workflow, carries other contracts or comments, or — when there is
// no compiled program to compare — mirrors as another document.
func reread(f *ast.File, text string) error {
	// The round-trip is parsed under the document's own source file, so an
	// {{include}} resolves — or is refused — on both sides alike. A document
	// from the JSON transport has no source file: naming one here made the
	// re-parse resolve its includes while the document itself could not,
	// and every bot with an include was refused at save.
	pr := parser.Parse(sourceFile(f), text)
	var errs []string
	for _, d := range pr.Diagnostics {
		if d.Severity == parser.SeverityError {
			errs = append(errs, d.Error())
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("the serialised source does not parse: %s", strings.Join(errs, "; "))
	}
	// The profile is not program — the same AST compiles the same in
	// either — so SameProgram cannot see it lost: a document saved in the
	// wrong profile would read its strings and its prompts otherwise.
	if got, want := pr.File.EffectiveProfile(), f.EffectiveProfile(); got != want {
		return fmt.Errorf("the serialised source reads as dsl profile %d, the document is profile %d", got, want)
	}
	ca, cb := ir.Compile(f), ir.Compile(pr.File)
	if why := ir.SameProgram(ca, cb); why != "" {
		return fmt.Errorf("the serialised source is not the same program: %s", why)
	}
	// The compiled program carries the bound contract (SameProgram sees one
	// lost, changed or forged by the text), but it normalises what the
	// author wrote — `version: 1` and none, an explicit `required: true`
	// and none read the same — and a document that does not compile has
	// no program at all: the span-free mirror holds the rest. A contract is
	// canonical by construction (names bare, JSON values compact in key
	// order), so the comparison is exact.
	if why := sameContracts(f, pr.File); why != "" {
		return fmt.Errorf("the serialised source is not the same document: %s", why)
	}
	// A comment is not program, so nothing above can see one lost: that is
	// exactly how a save dropped every comment written inside or between
	// declarations for as long as it did (#1282). Said here by name: every
	// comment of the document is in the text, on the same declaration, in
	// the same order. The ADDRESS inside a declaration is not compared —
	// the writer normalises the indentation of a comment written deeper
	// than the line under it, which moves no comment and changes no
	// text — so what is asserted is what the author would notice.
	if why := sameComments(f, pr.File); why != "" {
		return fmt.Errorf("the serialised source does not carry the same comments: %s", why)
	}
	if ca.Workflow == nil || cb.Workflow == nil {
		// No compiled program to compare — the shape of every half-authored
		// canvas document (no `workflow` yet). The AST mirror is span-free
		// and carries every declaration, so it is the oracle here; without
		// it the guard passed anything that did not compile. Comments are
		// not program and are left out of it: they travel around the
		// declarations rather than in them, the writer may add the
		// strict-escape directive, and sameComments above is what holds
		// them — by the line each names, not by its rank in a list.
		// The header is not program either: `dsl: 1` and no header read
		// alike (EffectiveProfile, compared above) and the writer omits
		// profile 1's header, so the mirror carries the profile as the text
		// reads it, not as the header spelled it — or every profile-1 file
		// written with its header was refused here the moment it did not
		// compile.
		fa, fb := *f, *pr.File
		fa.Profile, fb.Profile = fa.EffectiveProfile(), fb.EffectiveProfile()
		fa.Prompts, fb.Prompts = InlinePromptsLast(fa.Prompts), InlinePromptsLast(fb.Prompts)
		a, err := ast.MarshalFileWithoutComments(&fa)
		if err != nil {
			return fmt.Errorf("cannot compare the document: %w", err)
		}
		b, err := ast.MarshalFileWithoutComments(&fb)
		if err != nil {
			return fmt.Errorf("cannot compare the serialised source: %w", err)
		}
		if !bytes.Equal(a, b) {
			return fmt.Errorf("the serialised source is not the same document: %s", FirstJSONDifference(a, b))
		}
	}
	return nil
}

// sourceFile is the file the document's prompts were read from, "" when it
// came through the JSON transport (no spans) — which is every production
// caller: both save handlers build the document with ast.UnmarshalFile.
// Includes are the only thing a file name decides, and they live in
// prompts, so the first prompt's file is the document's; a document whose
// prompts came from several files (a bundle's prompts/*.md merged beside
// a main.bot's) never reaches Verify.
func sourceFile(f *ast.File) string {
	for _, p := range f.Prompts {
		if p.Span.Start.File != "" {
			return p.Span.Start.File
		}
	}
	return ""
}

// checkContract reports the first thing in c the text cannot carry: a nil
// entry, a name that is not an identifier, a JSON value with a number the
// lexer never reads (parser.WritableJSONValue).
func checkContract(c *ast.ContractDecl) error {
	if c == nil {
		return fmt.Errorf("a nil contract has no written form")
	}
	if !isBareIdent(c.Name) {
		return fmt.Errorf("its name is not an identifier")
	}
	if c.Version != nil {
		if err := writableInt("version", int64(*c.Version)); err != nil {
			return err
		}
	}
	if err := checkContractPorts("input", c.Inputs); err != nil {
		return err
	}
	if err := checkContractPorts("output", c.Outputs); err != nil {
		return err
	}
	for i, k := range c.Criteria {
		if k == nil {
			return fmt.Errorf("criterion %d is nil", i+1)
		}
		if !isBareIdent(k.Name) {
			return fmt.Errorf("criterion name %q is not an identifier", k.Name)
		}
		if err := writableName("criterion "+k.Name+" kind", k.Kind, true); err != nil {
			return err
		}
		if err := writableName("criterion "+k.Name+" port", k.Port, true); err != nil {
			return err
		}
		if err := parser.WritableJSONValue(k.Params); err != nil {
			return fmt.Errorf("criterion %q params: %v", k.Name, err)
		}
	}
	for i, e := range c.Effects {
		if e == nil {
			return fmt.Errorf("effect %d is nil", i+1)
		}
		if !isBareIdent(e.Name) {
			return fmt.Errorf("effect name %q is not an identifier", e.Name)
		}
	}
	return nil
}

func checkContractPorts(side string, ports []*ast.PortDecl) error {
	for i, p := range ports {
		if p == nil {
			return fmt.Errorf("%s %d is nil", side, i+1)
		}
		if !isBareIdent(p.Name) {
			return fmt.Errorf("%s name %q is not an identifier", side, p.Name)
		}
		base := p.Type
		for strings.HasSuffix(base, "[]") {
			base = strings.TrimSuffix(base, "[]")
		}
		if base == "" {
			return fmt.Errorf("%s %q has no type; a port is written `name: type`", side, p.Name)
		}
		if err := writableName(side+" "+p.Name+" type", base, false); err != nil {
			return err
		}
		if err := writableName(side+" "+p.Name+" from", p.From, true); err != nil {
			return err
		}
		if p.MinItems != nil {
			if err := writableInt(side+" "+p.Name+" min_items", int64(*p.MinItems)); err != nil {
				return err
			}
		}
		if p.MaxItems != nil {
			if err := writableInt(side+" "+p.Name+" max_items", int64(*p.MaxItems)); err != nil {
				return err
			}
		}
		if err := parser.WritableJSONValue(p.Default); err != nil {
			return fmt.Errorf("%s %q default: %v", side, p.Name, err)
		}
		if p.FileSpec != nil {
			if err := writableName(side+" "+p.Name+" file schema", p.FileSpec.Schema, false); err != nil {
				return err
			}
			if err := writableInt(side+" "+p.Name+" file min_bytes", p.FileSpec.MinBytes); err != nil {
				return err
			}
		}
	}
	return nil
}

// writableName holds a value the writer renders bare to the shape the
// grammar reads back — an identifier, dotted when the property is a
// reference; an empty value is absent, not wrong.
func writableName(what, v string, dotted bool) error {
	if v == "" {
		return nil
	}
	ok := isBareIdent(v)
	if dotted {
		ok = dottedIdent(v)
	}
	if !ok {
		return fmt.Errorf("%s %q is not an identifier", what, v)
	}
	return nil
}

// writableInt holds an integer the writer renders bare to what the lexer
// reads: the text has no signed number.
func writableInt(what string, v int64) error {
	if v < 0 {
		return fmt.Errorf("%s: the number %d cannot be written in a .bot, which has no signed number", what, v)
	}
	return nil
}

func contractName(c *ast.ContractDecl) string {
	if c == nil {
		return ""
	}
	return c.Name
}

// sameContracts names the first difference between the contracts of the
// document and those the text reads as, and between the contract each
// workflow names; "" when they agree.
func sameContracts(a, b *ast.File) string {
	// Without the comments: where one sits inside a contract is the
	// business of sameComments, which compares by what they SAY. Compared
	// here, a comment the writer re-anchored — legitimately, the property
	// it named being gone — refused the whole save.
	x, err := ast.MarshalFileWithoutComments(&ast.File{Contracts: a.Contracts})
	if err != nil {
		return "cannot compare the document's contracts: " + err.Error()
	}
	y, err := ast.MarshalFileWithoutComments(&ast.File{Contracts: b.Contracts})
	if err != nil {
		return "cannot compare the serialised source's contracts: " + err.Error()
	}
	if !bytes.Equal(x, y) {
		return FirstJSONDifference(x, y)
	}
	for i, w := range a.Workflows {
		if w == nil || i >= len(b.Workflows) || b.Workflows[i] == nil {
			continue
		}
		if w.Contract != b.Workflows[i].Contract {
			return fmt.Sprintf("workflow %q names contract %q, the serialised source %q", w.Name, w.Contract, b.Workflows[i].Contract)
		}
	}
	return ""
}

// checkFallbackNames reports the first fallback route with no name.
func checkFallbackNames(fbs []*ast.FallbackDecl) error {
	for i, fb := range fbs {
		if fb == nil || strings.TrimSpace(fb.Name) == "" {
			return fmt.Errorf("fallback route %d has no name; every route is written under its name", i+1)
		}
	}
	return nil
}

// InlinePromptsLast is ps with the declared prompts first, in their order,
// and the inline ones after them, sorted by name. An inline prompt is named
// after its body and listed where its node is read — the writer puts
// `system:` before `user:` and a group's nodes after the others, a document
// may write them the other way — so its rank says nothing that a comparison
// of two readings of one program should read; the node that uses it names
// it. The slice is new, ps is left as it came.
func InlinePromptsLast(ps []*ast.PromptDecl) []*ast.PromptDecl {
	out := make([]*ast.PromptDecl, 0, len(ps))
	var inline []*ast.PromptDecl
	for _, p := range ps {
		if p.Inline {
			inline = append(inline, p)
		} else {
			out = append(out, p)
		}
	}
	sort.SliceStable(inline, func(i, j int) bool { return inline[i].Name < inline[j].Name })
	return append(out, inline...)
}

// canonicalPrompts is a shallow copy of f whose prompt bodies are in the
// lexer's canonical form — the form the writer emits and the re-parse
// yields. The document itself is left as it came.
func canonicalPrompts(f *ast.File) *ast.File {
	cp := *f
	cp.Prompts = make([]*ast.PromptDecl, len(f.Prompts))
	for i, p := range f.Prompts {
		if p.Inline {
			// Written as a quoted string, which carries the body verbatim:
			// compared as it is.
			cp.Prompts[i] = p
			continue
		}
		q := *p
		q.Body = parser.CanonicalPromptBodyIn(f.EffectiveProfile(), p.Body)
		cp.Prompts[i] = &q
	}
	return &cp
}

// FirstJSONDifference names the first key path at which two JSON documents
// diverge, so a refused save says which declaration did not survive.
func FirstJSONDifference(a, b []byte) string {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return "the documents differ"
	}
	if d := diffAny("document", x, y); d != "" {
		return d
	}
	// The bytes differ but the decoded values do not (one side escaped a
	// rune the other wrote raw): still a refusal, never an empty reason.
	return "the documents differ in their encoding"
}

// diffAny names the first difference between two decoded JSON values: a
// mapping's keys walked in sorted order, so the difference a refusal names
// is the same on every run — never the one a map's iteration happened on.
func diffAny(path string, x, y any) string {
	switch xv := x.(type) {
	case map[string]any:
		yv, ok := y.(map[string]any)
		if !ok {
			return path + " differs in kind"
		}
		for _, k := range slices.Sorted(maps.Keys(xv)) {
			ye, ok := yv[k]
			if !ok {
				return path + "." + k + " is missing after the round-trip"
			}
			if d := diffAny(path+"."+k, xv[k], ye); d != "" {
				return d
			}
		}
		for _, k := range slices.Sorted(maps.Keys(yv)) {
			if _, ok := xv[k]; !ok {
				return path + "." + k + " appeared after the round-trip"
			}
		}
		return ""
	case []any:
		yv, ok := y.([]any)
		if !ok || len(xv) != len(yv) {
			return fmt.Sprintf("%s has a different length", path)
		}
		for i := range xv {
			if d := diffAny(fmt.Sprintf("%s[%d]", path, i), xv[i], yv[i]); d != "" {
				return d
			}
		}
		return ""
	default:
		if !reflect.DeepEqual(x, y) {
			return fmt.Sprintf("%s: %v became %v", path, x, y)
		}
		return ""
	}
}

// sameComments reports the first difference between the comments of two
// documents — the file's own head and tail, then each declaration's and
// each of its edges' — or "" when they carry the same ones in the same
// order. The strict-escape directive is left out: the writer places it
// itself, so a profile-1 file written strict gains one the document had
// not (unparse.go).
func sameComments(a, b *ast.File) string {
	if why := sameCommentTexts("the file's tail", fileComments(a.Comments, ast.CommentAtEnd), fileComments(b.Comments, ast.CommentAtEnd)); why != "" {
		return why
	}
	ca, cb := ast.CommentCarriers(a), ast.CommentCarriers(b)
	if len(ca) != len(cb) {
		return fmt.Sprintf("%d declarations carry comments in the document, %d in the text", len(ca), len(cb))
	}
	// The file's head and the comments of the FIRST declaration are one
	// pool. They are written one after the other with nothing between
	// them but a blank line, and when the head is empty not even that —
	// so which of the two a text carries them on is not a fact, and the
	// writer's own canonical order decides which declaration comes
	// first. What is a fact, and what is compared, is that the comments
	// above the first declaration are the same ones.
	headA, headB := fileHeadComments(a.Comments), fileHeadComments(b.Comments)
	if len(ca) > 0 {
		headA = append(headA, *ca[0].Comments...)
		headB = append(headB, *cb[0].Comments...)
	}
	if why := sameCommentTexts("the file's head", headA, headB); why != "" {
		return why
	}
	for i := range ca {
		if i == 0 {
			continue // pooled with the head above
		}
		where := ca[i].Kind + " " + ca[i].Name
		if ca[i].Kind != cb[i].Kind || ca[i].Name != cb[i].Name {
			return fmt.Sprintf("%s reads back as %s %s", where, cb[i].Kind, cb[i].Name)
		}
		if why := sameCommentTexts(where, *ca[i].Comments, *cb[i].Comments); why != "" {
			return why
		}
		if len(ca[i].Edges) != len(cb[i].Edges) {
			return fmt.Sprintf("%s has %d edges in the document, %d in the text", where, len(ca[i].Edges), len(cb[i].Edges))
		}
		for j := range ca[i].Edges {
			if ca[i].Edges[j] == nil || cb[i].Edges[j] == nil {
				continue
			}
			if why := sameCommentTexts(fmt.Sprintf("%s, edge %d", where, j+1), ca[i].Edges[j].Comments, cb[i].Edges[j].Comments); why != "" {
				return why
			}
		}
	}
	return ""
}

// sameCommentTexts compares the comments one carrier holds, as a SET of
// texts. Neither their order in the list nor the line each names is
// compared, and neither is a fact the comparison could rest on: the writer
// renders a declaration's properties in its own order, so two comments
// leading two properties swap whenever the source wrote them the other way
// round, and a comment whose property the document no longer has is written
// at the end of the block on purpose. What IS the fact, and what #1282 was
// about, is the one asserted here — this declaration still carries these
// comments, none lost, none gained, none landed on another declaration.
func sameCommentTexts(where string, a, b []*ast.Comment) string {
	ta, tb := commentTexts(a), commentTexts(b)
	if len(ta) != len(tb) {
		return fmt.Sprintf("%s carries %d comments, the text %d", where, len(ta), len(tb))
	}
	sort.Strings(ta)
	sort.Strings(tb)
	for i := range ta {
		if ta[i] != tb[i] {
			return fmt.Sprintf("%s: the comment %q is not in the text", where, ta[i])
		}
	}
	return ""
}

// commentTexts is what a carrier's comments say. The strict-escape
// directive is not one of them — the writer places it itself, so a
// profile-1 file written strict gains one the document had not.
func commentTexts(cs []*ast.Comment) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		if c == nil || parser.IsStrictEscapeDirective(c.Text) {
			continue
		}
		out = append(out, c.Text)
	}
	return out
}
