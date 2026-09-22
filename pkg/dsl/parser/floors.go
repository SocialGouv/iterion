package parser

// ContractSince names the release that first reads `contract` (lot 4bis of
// #1010) — the floor a bundle declaring one must declare in
// `requires.iterion`: a runner below it fails at its first parse of the
// main, the fragment or the child that declares the contract
// (bundle.CheckSyntaxFloor).
//
// While the lot waits, the pin is the next MINOR above the release
// `origin/main` carries; at the cut, release-it's before:git:beforeRelease
// hook runs `cmd/release-floors --apply`, which rewrites the pin to the
// version being released and refuses the release on any other shape
// (`task release:floors` previews the same verdict and writes nothing).
// Two exact arms then hold the result
// (bundle.TestSyntaxFloorsNameReleasesThatExist): before the release, the
// pin must be exactly the next minor above the newest release in the
// changelog — on the pull request's merge ref, main's; after the release,
// the pinned release's notes must carry the word `contract`. A pin the test
// refuses — a release that overtook it — moves by hand to the release that
// first reads the syntax.
const ContractSince = "3.150.0"

// VarMatchingSince names the release that first reads a var's
// `[matching: "<re>"]` constraint (#1350) — the floor a bundle declaring
// one must declare in `requires.iterion`. Below it the runner does not
// know the form and refuses the FILE as a parse error (`expected enum, got
// matching`), not the bundle as an unmet requirement, so the floor is what
// turns an unreadable bundle into a legible refusal.
//
// Same contract as ContractSince: while the lot waits, the pin is the next
// MINOR above the release `origin/main` carries; at the cut
// `cmd/release-floors --apply` rewrites it, and
// bundle.TestSyntaxFloorsNameReleasesThatExist holds the result.
const VarMatchingSince = "3.186.0"
