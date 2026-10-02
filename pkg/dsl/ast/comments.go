package ast

import (
	"fmt"
	"reflect"
	"strings"
)

// Comment transport — the `##` lines a document carries, on the carrier
// they were written around.
//
// Every carrier holds its comments under the same field name on both sides
// (`Comments`), so the two walks below carry them by reflection rather than
// by a copy written once per declaration kind: a declaration kind added to
// the AST later carries its comments without anyone remembering to list it —
// its mirror needs a `Comments []*jsonComment` field, and the sweep
// (TestCommentsTravelOnEveryCarrier) is what holds that.

// commentsField is the name both sides give the field, and the one name the
// walks look for.
const commentsField = "Comments"

func commentToJSON(c *Comment) *jsonComment {
	if c == nil {
		return nil
	}
	return &jsonComment{Text: c.Text, Anchor: c.Anchor, Place: commentPlaceToStr[c.Place], Blank: c.Blank}
}

func commentFromJSON(jc *jsonComment) (*Comment, error) {
	if jc == nil {
		return nil, nil
	}
	place, ok := strToCommentPlace[jc.Place]
	if !ok {
		return nil, fmt.Errorf("astjson: comment place %q — one of %q, %q, %q", jc.Place, "before", "end", "trailing")
	}
	// The text is canonicalised the way a prompt body is: `## x   ` and
	// `## x` are one comment — the hashes take one space and the line is
	// right-trimmed on the way back (workflowfile.CommentBody) — so a
	// canvas that holds the untrimmed form would make its whole document
	// unsaveable against a text that cannot carry it.
	return &Comment{Text: strings.TrimRight(jc.Text, " \t\r"), Anchor: jc.Anchor, Place: place, Blank: jc.Blank}, nil
}

// stampComments copies every carrier's comments onto its JSON mirror. It
// walks the AST and the mirror side by side, pairing fields by NAME, the
// way stampProvenance does — see its doc for what makes the pairing hold.
func stampComments(av, jv reflect.Value) {
	walkCommentCarriers(av, jv, func(a, j reflect.Value) {
		out := make([]*jsonComment, 0, a.Len())
		for i := 0; i < a.Len(); i++ {
			c, _ := a.Index(i).Interface().(*Comment)
			out = append(out, commentToJSON(c))
		}
		j.Set(reflect.ValueOf(out))
	})
}

// readComments is stampComments' inverse: a mirror's comments become its
// AST carrier's. A place the enum does not name is refused rather than read
// as another one — a document saying "trailng" would silently move a
// comment onto its own line.
func readComments(jv, av reflect.Value) error {
	var err error
	walkCommentCarriers(av, jv, func(a, j reflect.Value) {
		if j.Len() == 0 || err != nil {
			return
		}
		out := make([]*Comment, 0, j.Len())
		for i := 0; i < j.Len(); i++ {
			jc, _ := j.Index(i).Interface().(*jsonComment)
			c, cerr := commentFromJSON(jc)
			if cerr != nil {
				err = cerr
				return
			}
			out = append(out, c)
		}
		a.Set(reflect.ValueOf(out))
	})
	return err
}

// walkCommentCarriers walks an AST value and its JSON mirror side by side
// and calls fn on every pair of `Comments` fields it finds, the AST's first.
// The recursion never enters those fields — their element types differ on
// the two sides, which is exactly what fn is for.
func walkCommentCarriers(av, jv reflect.Value, fn func(a, j reflect.Value)) {
	av, jv = derefValue(av), derefValue(jv)
	if !av.IsValid() || !jv.IsValid() {
		return
	}
	switch av.Kind() {
	case reflect.Struct:
		if jv.Kind() != reflect.Struct {
			return
		}
		at := av.Type()
		for i := 0; i < av.NumField(); i++ {
			f := at.Field(i)
			if !f.IsExported() || f.Name == "Span" {
				continue
			}
			jf := jv.FieldByName(f.Name)
			if !jf.IsValid() {
				continue
			}
			if f.Name == commentsField {
				if av.Field(i).Kind() == reflect.Slice && jf.Kind() == reflect.Slice && jf.CanSet() && av.Field(i).CanSet() {
					fn(av.Field(i), jf)
				}
				continue
			}
			walkCommentCarriers(av.Field(i), jf, fn)
		}
	case reflect.Slice:
		if jv.Kind() != reflect.Slice || av.Len() != jv.Len() {
			return
		}
		for i := 0; i < av.Len(); i++ {
			walkCommentCarriers(av.Index(i), jv.Index(i), fn)
		}
	}
}

