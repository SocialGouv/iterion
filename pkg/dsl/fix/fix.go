// Package fix applies the mechanical remedies of compile diagnostics to a
// `.bot` text — the edits a diagnostic can carry because its fix is the same
// every time — surgically, on the original bytes, and provably: the fixed
// text parses, and compiles to the same diagnostics minus the ones fixed.
// Nothing goes through the writer, so comments and layout are untouched.
//
// The first remedy is C137: a `{{ref}}` an author quoted in a tool's
// `command:` or `postcondition:` — the runtime shell-quotes a ref already,
// and the two quotings cancel — loses exactly the quotes around it. A tool
// declared inside a `group` serves every `use` of that group: the dotted id
// of an instantiation is resolved through the AST to the group's own
// literal, and one edit at it remedies the diagnostic of every use. Quotes
// that hold more than the reference (`'v={{vars.x}}'`) are not mechanical:
// the diagnostic is left to the author, and said so.
package fix

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/internal/rewrite"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
)

// Edit is the mechanical rewrite a diagnostic carries: the text of one
// place of the file — a string literal — before and after. One edit fixes
// every quoted reference of its literal, so it rides every diagnostic of
// that literal it remedies.
type Edit struct {
	Code   ir.DiagCode `json:"code"`
	Node   string      `json:"node,omitempty"`
	Line   int         `json:"line"`
	Column int         `json:"column"`
	From   string      `json:"from"`
	To     string      `json:"to"`
	// Property is the property the literal belongs to (`command`,
	// `postcondition`); Refs are the references whose quotes the edit
	// removes — together they name the diagnostics the edit remedies, the
	// ones whose message says `<property>: <ref> sits inside quotes`.
	Property string   `json:"property"`
	Refs     []string `json:"refs"`
	// Nodes are the node ids the edit remedies when the literal belongs to a
	// group member: one per `use` of the group, `<prefix>.<member>` — the
	// same literal raises one diagnostic per instantiation. Node then names
	// the `<group>.<member>` the source declares, an id no diagnostic
	// carries.
	Nodes      []string `json:"nodes,omitempty"`
	start, end int
}

// Left is a diagnostic the fixer leaves to the author, and why.
type Left struct {
	Code    ir.DiagCode `json:"code"`
	Node    string      `json:"node,omitempty"`
	Message string      `json:"message"`
	Why     string      `json:"why"`
}

// Result is one file's outcome; nothing has been written.
type Result struct {
	Name     string
	Original []byte
	Fixed    []byte
	// Changed reports whether Fixed differs from Original.
	Changed bool
	Applied []Edit
	// Left are the diagnostics of the ORIGINAL text that no edit fixed:
	// those with no mechanical remedy, and those whose remedy could not be
	// placed.
	Left []Left
}

// ErrRefused marks a file the fixer will not rewrite; the message says why.
var ErrRefused = errors.New("fix refused")

