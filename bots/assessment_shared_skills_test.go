package bots

import (
	"testing"

	"github.com/SocialGouv/iterion/internal/skillshare"
)

// The programme contract has two readers with opposite jobs: the bot that
// WRITES it and the bot that EXECUTES it. They must read the same document.
//
// The mechanism that keeps them reading it is shared now:
// internal/skillshare declares every skill several bundles carry, writes each
// mirror from its family's source under `task gen`, and reddens on a copy that
// drifted. This test is what the bots package owes on top of it — that THESE
// two families are declared as mirrors of one source, so nobody closes the
// gate on the contract by re-declaring either copy a variant. A variant is a
// legitimate verdict for a skill a bundle tailors; for the contract it would
// mean the writer and the executor work from two different documents, which is
// the defect, not a tailoring.
func TestContractSkillsAreOneSourceInBothBundles(t *testing.T) {
	const writer, executor = "assessment", "modernize"
	for _, name := range []string{"plan-contract.md", "upgrade-archetypes.md"} {
		t.Run(name, func(t *testing.T) {
			var family skillshare.Family
			for _, f := range skillshare.Families() {
				if f.Name == name {
					family = f
				}
			}
			if family.Name == "" {
				t.Fatalf("%s is not declared in internal/skillshare/families.go, so nothing keeps the bot that "+
					"writes the programme contract and the bot that executes it reading the same document", name)
			}
			members := map[string]skillshare.Kind{}
			for _, b := range family.Members() {
				members[b] = family.KindOf(b)
			}
			for _, bundle := range []string{writer, executor} {
				kind, ok := members[bundle]
				if !ok {
					t.Fatalf("family %s does not declare %s as a member", name, bundle)
				}
				if kind == skillshare.VariantKind {
					t.Fatalf("family %s declares %s's copy a VARIANT. The bot that writes the contract and the "+
						"bot that executes it would be working from two different documents — one of them must be "+
						"the source and the other its mirror.", name, bundle)
				}
			}
			if members[writer] == members[executor] {
				t.Fatalf("family %s declares both %s and %s as %s — one is the source, the other its mirror",
					name, writer, executor, members[writer])
			}
		})
	}
}
