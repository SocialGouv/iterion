// Package authorcorpus holds the corpus benches of the author document (the
// YAML twin of a .bot): every .bot of the repository converted with
// `fmt --to yaml` and held to E056 under edits of its own program, and every
// .bot of bots/ and examples/ drawn through its document and through itself.
//
// The benches read every .bot the tree holds, with its bundle prompts,
// fragments and manifest, so a change anywhere in that corpus can turn them
// red. They live here rather than in pkg/cli, whose own tests read only what
// they import: a test selector that picks a package by what its tests can
// reach selects pkg/cli on a change to the CLI, and this package on a change
// to the CLI or to the corpus.
package authorcorpus
