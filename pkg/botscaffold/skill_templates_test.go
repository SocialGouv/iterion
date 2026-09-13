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
// to the next heading of the same or a higher level.
func skillSection(t *testing.T, path, title string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	level := 0
	for _, line := range strings.Split(string(raw), "\n") {
		if level == 0 {
			if strings.HasSuffix(line, "# "+title) {
				level = strings.Index(line, " ")
			}
			continue
		}
		if strings.HasPrefix(line, "#") && strings.Index(line, " ") <= level {
			break
		}
		out = append(out, line)
	}
	if len(out) == 0 {
		t.Fatalf("%s has no %q section", path, title)
	}
	return out
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