// foldGroupComments moves the comments a document put on a group's MEMBERS
// onto the group itself, under the path the parser gives them — `agent a`,
// `agent a.model` — because the group is the carrier the writer places
// through (CommentCarriers). The transport reaches a member's Comments
// field (the walk descends into the group), the writer does not: without
// this, a document that set one would be accepted and the comment written
// nowhere.
func foldGroupComments(f *File) {
	for _, g := range f.Groups {
		if g == nil {
			continue
		}
		fold := func(kind, name string, slot *[]*Comment) {
			if len(*slot) == 0 {
				return
			}
			prefix := kind + " " + name
			for _, c := range *slot {
				if c == nil {
					continue
				}
				d := *c
				if d.Anchor == "" {
					d.Anchor = prefix
				} else {
					d.Anchor = prefix + "." + d.Anchor
				}
				g.Comments = append(g.Comments, &d)
			}
			*slot = nil
		}
		for _, d := range g.Agents {
			fold("agent", d.Name, &d.Comments)
		}
		for _, d := range g.Judges {
			fold("judge", d.Name, &d.Comments)
		}
		for _, d := range g.Routers {
			fold("router", d.Name, &d.Comments)
		}
		for _, d := range g.Humans {
			fold("human", d.Name, &d.Comments)
		}
		for _, d := range g.Tools {
			fold("tool", d.Name, &d.Comments)
		}
		for _, d := range g.Computes {
			fold("compute", d.Name, &d.Comments)
		}
	}
}

// CommentCarrier is one declaration of a file with the comments written
// around it: the keyword that opens it, the name that follows, the list
// itself, and its edges, which carry their own.
type CommentCarrier struct {
	Kind string
	Name string
	// Comments is the declaration's own list, addressable so a reader that
	// attaches (pkg/dsl/parser) can append to it.
	Comments *[]*Comment
	Edges    []*Edge
}

// CommentCarriers enumerates every declaration of f that can carry the
// comments written around it, in the WRITER's order (unparse.writeFile), so
// "the first declaration of the text" means the same thing to a reader of
// this list as to a reader of the file: the one list of the
// class, read by the parser to attach a comment and by the writer to put it
// back. A declaration kind absent here keeps no comment, so the sweep
// (TestEveryDeclarationKindCarriesComments) reads this list against
// File's own fields.
//
// `import` is deliberately not one: its JSON transport is a list of paths
// with nothing to hang a comment on, so a comment written around an import
// stays at the file's head, where the writer has always put it. Nor is an
// INLINE prompt — a prompt written as the text of a property, which has no
// header line in the text at all.
func CommentCarriers(f *File) []CommentCarrier {
	if f == nil {
		return nil
	}
	var out []CommentCarrier
	add := func(kind, name string, slot *[]*Comment, edges []*Edge) {
		out = append(out, CommentCarrier{Kind: kind, Name: name, Comments: slot, Edges: edges})
	}
	if f.Vars != nil {
		add("vars", "", &f.Vars.Comments, nil)
	}
	if f.Presets != nil {
		add("presets", "", &f.Presets.Comments, nil)
	}
	if f.Attachments != nil {
		add("attachments", "", &f.Attachments.Comments, nil)
	}
	if f.Secrets != nil {
		add("secrets", "", &f.Secrets.Comments, nil)
	}
	for _, d := range f.MCPServers {
		add("mcp_server", d.Name, &d.Comments, nil)
	}
	for _, d := range f.Prompts {
		if d.Inline {
			// Written as the text of a property, with no header line of
			// its own: the writer does not emit it (declaredPrompts) and
			// the parser can never attach to it. Listing it would make it
			// "the first declaration" of nearly every file — which is
			// where the head is pooled and where the head is closed.
			continue
		}
		add("prompt", d.Name, &d.Comments, nil)
	}
	for _, d := range f.Schemas {
		add("schema", d.Name, &d.Comments, nil)
	}
	for _, d := range f.Contracts {
		add("contract", d.Name, &d.Comments, nil)
	}
	for _, d := range f.Cursors {
		add("cursor", d.Name, &d.Comments, nil)
	}
	for _, d := range f.Supervisors {
		add("supervisor", d.Name, &d.Comments, nil)
	}
	for _, d := range f.Agents {
		add("agent", d.Name, &d.Comments, nil)
	}
	for _, d := range f.Judges {
		add("judge", d.Name, &d.Comments, nil)
	}
	for _, d := range f.Routers {
		add("router", d.Name, &d.Comments, nil)
	}
	for _, d := range f.Humans {
		add("human", d.Name, &d.Comments, nil)
	}
	for _, d := range f.Tools {
		add("tool", d.Name, &d.Comments, nil)
	}
	for _, d := range f.Computes {
		add("compute", d.Name, &d.Comments, nil)
	}
	for _, d := range f.Emits {
		add("emit", d.Name, &d.Comments, nil)
	}
	for _, d := range f.Waits {
		add("wait", d.Name, &d.Comments, nil)
	}
	for _, d := range f.AwaitAnswers {
		add("await_answers", d.Name, &d.Comments, nil)
	}
	for _, d := range f.Fails {
		add("fail", d.Name, &d.Comments, nil)
	}
	for _, d := range f.Subbots {
		add("subbot", d.Name, &d.Comments, nil)
	}
	for _, d := range f.Groups {
		add("group", d.Name, &d.Comments, d.Edges)
	}
	for _, d := range f.Uses {
		add("use", d.Prefix, &d.Comments, nil)
	}
	for _, d := range f.Workflows {
		add("workflow", d.Name, &d.Comments, d.Edges)
	}
	return out
}

