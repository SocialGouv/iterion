// Package ast defines the Abstract Syntax Tree for the iterion DSL V1.
package ast

// Pos represents a position in a source file.
type Pos struct {
	File   string // source file name
	Line   int    // 1-based line number
	Column int    // 1-based column number
}

// Span represents a range in a source file.
type Span struct {
	Start Pos
	End   Pos
}

// PropertySpan is the span of one property line of a declaration: from the
// property's name to the end of its value — the end of its body for a block
// property, of its last line for a value written over several — End being
// the position just past the last rune.
type PropertySpan struct {
	Name string
	Span Span
}

// PropertySpans is the table of property spans a declaration carries, one
// entry per property name the parser accepted (a refused name, E012, is no
// property of the declaration), in the order it first read them: what a
// compile diagnostic about a property's VALUE points at, where the
// declaration's header was the only position before (#1094). A slice rather
// than a map: the author twin remaps every Pos reachable through exported
// fields, slices included, and a map's values are not addressable. A
// position, so the JSON transport leaves it out as it leaves Span out.
type PropertySpans []PropertySpan

// Lookup returns the span recorded under name.
func (ps PropertySpans) Lookup(name string) (Span, bool) {
	for _, p := range ps {
		if p.Name == name {
			return p.Span, true
		}
	}
	return Span{}, false
}

// Set records the span of the property line named name. A property written
// twice keeps the span of the line written last.
func (ps *PropertySpans) Set(name string, sp Span) {
	for i := range *ps {
		if (*ps)[i].Name == name {
			(*ps)[i].Span = sp
			return
		}
	}
	*ps = append(*ps, PropertySpan{Name: name, Span: sp})
}
