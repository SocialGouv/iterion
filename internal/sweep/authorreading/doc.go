// Package authorreading guards that no pkg/server function reads an author
// document: the YAML decode of pkg/dsl/author is sized for the CLI, and the
// guard type-checks the whole module to follow every call chain that reaches
// it.
//
// It lives outside pkg/server because its input is the whole module, not the
// server package: a test that loads every package would make any Go change
// select pkg/server's own behaviour tests. Here, the module-wide load selects
// only this package, and pkg/server is judged by the module-relative path of
// its functions, not by hosting the test.
package authorreading