// PlanFor plans the edits for diags, the compile diagnostics of src, in
// file order; a diagnostic with a remedy that cannot be placed is returned
// among left. file is the unit's merged program, the AST the dotted id of a
// group instantiation is resolved through (nil leaves those to the author).
// Diagnostics with no remedy are not left here: PlanFor is the view a
// validator annotates its diagnostics with.
func PlanFor(name string, src []byte, file *ast.File, diags []ir.Diagnostic) (edits []Edit, left []Left) {
	norm := rewrite.Normalize(src)
	toks := parser.NewLexer(name, norm.Text).All()
	runeToByte := rewrite.RuneByteOffsets(norm.Text)
	byToken := map[int]*Edit{} // token index → the edit merging every ref of that literal
	for _, d := range diags {
		if d.Code != ir.DiagQuotedCommandRef {
			continue
		}
		// A member of a group, instantiated by `use`: its literal lives once
		// under `group <g>:` and serves every use. The dotted id resolves
		// through the AST — `<prefix>.<member>` names the group the use
		// instantiates and the tool it declares — and ONE edit at the
		// group's literal remedies the diagnostic of every instantiation.
		dotted := strings.Contains(d.NodeID, ".")
		group, member := "", ""
		if dotted && file != nil {
			if prefix, m, ok := strings.Cut(d.NodeID, "."); ok {
				if g, ok := groupOfUse(file, prefix); ok && hasGroupTool(file, g, m) {
					group, member = g, m
				}
			}
		}
		// The node's `command:` first, its `postcondition:` next: the
		// literal that still hugs the reference the diagnostic names is
		// the one it speaks of.
		var placed, found, raw bool
		for _, prop := range []string{"command", "postcondition"} {
			ti := -1
			switch {
			case group != "":
				ti = groupLiteralToken(toks, group, member, prop)
			case !dotted:
				ti = literalToken(toks, d.NodeID, prop)
			}
			if ti < 0 {
				continue
			}
			found = true
			e := byToken[ti]
			if e == nil {
				t := toks[ti]
				start, end := runeToByte[t.Offset], runeToByte[t.End]
				e = &Edit{Code: d.Code, Line: t.Line, Column: t.Column, From: norm.Text[start:end], To: norm.Text[start:end], Property: prop, start: start, end: end}
				if group != "" {
					e.Node = group + "." + member
					e.Nodes = instanceNodes(file, group, member)
				} else {
					e.Node = d.NodeID
				}
				byToken[ti] = e
			}
			// The diagnostic names the property and the reference — a
			// postcondition's diagnostic is never placed on the command's
			// literal, whichever hugs the reference.
			for _, ref := range ir.QuotedCommandRefs(toks[ti].Value) {
				if !strings.Contains(d.Message, prop+": "+ref+" is raw") && !strings.Contains(d.Message, prop+": "+ref+" sits inside quotes") {
					continue
				}
				// A raw `{{!ref}}` is not escaped by the runtime: the author's
				// quotes are its only containment, and removing them alone
				// leaves the value bare in the shell. Not mechanical — the
				// remedy is the author's (drop the bang, or keep the quotes).
				if ir.QuotedRefIsRaw(ref) {
					raw = true
					break
				}
				if to, ok := unquoted(e.To, ref); ok {
					e.To = to
					e.Refs = append(e.Refs, ref)
					placed = true
					break
				}
				if group != "" && slices.Contains(e.Refs, ref) {
					// The literal serves every instantiation: an earlier
					// use's diagnostic already removed this reference's
					// quotes — this one rides the same edit.
					placed = true
					break
				}
			}
			if placed || raw {
				break
			}
		}
		switch {
		case placed:
		case raw:
			left = append(left, Left{Code: d.Code, Node: d.NodeID, Message: d.Message, Why: "the reference is raw (`!`): the runtime does not escape it, so the quotes are its only containment and removing them would leave the value bare in the shell — drop the bang, or keep the quotes knowing where the value comes from; not mechanical"})
		case !found:
			if dotted {
				left = append(left, Left{Code: d.Code, Node: d.NodeID, Message: d.Message, Why: "no `use` of the source instantiates a group tool under this id, so the literal cannot be located"})
			} else {
				left = append(left, Left{Code: d.Code, Node: d.NodeID, Message: d.Message, Why: fmt.Sprintf("no command: or postcondition: literal of tool %q was found in the source", d.NodeID)})
			}
		default:
			left = append(left, Left{Code: d.Code, Node: d.NodeID, Message: d.Message, Why: "the quotes hold more than the reference, or the reference is written more than once: removing them is not mechanical"})
		}
	}
	for ti, e := range byToken {
		if e.To != e.From {
			edits = append(edits, *e)
		} else {
			delete(byToken, ti)
		}
	}
	sort.Slice(edits, func(i, j int) bool { return edits[i].start < edits[j].start })
	return edits, left
}

// unquoted removes the quotes that hug ref in text — `'{{ref}}'`, `\"{{ref}}\"`
// or `"{{ref}}"`, written exactly once — and reports whether it could.
func unquoted(text, ref string) (string, bool) {
	for _, q := range []string{"'", `\"`, `"`} {
		hugged := q + ref + q
		if strings.Count(text, hugged) == 1 {
			return strings.Replace(text, hugged, ref, 1), true
		}
	}
	return text, false
}

// literalToken is the index of the string token of `<prop>:` inside the
// block of `tool <node>:`, or -1.
func literalToken(toks []parser.Token, node, prop string) int {
	for i := 0; i+2 < len(toks); i++ {
		if toks[i].Type != parser.TokenTool || toks[i+1].Type != parser.TokenIdent || toks[i+1].Value != node || toks[i+2].Type != parser.TokenColon {
			continue
		}
		return blockPropLiteral(toks, i, prop)
	}
	return -1
}

// groupLiteralToken is the index of the string token of `<prop>:` inside
// `tool <member>:` within the block of `group <group>:` — the one literal
// every instantiation of the member serves — or -1.
func groupLiteralToken(toks []parser.Token, group, member, prop string) int {
	for i := 0; i+2 < len(toks); i++ {
		if toks[i].Type != parser.TokenGroup || toks[i+1].Type != parser.TokenIdent || toks[i+1].Value != group {
			continue
		}
		if toks[i+2].Type != parser.TokenColon && toks[i+2].Type != parser.TokenLParen {
			continue
		}
		depth := 0
		for j := i + 3; j < len(toks); j++ {
			switch toks[j].Type {
			case parser.TokenIndent:
				depth++
			case parser.TokenDedent:
				depth--
				if depth <= 0 {
					return -1
				}
			case parser.TokenEOF:
				return -1
			}
			if depth != 1 || j+2 >= len(toks) {
				continue
			}
			if toks[j].Type == parser.TokenTool && toks[j+1].Type == parser.TokenIdent && toks[j+1].Value == member && toks[j+2].Type == parser.TokenColon {
				return blockPropLiteral(toks, j, prop)
			}
		}
		return -1
	}
	return -1
}

