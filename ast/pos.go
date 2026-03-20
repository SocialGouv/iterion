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
