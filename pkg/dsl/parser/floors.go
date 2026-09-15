package parser

// ContractSince names the release that first reads `contract` (lot 4bis of
// #1010) — the floor a bundle declaring one must declare in
// `requires.iterion`: a runner below it fails at its first parse of the
// main, the fragment or the child that declares the contract
// (bundle.CheckSyntaxFloor).
//
// The pin is the next MINOR above the release `origin/main` carries when
// the lot is enqueued (a `feat:` merge bumps the minor); a release cut
// before the merge overtakes it, and the pin then names a build that
// cannot read the syntax. Two guards hold it: TestSyntaxFloorsNameReleasesThatExist
// refuses a pin at or below the newest release in the changelog — on the
// pull request's merge ref, main's changelog — and, once the release that
// carries the merge is cut, `git tag --contains <the commit that added
// contract to the parser> | sort -V | head -1` must equal this constant;
// realign it in a patch before anything authors a manifest against it.
const ContractSince = "3.150.0"
