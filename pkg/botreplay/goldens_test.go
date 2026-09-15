package botreplay

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// TestGoldens is the replay gate: for every wired scenario it loads the
// committed fixture, recompiles the bot, and re-validates the recorded
// LLM output against the CURRENT schema plus the scenario's invariants
// (required-field presence, no hallucinated assignees). It hits no LLM
// and needs no credentials, so it runs in `task test` and the dedicated
// `task test:goldens` gate inside `task check`.
func TestGoldens(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("repoRoot: %v", err)
	}
	valid, err := ValidBots(root)
	if err != nil {
		t.Fatalf("ValidBots: %v", err)
	}
	if len(valid) == 0 {
		t.Fatal("ValidBots returned an empty set — examples/ not discovered")
	}

	wfCache := map[string]*ir.Workflow{}
	getWF := func(bot string) (*ir.Workflow, error) {
		if wf, ok := wfCache[bot]; ok {
			return wf, nil
		}
		wf, err := CompileBot(bot)
		if err != nil {
			return nil, err
		}
		wfCache[bot] = wf
		return wf, nil
	}

	for _, s := range Scenarios() {
		t.Run(s.Bot+"/"+s.Name, func(t *testing.T) {
			path := s.FixturePath()
			f, err := LoadFixture(path)
			if err != nil {
				t.Fatalf("load fixture %s: %v (run `task test:goldens:record` to (re)generate)", path, err)
			}
			if f.Node != s.Node {
				t.Fatalf("fixture node %q != scenario node %q", f.Node, s.Node)
			}

			wf, err := getWF(s.Bot)
			if err != nil {
				t.Fatalf("compile bot %q: %v", s.Bot, err)
			}

			if err := VerifySchema(f, wf); err != nil {
				t.Errorf("schema validity: %v", err)
			}
			if len(s.RequiredNonEmpty) > 0 {
				if err := VerifyRequiredNonEmpty(f, s.RequiredNonEmpty); err != nil {
					t.Errorf("required fields: %v", err)
				}
			}
			if err := VerifyRequiredSubstrings(f, s.RequiredSubstrings); err != nil {
				t.Errorf("factual anchors: %v", err)
			}
			if s.CheckAssignees {
				if err := VerifyNoHallucinatedAssignees(f, valid); err != nil {
					t.Errorf("assignees: %v", err)
				}
			}
		})
	}
}
