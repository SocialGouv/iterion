//go:build ignore && linux

// The same reserved name inside a conjunction: no build of the toolchain
// compiles this file, since the conjunction is false whatever `linux` says,
// and theReservedNameKeepsItOut answers that from the expression — the shape,
// never the bare name. The name still reaches tagsDemandedBy through
// `tagsOf`, which is why it is the one name that demand drops: forced as a
// -tags value, the listing is given -tags ignore and every test of the guard
// fatals over a module the toolchain builds clean.
package main

func main() {}
