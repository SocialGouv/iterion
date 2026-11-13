// Package clawfold computes and applies the fold of claw-code-go into
// pkg/claw, with a per-file proof that each moved file is the pinned source
// byte for byte except on the import lines the rewrite touches.
//
// The membership is computed, never listed: the vendored set iterion links
// (embeds included), closed over the imports of every Go file of every
// member package — sources and tests — at the pinned commit, plus the
// tests-only packages nested under a member, the non-Go files member
// packages own, and the files outside every package that a member test
// reads through a relative path.
//
// The fold lands in slices under a line cap: asset files first (verbatim,
// inert), then the Go packages in the order the source+test import graph
// imposes, its strongly connected components contracted — an external test
// package that imports back binds its packages into one landing. Each slice
// compiles and tests on what landed before it, and lands unused; the switch
// is the step that moves iterion's own imports and drops the module.
//
// The proofs: a destination file's blob is the rewrite of the pin's blob;
// the inverse rewrite gives the pin's blob back; the two differ on the
// rewritten import lines only; the mode is the pin's. Only import specs are
// rewritten — a literal or a comment naming the module path is left as is
// and reported as a residual.
package clawfold