// blockPropLiteral is the index of the string token of `<prop>:` inside the
// block the `tool <name>:` header at i opens, or -1.
func blockPropLiteral(toks []parser.Token, i int, prop string) int {
	depth := 0
	for j := i + 3; j < len(toks); j++ {
		switch toks[j].Type {
		case parser.TokenIndent:
			depth++
		case parser.TokenDedent:
			depth--
			if depth <= 0 {
				return -1
			}
		case parser.TokenEOF:
			return -1
		}
		if depth != 1 || j+2 >= len(toks) {
			continue
		}
		isProp := (prop == "command" && toks[j].Type == parser.TokenCommand) || (toks[j].Type == parser.TokenIdent && toks[j].Value == prop)
		if isProp && toks[j+1].Type == parser.TokenColon && toks[j+2].Type == parser.TokenString {
			return j + 2
		}
	}
	return -1
}

// groupOfUse is the group a `use` instantiates under the given prefix.
func groupOfUse(file *ast.File, prefix string) (string, bool) {
	for _, u := range file.Uses {
		if u.Prefix == prefix {
			return u.Group, true
		}
	}
	return "", false
}

// hasGroupTool reports whether the named group declares a tool with the
// given member name — the only member kind whose C137 can carry an edit.
func hasGroupTool(file *ast.File, group, member string) bool {
	for _, g := range file.Groups {
		if g.Name != group {
			continue
		}
		for _, t := range g.Tools {
			if t.Name == member {
				return true
			}
		}
	}
	return false
}

// instanceNodes are the node ids the `use` statements of the group give the
// member — one diagnostic of the shared literal per instantiation, and the
// set one edit remedies.
func instanceNodes(file *ast.File, group, member string) []string {
	var out []string
	for _, u := range file.Uses {
		if u.Group == group {
			out = append(out, u.Prefix+"."+member)
		}
	}
	sort.Strings(out)
	return out
}

// Bytes fixes one file's bytes: name is the file's path. The unit it
// belongs to is read from the bot's main — a fragment under lib/ through
// the main.bot that imports it, a main with its fragments — the program is
// compiled whole, and the diagnostics remedied are this file's own: the
// edits land on its bytes, the rest of the unit is the other files'.
func Bytes(name string, src []byte) (*Result, error) {
	res := &Result{Name: name, Original: src, Fixed: src}
	norm := rewrite.Normalize(src)
	mainPath, rel := unitOf(name)
	before := unit.LoadDirStaged(mainPath, map[string][]byte{rel: src})
	if errs := parseErrors(before.Diagnostics); len(errs) > 0 {
		return nil, fmt.Errorf("%w: %s does not parse — fix it first: %s", ErrRefused, name, strings.Join(errs, "; "))
	}
	if before.Merged == nil {
		return nil, fmt.Errorf("%w: %s carries no program", ErrRefused, name)
	}
	own, ok := fileNamed(before, rel)
	if !ok {
		return nil, fmt.Errorf("%w: %s is not read by its bot's main (%s): nothing of it compiles", ErrRefused, name, mainPath)
	}
	cr := ir.Compile(before.Merged)
	mine := ownDiagnostics(cr.Diagnostics, own)
	edits, left := PlanFor(name, src, before.Merged, mine)
	res.Left = left
	// The diagnostics the edits remove: ONE per reference unquoted — the
	// same reference quoted three times raises the same text three times,
	// and an edit that removes two pairs of quotes removes two of them. A
	// group member's edit removes one per (node, reference): the same
	// literal raises one diagnostic per instantiation, and Nodes names them
	// all.
	want := make([]string, len(mine))
	for i, d := range mine {
		want[i] = diagKey(d)
	}
	for _, e := range edits {
		if len(e.Nodes) == 0 {
			// A top-level literal: one removed pair (one Refs copy) removes
			// one diagnostic.
			for _, ref := range e.Refs {
				for i, d := range mine {
					if want[i] != "" && d.Code == e.Code && d.NodeID == e.Node && strings.Contains(d.Message, e.Property+": "+ref+" sits inside quotes") {
						want[i] = ""
						break
					}
				}
			}
			continue
		}
		// A group member's literal: ONE removed pair serves every
		// instantiation — the same literal raises one diagnostic per node.
		for _, ref := range e.Refs {
			for _, node := range e.Nodes {
				for i, d := range mine {
					if want[i] != "" && d.Code == e.Code && d.NodeID == node && strings.Contains(d.Message, e.Property+": "+ref+" sits inside quotes") {
						want[i] = ""
						break
					}
				}
			}
		}
	}
	for i, d := range mine {
		if want[i] == "" || isLeft(left, d) {
			continue
		}
		res.Left = append(res.Left, Left{Code: d.Code, Node: d.NodeID, Message: d.Message, Why: "no mechanical remedy for " + string(d.Code)})
	}
	if len(edits) == 0 {
		return res, nil
	}
	rewrites := make([]rewrite.Edit, len(edits))
	for i, e := range edits {
		rewrites[i] = rewrite.Edit{Start: e.start, End: e.end, Repl: e.To}
	}
	if _, err := rewrite.Apply(norm.Text, rewrites); err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrRefused, name, err)
	}
	res.Fixed = norm.MapBack(src, rewrites)
	res.Changed = string(res.Fixed) != string(src)
	res.Applied = edits

	// The proof: the fixed text parses, and the unit compiles to the same
	// diagnostics of this file minus the ones fixed — nothing else moved.
	after := unit.LoadDirStaged(mainPath, map[string][]byte{rel: res.Fixed})
	if errs := parseErrors(after.Diagnostics); len(errs) > 0 || after.Merged == nil {
		return nil, fmt.Errorf("%w: the fixed %s does not parse (a defect of the fix, not of the file): %s", ErrRefused, name, strings.Join(errs, "; "))
	}
	var remaining, got []string
	for _, k := range want {
		if k != "" {
			remaining = append(remaining, k)
		}
	}
	for _, d := range ownDiagnostics(ir.Compile(after.Merged).Diagnostics, own) {
		got = append(got, diagKey(d))
	}
	if why := proven(remaining, got); why != "" {
		return nil, fmt.Errorf("%w: the fix would change what %s compiles to: %s", ErrRefused, name, why)
	}
	return res, nil
}

