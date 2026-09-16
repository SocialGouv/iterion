// Package fix applies the mechanical remedies of compile diagnostics to a
// `.bot` text — the edits a diagnostic can carry because its fix is the same
// every time — surgically, on the original bytes, and provably: the fixed
// text parses, and compiles to the same diagnostics minus the ones fixed.
// Nothing goes through the writer, so comments and layout are untouched.
//
// The first remedy is C137: a `{{ref}}` an author quoted in a tool's
// `command:` or `postcondition:` — the runtime shell-quotes a ref already,
// and the two quotings cancel — loses exactly the quotes around it. Quotes
// that hold more than the reference (`'v={{vars.x}}'`) are not mechanical:
// the diagnostic is left to the author, and said so.
package fix

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

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
	Property   string   `json:"property"`
	Refs       []string `json:"refs"`
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
// among left. Diagnostics with no remedy are not left here: PlanFor is the
// view a validator annotates its diagnostics with.
func PlanFor(name string, src []byte, diags []ir.Diagnostic) (edits []Edit, left []Left) {
	norm := rewrite.Normalize(src)
	toks := parser.NewLexer(name, norm.Text).All()
	runeToByte := rewrite.RuneByteOffsets(norm.Text)
	byToken := map[int]*Edit{} // token index → the edit merging every ref of that literal
	for _, d := range diags {
		if d.Code != ir.DiagQuotedCommandRef {
			continue
		}
		// A member of a group, instantiated by `use`: its literal lives once
		// under `group <g>:` and serves every use — not rewritten yet, said
		// as such rather than searched for under a name the source has not.
		if strings.Contains(d.NodeID, ".") {
			left = append(left, Left{Code: d.Code, Node: d.NodeID, Message: d.Message, Why: "a member of a group instantiated by `use`: the literal lives once under `group <name>:` and serves every use — not rewritten mechanically yet, remove the quotes there by hand"})
			continue
		}
		// The node's `command:` first, its `postcondition:` next: the
		// literal that still hugs the reference the diagnostic names is
		// the one it speaks of.
		var placed, found, raw bool
		for _, prop := range []string{"command", "postcondition"} {
			ti := literalToken(toks, d.NodeID, prop)
			if ti < 0 {
				continue
			}
			found = true
			e := byToken[ti]
			if e == nil {
				t := toks[ti]
				start, end := runeToByte[t.Offset], runeToByte[t.End]
				e = &Edit{Code: d.Code, Node: d.NodeID, Line: t.Line, Column: t.Column, From: norm.Text[start:end], To: norm.Text[start:end], Property: prop, start: start, end: end}
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
			left = append(left, Left{Code: d.Code, Node: d.NodeID, Message: d.Message, Why: fmt.Sprintf("no command: or postcondition: literal of tool %q was found in the source", d.NodeID)})
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
	return -1
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
	edits, left := PlanFor(name, src, mine)
	res.Left = left
	// The diagnostics the edits remove: ONE per reference unquoted — the
	// same reference quoted three times raises the same text three times,
	// and an edit that removes two pairs of quotes removes two of them.
	want := make([]string, len(mine))
	for i, d := range mine {
		want[i] = diagKey(d)
	}
	for _, e := range edits {
		for _, ref := range e.Refs {
			for i, d := range mine {
				if want[i] != "" && d.Code == e.Code && d.NodeID == e.Node && strings.Contains(d.Message, e.Property+": "+ref+" sits inside quotes") {
					want[i] = ""
					break
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
