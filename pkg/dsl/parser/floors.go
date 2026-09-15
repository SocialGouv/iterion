package parser

import "fmt"

// ContractSince names the release that first reads `contract` (lot 4bis of
// #1010) — the floor a bundle declaring one must declare in
// `requires.iterion`: a runner below it fails at its first parse of the
// main, the fragment or the child that declares the contract
// (bundle.CheckSyntaxFloor). Pinned to the next release when the syntax
// ships; should the number shift, this entry moves with it
// (TestSyntaxFloorsNameReleasesThatExist holds it once the release is cut).
const ContractSince = "3.149.0"

// SyntaxFloors is the registry of the releases a syntax first ships in, by
// the name of the constant that pins each — the one list the release test,
// the floor predicate's callers and a reference walk, so a floor added
// later is never left out of one of them.
func SyntaxFloors() map[string]string {
	floors := map[string]string{
		"parser.ImportSince":   ImportSince,
		"parser.ContractSince": ContractSince,
	}
	for profile, since := range ProfileSince {
		floors[fmt.Sprintf("parser.ProfileSince[%d]", profile)] = since
	}
	return floors
}