// proven compares the diagnostics expected of the fixed text with the ones
// it compiles to, as multisets; "" when they are the same, else the first
// difference.
func proven(want, got []string) string {
	want, got = append([]string(nil), want...), append([]string(nil), got...)
	sort.Strings(want)
	sort.Strings(got)
	if strings.Join(want, "\n") == strings.Join(got, "\n") {
		return ""
	}
	return firstDifference(want, got)
}

// unitOf is the main a file's unit is read from, and the file's slash path
// under that main's directory: a fragment below a lib/ ancestor whose root
// holds a main.bot belongs to that bot; any other file is its own main.
func unitOf(name string) (mainPath, rel string) {
	abs, err := filepath.Abs(name)
	if err != nil {
		abs = name
	}
	for dir := filepath.Dir(abs); ; dir = filepath.Dir(dir) {
		if filepath.Base(dir) == unit.FragmentDir {
			root := filepath.Dir(dir)
			main := filepath.Join(root, "main.bot")
			if _, err := os.Stat(main); err == nil {
				if r, err := filepath.Rel(root, abs); err == nil {
					return main, filepath.ToSlash(r)
				}
			}
			break
		}
		if filepath.Dir(dir) == dir {
			break
		}
	}
	return abs, filepath.Base(abs)
}

// fileNamed is the name the unit gave the file at rel — the one its
// diagnostics carry — and whether the unit read it at all.
func fileNamed(u *unit.Unit, rel string) (string, bool) {
	for _, f := range u.Files {
		if f.Rel == rel {
			return f.Name, true
		}
	}
	return "", false
}

// ownDiagnostics are the diagnostics attributed to the file named — the
// ones its text can remedy; a global one (no file) and another file's are
// the unit's.
func ownDiagnostics(diags []ir.Diagnostic, name string) []ir.Diagnostic {
	var out []ir.Diagnostic
	for _, d := range diags {
		if d.File == name {
			out = append(out, d)
		}
	}
	return out
}

func diagKey(d ir.Diagnostic) string {
	return string(d.Code) + "|" + d.NodeID + "|" + d.EdgeID + "|" + d.Message
}

func isLeft(left []Left, d ir.Diagnostic) bool {
	for _, l := range left {
		if l.Code == d.Code && l.Node == d.NodeID && l.Message == d.Message {
			return true
		}
	}
	return false
}

func parseErrors(diags []parser.Diagnostic) []string {
	var errs []string
	for _, d := range diags {
		if d.Severity == parser.SeverityError {
			errs = append(errs, d.Error())
		}
	}
	return errs
}

func firstDifference(want, got []string) string {
	seen := map[string]int{}
	for _, w := range want {
		seen[w]++
	}
	for _, g := range got {
		if seen[g] == 0 {
			return "a diagnostic appears that the original had not: " + g
		}
		seen[g]--
	}
	for w, n := range seen {
		if n > 0 {
			return "a diagnostic of the original is gone that no edit fixed: " + w
		}
	}
	return "the diagnostics differ"
}
