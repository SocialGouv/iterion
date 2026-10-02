package botscaffold

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The authoring skills carry a "Start from a template" section whose table
// names the gallery's shapes. It is hand-written, so it is held here to
// Templates() in both directions, in BOTH skills — the root SKILL.md an
// agent installs and the whats-next quickref mirrored into runs — and the
// two tables are required to be the same text (the runners never see the
// root skill; a shape documented in one and not the other would be a
// template half the agents cannot find).
var skillFiles = []string{
	filepath.Join("..", "..", "SKILL.md"),
	filepath.Join("..", "..", "bots", "whats-next", "skills", "iterion-dsl-quickref.md"),
}

var templateRowRe = regexp.MustCompile("^\\| `([a-z][a-z0-9-]*)` \\|")

// skillSection returns the lines of the file's section titled `title`, up
// to the next heading of the same or a higher level. A line inside a code
// fence is never a heading: a `# comment` of a shell or YAML example ends
// no section.
func skillSection(t *testing.T, path, title string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	level := 0
	inFence := false
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
		}
		heading := !inFence && strings.HasPrefix(line, "#")
		if level == 0 {
			if heading && strings.HasSuffix(line, "# "+title) {
				level = strings.Index(line, " ")
			}
			continue
		}
		if heading && strings.Index(line, " ") <= level {
			break
		}
		out = append(out, line)
	}
	if len(out) == 0 {
		t.Fatalf("%s has no %q section", path, title)
	}
	return out
}

// A `# comment` inside a code fence of a section is not the next heading:
// the section runs past the fence, to the heading that ends it — else two
// sections that differ after such a line would compare equal.
func TestSkillSectionIgnoresAHashInsideACodeFence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "skill.md")
	md := "## A\nbefore\n```sh\n# not a heading\n```\nafter the fence\n## B\nnot in A\n"
	if err := os.WriteFile(path, []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(skillSection(t, path, "A"), "\n")
	if !strings.Contains(got, "after the fence") || strings.Contains(got, "not in A") {
		t.Fatalf("section A read as %q", got)
	}
}

func templateSection(t *testing.T, path string) []string {
	t.Helper()
	return skillSection(t, path, "Start from a template")
}

// TestSkillsCarryTheSameUnwrittenRules: the "Rules the grammar does not
// show" section — what an author cannot read off the syntax tables and
// the authoring probe found agents guessing — is the same text in both
// skills, so a rule written into the one an operator installs also reaches
// the one the runners mirror into a run.
func TestSkillsCarryTheSameUnwrittenRules(t *testing.T) {
	var rules [][]string
	for _, path := range skillFiles {
		section := skillSection(t, path, "Rules the grammar does not show")
		var bullets int
		for _, line := range section {
			if strings.HasPrefix(line, "- **") {
				bullets++
			}
		}
		if bullets < 10 {
			t.Errorf("%s: the rules section carries %d rules, fewer than the probe found agents guessing", path, bullets)
		}
		rules = append(rules, section)
	}
	if strings.TrimSpace(strings.Join(rules[0], "\n")) != strings.TrimSpace(strings.Join(rules[1], "\n")) {
		t.Errorf("the rules sections of %s and %s differ; the two skills carry the same content", skillFiles[0], skillFiles[1])
	}
}

// TestSkillsCarryTheSameTwinSection: the "Writing the twin in YAML"
// section — the loop and the three rules a YAML author needs that a .bot
// author does not — is the same text in both skills, whatever its heading
// level: the runners read the quickref, never the root skill.
func TestSkillsCarryTheSameTwinSection(t *testing.T) {
	var sections []string
	for _, path := range skillFiles {
		section := strings.TrimSpace(strings.Join(skillSection(t, path, "Writing the twin in YAML"), "\n"))
		for _, must := range []string{"iterion validate x.bot.yaml", "iterion fmt --to bot x.bot.yaml", "`: `", "` #`"} {
			if !strings.Contains(section, must) {
				t.Errorf("%s: the twin section does not say %s", path, must)
			}
		}
		sections = append(sections, section)
	}
	if sections[0] != sections[1] {
		t.Errorf("the twin sections of %s and %s differ; the two skills carry the same content", skillFiles[0], skillFiles[1])
	}
}

func TestSkillsNameEveryTemplateAndNothingElse(t *testing.T) {
	var tables [][]string
	for _, path := range skillFiles {
		section := templateSection(t, path)
		named := map[string]bool{}
		var table []string
		for _, line := range section {
			if m := templateRowRe.FindStringSubmatch(line); m != nil {
				named[m[1]] = true
			}
			if strings.HasPrefix(line, "| ") {
				table = append(table, line)
			}
		}
		tables = append(tables, table)
		body := strings.Join(section, "\n")
		for _, tpl := range Templates() {
			if tpl.Spec.Shape != "" && !named[tpl.ID] {
				t.Errorf("%s: shape template %q is not a row of the table", path, tpl.ID)
			}
			if tpl.Spec.Shape == "" && !strings.Contains(body, "`"+tpl.ID+"`") {
				t.Errorf("%s: single-agent template %q is not named in the section", path, tpl.ID)
			}
		}
		for id := range named {
			if _, ok := TemplateByID(id); !ok {
				t.Errorf("%s: the table names %q, which is not a template", path, id)
			}
		}
	}
	if strings.Join(tables[0], "\n") != strings.Join(tables[1], "\n") {
		t.Errorf("the template tables of %s and %s differ; the two skills carry the same content", skillFiles[0], skillFiles[1])
	}
}
