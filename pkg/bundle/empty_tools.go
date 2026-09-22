package bundle

// DeclaredEmptyToolsSince is the first release that reads an agent/judge
// `tools: []` as a DECLARATION — the author saying "this node has no tools" —
// rather than as an absent list.
//
// The floor is not cosmetic: on an engine below it, `tools: []` parses fine
// and means the OPPOSITE. An undeclared list is "no restriction" on every CLI
// backend, so an older runner hands the node Claude Code's whole native
// roster — a bot that asked for no tools runs with all of them. The bundle
// therefore declares the floor and an older engine refuses it
// (BOT_REQUIRES_NEWER_ENGINE) instead of inverting it in silence.
//
// The pin is held by TestSyntaxFloorsNameReleasesThatExist and realigned by
// the release cut itself (internal/floorsalign), like every other floor:
// while the release is uncut it must be exactly the next minor above the
// changelog's newest release, and once cut, the release's notes must carry
// the syntax's word.
const DeclaredEmptyToolsSince = "3.187.0"
