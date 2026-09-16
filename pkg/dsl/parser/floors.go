package parser

// ContractSince names the release that first reads `contract` (lot 4bis of
// #1010) — the floor a bundle declaring one must declare in
// `requires.iterion`: a runner below it fails at its first parse of the
// main, the fragment or the child that declares the contract
// (bundle.CheckSyntaxFloor).
//
// The pin is the next MINOR above the release `origin/main` carries when
// the lot is enqueued (a `feat:` merge bumps the minor). Two exact arms hold
// it (bundle.TestSyntaxFloorsNameReleasesThatExist): before the release,
// the pin must be exactly the next minor above the newest release in the
// changelog — on the pull request's merge ref, main's — so a release that
// overtook it (the pin would name a build that does not read the syntax)
// and an over-pin (the pin would refuse builds that do) are both red before
// the merge; after the release, the pinned release's notes must carry the
// word `contract`, so a number another feature took is red as well. A pin
// the test refuses moves to the release that first reads the syntax, in a
// patch, before anything authors a manifest against it.
const ContractSince = "3.150.0"
