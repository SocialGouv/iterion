// Package catalogcorpus holds the guards over the shipped workflow corpus as
// a whole: every bot under bots/ and examples/, the dispatcher's default
// assignee, the operator scripts under scripts/adhoc/, and the pages and
// manifests that describe them. Each guard parses, compiles or reads every
// member of that corpus — the parse/compile floor, the typing and consistency
// floors, the untrusted-input boundary class, the input-mapping and prose
// sweeps, the catalogue freshness check — so a change anywhere in it reaches
// all of them.
//
// They live outside bots so that a test selector can run them when the corpus
// changes without running the whole bots package, whose own behaviour tests
// then select only on what they import. The tests run from <root>/bots (see
// TestMain), so every path and frozen key reads as it did when they lived
// there.
package catalogcorpus
