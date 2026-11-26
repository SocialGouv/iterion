// Package runtimecatalog holds the pkg/runtime guards whose subject is the
// shipped bot catalogue under bots/: each compiles or runs a real .bot (or a
// bundle's presets/) under the production engine and executor, and goes red
// when that file regresses — a fixer that refuses its delivery on the wrong
// code, a catalog tool node that prints beside its declared answer, a recipe
// that reads the audited checkout's skills or binaries, a ledger exit without
// its guard, a preset without its display name.
//
// They live outside pkg/runtime because they read the whole bots/ tree: kept
// there, every catalogue edit would select the engine's own suite (minutes of
// behaviour tests that import nothing from bots/), and every engine edit would
// re-run the catalogue sweeps. Here a change to bots/ selects these guards
// alone, and pkg/runtime reaches them through its import, so an engine change
// still selects them. They use only pkg/runtime's exported surface.
package runtimecatalog
