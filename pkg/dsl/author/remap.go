package author

import (
	"reflect"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// mapper turns a position in the spelled text into the position of the
// YAML node the line came from.
type mapper struct {
	name  string
	lines []line
}

// at returns the YAML position of (line, col) in the spelled text: the
// value's node when the column falls in the value, the line's own node
// otherwise, the nearest line above with one for a blank or synthetic
// line, and 1:1 when nothing is known.
func (m *mapper) at(l, c int) (int, int) {
	if l < 1 || l > len(m.lines) {
		return 1, 1
	}
	ln := m.lines[l-1]
	if ln.valCol > 0 && c >= ln.valCol && ln.val.line > 0 {
		return ln.val.line, ln.val.col
	}
	if ln.at.line > 0 {
		return ln.at.line, ln.at.col
	}
	for i := l - 2; i >= 0; i-- {
		if p := m.lines[i].at; p.line > 0 {
			return p.line, p.col
		}
	}
	return 1, 1
}

func (m *mapper) diagnostics(diags []parser.Diagnostic) []parser.Diagnostic {
	out := make([]parser.Diagnostic, 0, len(diags))
	for _, d := range diags {
		d.File = m.name
		d.Line, d.Column = m.at(d.Line, d.Column)
		out = append(out, d)
	}
	return out
}

var posType = reflect.TypeOf(ast.Pos{})

// spans rewrites every position of the AST onto the YAML: a walk over
// every exported field, slice element and pointer, so a position added to
// the AST later is mapped without anyone listing it.
func (m *mapper) spans(f *ast.File) {
	m.walk(reflect.ValueOf(f))
}

func (m *mapper) walk(v reflect.Value) {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			m.walk(v.Elem())
		}
	case reflect.Struct:
		if v.Type() == posType {
			if v.CanAddr() {
				p := v.Addr().Interface().(*ast.Pos)
				if p.Line > 0 {
					p.File = m.name
					p.Line, p.Column = m.at(p.Line, p.Column)
				}
			}
			return
		}
		t := v.Type()
		for i := 0; i < v.NumField(); i++ {
			if t.Field(i).IsExported() {
				m.walk(v.Field(i))
			}
		}
	case reflect.Slice, reflect.Array:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return // bytes (a json.RawMessage) hold no position
		}
		for i := 0; i < v.Len(); i++ {
			m.walk(v.Index(i))
		}
	}
}
