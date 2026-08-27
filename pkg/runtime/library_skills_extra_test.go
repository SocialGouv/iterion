package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// wfWithExtraSkills is a workflow whose only skill refs are workflow-level.
// Named apart from the sibling file's helper, which takes a different shape.
func wfWithExtraSkills(names ...string) *ir.Workflow {
	return &ir.Workflow{Name: "t", Skills: names}
}

// writeLibrarySkill plants a skill in a project-scoped library
// (<storeDir>/skills/<name>/SKILL.md — what LocalStoreForProject layers over
// the machine-global one).
func writeLibrarySkill(t *testing.T, storeDir, name, description string) {
	t.Helper()
	dir := filepath.Join(storeDir, "skills", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	body := "---\nname: " + name + "\ndescription: " + description + "\n---\n\n# " + name + "\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write skill: %v", err)
	}
}

// The whole contract in one word: ADDITIVE. An operator adding a house
// standard must not be able to remove what the bot's author declared —
// that would be a run silently missing knowledge its .bot promises.
func TestExtraSkillsAddToTheWorkflowsOwn(t *testing.T) {
	workDir := t.TempDir()
	storeDir := t.TempDir()
	writeLibrarySkill(t, storeDir, "bot-declared", "the bot's own")
	writeLibrarySkill(t, storeDir, "house-standard", "the operator's own")

	hints, _, err := mirrorLibrarySkills(workDir, storeDir, wfWithExtraSkills("bot-declared"),
		[]string{"house-standard"}, nil, nil)
	if err != nil {
		t.Fatalf("mirror: %v", err)
	}
	for _, want := range []string{"bot-declared", "house-standard"} {
		if _, ok := hints[want]; !ok {
			t.Errorf("skill %q missing from the hints — the roster is what tells the model it exists", want)
		}
		p := filepath.Join(workDir, ".claude", "skills", want, "SKILL.md")
		if _, err := os.Stat(p); err != nil {
			t.Errorf("skill %q not mirrored to %s: %v", want, p, err)
		}
	}
}

// The hint is the roster line. A skill mirrored to disk but absent from the
// prompt is one the agent never learns it can ask for — for a library skill
// the "## Skills" section is the only thing that names it.
func TestExtraSkillCarriesItsDescriptionIntoTheRoster(t *testing.T) {
	workDir := t.TempDir()
	storeDir := t.TempDir()
	writeLibrarySkill(t, storeDir, "house-standard", "how this shop authors bots")

	hints, _, err := mirrorLibrarySkills(workDir, storeDir, wfWithExtraSkills(),
		[]string{"house-standard"}, nil, nil)
	if err != nil {
		t.Fatalf("mirror: %v", err)
	}
	if got := hints["house-standard"]; got != "how this shop authors bots" {
		t.Errorf("description lost: got %q", got)
	}
}

func TestUnionSkillRefsDedupes(t *testing.T) {
	got := unionSkillRefs([]string{"a", "b"}, []string{"b", "c", ""})
	want := []string{"a", "b", "c"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestUnionSkillRefsWithNoExtrasIsIdentity(t *testing.T) {
	wf := []string{"a", "b"}
	if got := unionSkillRefs(wf, nil); strings.Join(got, ",") != "a,b" {
		t.Errorf("got %v", got)
	}
}

// An operator-typed name that resolves to nothing is an ERROR, not a warning.
// The workflow's own refs stay soft (a bundle ships its skills, the library is
// a fallback) — but nobody typed those, and dropping a typed one with a log
// line reproduces exactly the silent failure this seam exists to close.
func TestResolveExtraSkillsRefusesAnUnknownName(t *testing.T) {
	storeDir := t.TempDir()
	writeLibrarySkill(t, storeDir, "house-standard", "present")

	err := ResolveExtraSkills(storeDir, []string{"house-standard", "typo-standard"})
	if err == nil {
		t.Fatal("expected an error for a name that is not in the library")
	}
	if !strings.Contains(err.Error(), "typo-standard") {
		t.Errorf("the error must name the missing skill: %v", err)
	}
	// The --preset precedent: say what IS available, so the operator can fix
	// it from the message instead of going to look.
	if !strings.Contains(err.Error(), "house-standard") {
		t.Errorf("the error must list what is available: %v", err)
	}
}

func TestResolveExtraSkillsAcceptsResolvableNames(t *testing.T) {
	storeDir := t.TempDir()
	writeLibrarySkill(t, storeDir, "house-standard", "present")
	if err := ResolveExtraSkills(storeDir, []string{"house-standard"}); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestResolveExtraSkillsIsANoOpWithNoNames(t *testing.T) {
	if err := ResolveExtraSkills(t.TempDir(), nil); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestResolveExtraSkillsRefusesAPathEscapingName(t *testing.T) {
	err := ResolveExtraSkills(t.TempDir(), []string{"../../tmp/x"})
	if err == nil {
		t.Fatal("expected an error for a path-shaped skill name")
	}
	if !strings.Contains(err.Error(), "invalid skill name") && !strings.Contains(err.Error(), "../../tmp/x") {
		t.Errorf("the error must name the bad skill: %v", err)
	}
}
