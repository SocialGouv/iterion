package parser

import "github.com/SocialGouv/iterion/pkg/dsl/ast"

// attachComments gives every `##` line of a parsed source to the
// declaration it was written around, with the address Scan read for it.
//
// What the file itself keeps (File.Comments) is what is written around no
// declaration: its head, above the first line of code, and its tail. A
// comment the AST has no carrier for — one around an `import`, one around a
// declaration a diagnostic kept out of the AST — is kept at the head, which
// is where the writer has always put it: a comment is never dropped, and
// the one place it can land without an address is the one it would have
// landed in before this existed.
func attachComments(file string, f *ast.File, tokens []Token) {
	if f == nil {
		return
	}
	_, placed := Scan(tokens)
	idx := newDeclIndex(f)
	var head []*ast.Comment
	for _, pc := range placed {
		c := &ast.Comment{
			Text:   pc.Text,
			Anchor: pc.Path,
			Place:  pc.Place,
			Blank:  pc.Blank,
			Span:   ast.Span{Start: ast.Pos{File: file, Line: pc.Line, Column: pc.Col}, End: ast.Pos{File: file, Line: pc.Line, Column: pc.Col}},
		}
		if pc.Decl.IsFile() {
			// The file's own: its head, and its tail below the last
			// declaration. Both keep the place they were written at.
			c.Anchor = ""
			head = append(head, c)
			continue
		}
		slot := idx.slotFor(pc, c)
		if slot == nil {
			c.Anchor, c.Place = "", ast.CommentBefore
			head = append(head, c)
			continue
		}
		*slot = append(*slot, c)
	}
	f.Comments = head
}

// declIndex finds the carrier of an address: the declaration a line belongs
// to, by the keyword that opens it and the name that follows.
type declIndex struct {
	// slots is the comment list of each declaration, and edges its edge
	// list when it has one.
	slots map[declKey]*[]*ast.Comment
	edges map[declKey][]*ast.Edge
}

type declKey struct{ kind, name string }

func newDeclIndex(f *ast.File) *declIndex {
	idx := &declIndex{
		slots: map[declKey]*[]*ast.Comment{},
		edges: map[declKey][]*ast.Edge{},
	}
	for _, c := range ast.CommentCarriers(f) {
		k := declKey{c.Kind, c.Name}
		if _, dup := idx.slots[k]; dup {
			// Two declarations of a kind under one name: the program is
			// refused later (E010) and the address is ambiguous now.
			// Neither gets the comment — not on the declaration and not
			// on its edges; it stays at the head.
			idx.slots[k], idx.edges[k] = nil, nil
			continue
		}
		idx.slots[k] = c.Comments
		idx.edges[k] = c.Edges
	}
	return idx
}

// slotFor is the comment list a placed comment belongs in, or nil when the
// AST holds no carrier for its address. An edge carries the comments
// written around its own line; c's anchor is cleared then, the edge BEING
// the address.
func (idx *declIndex) slotFor(pc PlacedComment, c *ast.Comment) *[]*ast.Comment {
	k := declKey{pc.Decl.Kind, pc.Decl.Name}
	if pc.Edge > 0 && pc.Path == EdgeKey(pc.Edge) && pc.Place != ast.CommentAtEnd {
		edges := idx.edges[k]
		if edges == nil || pc.Edge > len(edges) || edges[pc.Edge-1] == nil {
			return nil
		}
		c.Anchor = ""
		return &edges[pc.Edge-1].Comments
	}
	slot, ok := idx.slots[k]
	if !ok || slot == nil {
		return nil
	}
	return slot
}
