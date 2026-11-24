package clawfold

import (
	"bytes"
	"fmt"
	"go/format"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
	"strings"
)

// Mapping rewrites Go import paths under one module path prefix to another.
// It touches import specs only — a comment, a string literal or an
// identifier that spells the same path is left byte for byte — so its
// inverse is the same mapping read the other way, and applying the inverse
// to a rewritten file gives back the original bytes exactly as long as the
// original held no import already under the destination prefix
// ([Mapping.ImportsUnderDestination] is that precondition).
type Mapping struct {
	From string
	To   string
}

// Inverse is the mapping that undoes m.
func (m Mapping) Inverse() Mapping { return Mapping{From: m.To, To: m.From} }

// mapPath returns path rewritten under m and whether it is under m.From:
// equal to it, or below it at a path-segment boundary. A sibling module
// that merely shares the prefix (`claw-code-gopher`) is not under it.
func (m Mapping) mapPath(path string) (string, bool) {
	if path == m.From {
		return m.To, true
	}
	if rest, ok := strings.CutPrefix(path, m.From+"/"); ok {
		return m.To + "/" + rest, true
	}
	return "", false
}

// importSpec is one import declaration's path literal, located in the file.
type importSpec struct {
	path  string // the unquoted import path
	start int    // byte offset of the literal's opening quote
	end   int    // byte offset just past its closing quote
	line  int    // 1-based line of the literal
}

func importSpecs(filename string, src []byte) ([]importSpec, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, src, parser.ImportsOnly)
	if err != nil {
		return nil, fmt.Errorf("parse imports of %s: %w", filename, err)
	}
	specs := make([]importSpec, 0, len(file.Imports))
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			return nil, fmt.Errorf("%s: unquote import %s: %w", fset.Position(imp.Path.Pos()), imp.Path.Value, err)
		}
		// Physical positions: a //line directive must not move the line a
		// proof compares against the bytes.
		start := fset.PositionFor(imp.Path.Pos(), false)
		end := fset.PositionFor(imp.Path.End(), false)
		specs = append(specs, importSpec{path: path, start: start.Offset, end: end.Offset, line: start.Line})
	}
	return specs, nil
}

// ImportsUnderDestination returns the lines of src holding an import that is
// already under m.To. A file with such an import cannot be rewritten under m
// with an exact inverse: the inverse would carry that import back to m.From
// too.
func (m Mapping) ImportsUnderDestination(filename string, src []byte) ([]int, error) {
	specs, err := importSpecs(filename, src)
	if err != nil {
		return nil, err
	}
	var lines []int
	for _, spec := range specs {
		if _, under := m.Inverse().mapPath(spec.path); under {
			lines = append(lines, spec.line)
		}
	}
	return lines, nil
}

// Rewrite returns src with every import spec under m.From rewritten under
// m.To, and the 1-based lines it changed, ascending. Nothing outside the
// import path literals moves, so the line count is unchanged. A file with no
// such import comes back identical with no lines: Rewrite is idempotent.
//
// A literal that spells its path with an escape sequence is refused: its
// bytes are not the path, so a byte-exact inverse could not be promised.
func (m Mapping) Rewrite(filename string, src []byte) ([]byte, []int, error) {
	out, lines, _, err := m.rewrite(filename, src)
	return out, lines, err
}

// rewrite is Rewrite that also counts the import specs it rewrote.
func (m Mapping) rewrite(filename string, src []byte) ([]byte, []int, int, error) {
	specs, err := importSpecs(filename, src)
	if err != nil {
		return nil, nil, 0, err
	}
	type edit struct {
		start, end int
		literal    []byte
	}
	var edits []edit
	var lines []int
	for _, spec := range specs {
		mapped, under := m.mapPath(spec.path)
		if !under {
			continue
		}
		literal := src[spec.start:spec.end]
		quote := literal[0]
		if !bytes.Equal(literal[1:len(literal)-1], []byte(spec.path)) {
			return nil, nil, 0, fmt.Errorf("%s:%d: import literal %s spells its path with an escape; refusing a rewrite whose inverse would not be byte-exact", filename, spec.line, literal)
		}
		replacement := make([]byte, 0, len(mapped)+2)
		replacement = append(replacement, quote)
		replacement = append(replacement, mapped...)
		replacement = append(replacement, quote)
		edits = append(edits, edit{start: spec.start, end: spec.end, literal: replacement})
		lines = append(lines, spec.line)
	}
	if len(edits) == 0 {
		return bytes.Clone(src), nil, 0, nil
	}
	sort.Slice(edits, func(i, j int) bool { return edits[i].start < edits[j].start })
	var out bytes.Buffer
	out.Grow(len(src) + 32*len(edits))
	last := 0
	for _, e := range edits {
		out.Write(src[last:e.start])
		out.Write(e.literal)
		last = e.end
	}
	out.Write(src[last:])
	sort.Ints(lines)
	return out.Bytes(), dedupe(lines), len(edits), nil
}

func dedupe(sorted []int) []int {
	out := sorted[:0]
	for i, v := range sorted {
		if i == 0 || v != sorted[i-1] {
			out = append(out, v)
		}
	}
	return out
}

// GofmtStable reports whether gofmt leaves src unchanged. A file that does
// not parse is reported through the error.
func GofmtStable(src []byte) (bool, error) {
	formatted, err := format.Source(src)
	if err != nil {
		return false, err
	}
	return bytes.Equal(formatted, src), nil
}

// ChangedLines returns the 1-based lines on which a and b differ, for two
// texts with the same number of lines; ok is false when the counts differ.
func ChangedLines(a, b []byte) (lines []int, ok bool) {
	la := bytes.Split(a, []byte("\n"))
	lb := bytes.Split(b, []byte("\n"))
	if len(la) != len(lb) {
		return nil, false
	}
	for i := range la {
		if !bytes.Equal(la[i], lb[i]) {
			lines = append(lines, i+1)
		}
	}
	return lines, true
}
