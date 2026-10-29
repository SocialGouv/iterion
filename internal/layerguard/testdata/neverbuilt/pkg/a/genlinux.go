//go:build ignore && linux

// The same reserved name inside a conjunction: `tagsOf` hands `ignore` back
// from the && and the guard must drop the file on that name, not on the
// SHAPE of the expression. Recognised by the bare form alone, this file puts
// `ignore` in projectTags, the listing is forced to -tags ignore, and every
// test of the guard fatals over a module the toolchain builds clean.
package main

func main() {}
