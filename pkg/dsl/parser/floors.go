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
