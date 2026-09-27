package bots

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// The programme contract has two readers with opposite jobs: the bot that
// WRITES it and the bot that EXECUTES it. They must read the same document.
//
// iterion has no skill-sharing primitive, so the authoring doc's rule applies —
// duplicate, and point each copy at its peers. A pointer in a comment is not a
// mechanism, though: two copies drift the first time somebody edits one. This
// test is the mechanism. Edit either copy and it reddens until both carry it.
func TestContractSkillsAreIdenticalInBothBundles(t *testing.T) {
	shared := []string{"plan-contract.md", "upgrade-archetypes.md"}
	for _, name := range shared {
		t.Run(name, func(t *testing.T) {
			writer, err := os.ReadFile(filepath.Join("assessment", "skills", name))
			if err != nil {
				t.Fatalf("the bot that writes the contract does not ship %s: %v", name, err)
			}
			executor, err := os.ReadFile(filepath.Join("modernize", "skills", name))
			if err != nil {
				t.Fatalf("the bot that executes the contract does not ship %s: %v", name, err)
			}
			if !bytes.Equal(writer, executor) {
				t.Fatalf("assessment/skills/%s and modernize/skills/%s have drifted. The bot that "+
					"writes the contract and the bot that executes it would be working from two "+
					"different documents — copy the edit to both.", name, name)
			}
			if !bytes.Contains(writer, []byte("TODO(skill-duplication)")) {
				t.Errorf("%s carries no peer pointer; a reader who lands on one copy has no way to "+
					"know the other exists", name)
			}
		})
	}
}