// HasPlacedComments reports whether any declaration of f carries a comment
// — what tells a writer it has anything to put back.
func HasPlacedComments(f *File) bool {
	for _, c := range CommentCarriers(f) {
		if len(*c.Comments) > 0 {
			return true
		}
		for _, e := range c.Edges {
			if e != nil && len(e.Comments) > 0 {
				return true
			}
		}
	}
	return false
}

// MarshalFileWithoutComments is MarshalFile with every `comments` key
// removed, at the file and on each declaration alike: what is left is the
// document the PROGRAM is written in. A caller comparing two texts as
// programs uses it, and compares what the comments say apart
// (AllCommentTexts) — where a comment sits is not program, and a text edit
// that inserts a line above a declaration legitimately moves one.
func MarshalFileWithoutComments(f *File) ([]byte, error) {
	// Emptied on the AST, not deleted from the JSON: a raw value of the
	// document — a contract's `default:` or `params:` — may itself hold a
	// key named "comments", and deleting by name would take it too.
	restore := takeComments(f)
	defer restore()
	return MarshalFile(f)
}

// takeComments empties every comment list of f and returns the function
// that puts them back.
func takeComments(f *File) func() {
	saved := [][]*Comment{f.Comments}
	slots := []*[]*Comment{&f.Comments}
	f.Comments = nil
	for _, c := range CommentCarriers(f) {
		saved = append(saved, *c.Comments)
		slots = append(slots, c.Comments)
		*c.Comments = nil
		for _, e := range c.Edges {
			if e == nil {
				continue
			}
			saved = append(saved, e.Comments)
			slots = append(slots, &e.Comments)
			e.Comments = nil
		}
	}
	return func() {
		for i, slot := range slots {
			*slot = saved[i]
		}
	}
}

// AllCommentTexts is what every comment of a document says — the file's own
// and each declaration's, edges included — in the order the carriers come.
func AllCommentTexts(f *File) []string {
	var out []string
	add := func(cs []*Comment) {
		for _, c := range cs {
			if c != nil {
				out = append(out, c.Text)
			}
		}
	}
	add(f.Comments)
	for _, c := range CommentCarriers(f) {
		add(*c.Comments)
		for _, e := range c.Edges {
			if e != nil {
				add(e.Comments)
			}
		}
	}
	return out
}
