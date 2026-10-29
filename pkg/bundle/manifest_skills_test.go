package bundle

import (
	"strings"
	"testing"
)

// skillSharingManifest is a manifest declaring both sides of the shared
// skill contract, with the body handed over so a case can malform one field
// and nothing else.
func skillSharingManifest(exports, dependencies string) string {
	body := "name: app-dev\nversion: 1.0.0\n"
	if exports != "" {
		body += "exports:\n  skills:\n" + exports
	}
	if dependencies != "" {
		body += "dependencies:\n  skills:\n" + dependencies
	}
	return body
}

// The key decodes, and every rule refuses PER INDEX: an author is told
// which entry is wrong and why, the bar validateWorkflowSharing sets for
// the other artefact kind. A rule that passed silently would admit a
// manifest whose declaration cannot be resolved later.
//
// Reddens on the mutation that drops validateSkillSharing from the decoder,
// and on any rule whose message loses its index.
func TestExportsAndDependenciesSkillsRefusePerIndex(t *testing.T) {
	for name, tc := range map[string]struct {
		exports, dependencies string
		want                  string
	}{
		"export without an id":       {"    - path: skills/a.md\n", "", "exports.skills[0].id is required"},
		"export id with a separator": {"    - id: a/b\n      path: skills/a.md\n", "", "exports.skills[0].id \"a/b\" must not contain path separators"},
		"second export duplicates":   {"    - id: a\n      path: skills/a.md\n    - id: a\n      path: skills/b.md\n", "", "exports.skills[1].id \"a\" is duplicated"},
		"export without a path":      {"    - id: a\n", "", "exports.skills[0].path is required"},
		"export path escaping":       {"    - id: a\n      path: ../other/a.md\n", "", "must stay inside the bundle"},
		"export path not a document": {"    - id: a\n      path: skills/a.bot\n", "", "must name a .md skill document"},
		"dependency without a name":  {"", "    - from: app-dev\n", "dependencies.skills[0].name is required"},
		"dependency name separator":  {"", "    - name: a/b\n      from: app-dev\n", "dependencies.skills[0].name \"a/b\" must not contain path separators"},
		"second dependency repeats":  {"", "    - name: a\n      from: x\n    - name: a\n      from: y\n", "dependencies.skills[1].name \"a\" is duplicated"},
		"dependency without a from":  {"", "    - name: a\n", "dependencies.skills[0].from is required"},
		"dependency from separator":  {"", "    - name: a\n      from: ../x\n", "dependencies.skills[0].from \"../x\" must not contain path separators"},
		"dependency id separator":    {"", "    - name: a\n      from: x\n      id: p/q\n", "dependencies.skills[0].id \"p/q\" must not contain path separators"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := DecodeManifest([]byte(skillSharingManifest(tc.exports, tc.dependencies)), "manifest.yaml")
			if err == nil {
				t.Fatalf("the manifest decoded: nothing refused %s", name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("the refusal does not name the entry and the rule:\ngot:  %v\nwant: %s", err, tc.want)
			}
		})
	}

	// The well-formed shape ADR-144 spells out: it decodes, it normalizes
	// (an id defaults to the name it is referenced under, a path is cleaned
	// to a bundle-relative slash path), and the keys it declares are read.
	body := skillSharingManifest(
		"    - id: verify-build\n      path: ./skills/verify-build.md\n",
		"    - name: verify-build\n      from: app-dev\n    - name: diff-scope\n      from: app-dev\n      id: diff-scope-v2\n")
	m, err := DecodeManifest([]byte(body), "manifest.yaml")
	if err != nil {
		t.Fatalf("the declared shape does not decode: %v", err)
	}
	if len(m.Exports.Skills) != 1 || m.Exports.Skills[0].ID != "verify-build" || m.Exports.Skills[0].Path != "skills/verify-build.md" {
		t.Fatalf("the export did not decode or normalize: %+v", m.Exports.Skills)
	}
	if len(m.Dependencies.Skills) != 2 {
		t.Fatalf("the dependencies did not decode: %+v", m.Dependencies.Skills)
	}
	if got := m.Dependencies.Skills[0]; got.ID != "verify-build" || got.From != "app-dev" {
		t.Fatalf("an id left empty does not default to the name it is referenced under: %+v", got)
	}
	if got := m.Dependencies.Skills[1]; got.ID != "diff-scope-v2" {
		t.Fatalf("a declared id was replaced: %+v", got)
	}
	if !m.DeclaredKeys.Declares(keyExportsSkills) || !m.DeclaredKeys.Declares(keyDependenciesSkills) {
		t.Fatalf("the nested keys are not read as declared: %s", declaredKeyList(m.DeclaredKeys))
	}
	// `exports.workflows` is the sibling key, and it asks for no floor: a
	// manifest that declares it alone owes nothing.
	if !m.DeclaredKeys.Declares(keyExportsSkills) || m.DeclaredKeys["exports.workflows"] {
		t.Fatalf("the key reader confused the two export kinds: %s", declaredKeyList(m.DeclaredKeys))
	}
}

// Each shared-skill key owes the release floor its own pin names, through
// the one floors table: `validate` C252, the push admission and the
// scaffold all read it, so an adopter is told "this bundle needs a build
// that reads the key" instead of meeting a strict-decode failure on a pod.
//
// floortest:exempt SkillSharingSince — the floor declared here is the one
// each case is AT, read from its own pin; no case states a release below a
// floor, which is floortest.Below's job.
func TestTheSharedSkillKeysOweTheirReleaseFloor(t *testing.T) {
	for name, tc := range map[string]struct {
		exports, dependencies, floor string
		wantOK                       bool
	}{
		"an export with no floor":              {"    - id: a\n      path: skills/a.md\n", "", "", false},
		"an export with a floor below the pin": {"    - id: a\n      path: skills/a.md\n", "", ">= 1.0.0", false},
		"an export with a floor above the pin": {"    - id: a\n      path: skills/a.md\n", "", ">= 9.999.0", true},
		"an export at the pin":                 {"    - id: a\n      path: skills/a.md\n", "", ">= " + SkillSharingSince, true},
		"a dependency with no floor":           {"", "    - name: a\n      from: x\n", "", false},
		"a dependency at the pin":              {"", "    - name: a\n      from: x\n", ">= " + SkillSharingSince, true},
		"neither key declared":                 {"", "", "", true},
	} {
		t.Run(name, func(t *testing.T) {
			body := skillSharingManifest(tc.exports, tc.dependencies)
			if tc.floor != "" {
				body += "requires:\n  iterion: \"" + tc.floor + "\"\n"
			}
			m, err := DecodeManifest([]byte(body), "manifest.yaml")
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			pf := CheckSyntaxFloor(m, SyntaxRequirements{})
			if pf.OK != tc.wantOK {
				t.Fatalf("floor OK = %v, want %v: %+v\nmanifest:\n%s", pf.OK, tc.wantOK, pf, body)
			}
			if tc.wantOK {
				return
			}
			if pf.Need != SkillSharingSince || !strings.Contains(pf.Uses, "skills") {
				t.Fatalf("the verdict does not name the keys and the release that reads them: %+v", pf)
			}
			if !strings.Contains(pf.Consequence, "refuses the whole document") {
				t.Fatalf("the verdict does not say what a build below the floor does: %q", pf.Consequence)
			}
		})
	}
}
