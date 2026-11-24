//go:build ignore

// The idiomatic generator: `//go:generate go run gen.go` compiles this file
// alone, and the toolchain's reserved `ignore` keeps its `package main` out
// of package a. Pass the name as a -tags value and it comes back in — two
// package names in one directory, and `go list` fails for the whole module.
package main

func main() {}
