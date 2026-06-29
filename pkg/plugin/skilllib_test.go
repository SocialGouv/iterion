package plugin

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeName(t *testing.T) {
	cases := map[string]string{
		"https://github.com/acme/Cool-Skills.git": "cool-skills",
		"git@github.com:acme/cool_skills.git":     "cool-skills",
		"/home/jo/my skills/":                     "my-skills",
		"Awesome.Claude.Skills":                   "awesome-claude-skills",
		"":                                        "skill-library",
	}
	for in, want := range cases {
		if got := NormalizeName(in); got != want {
			t.Errorf("NormalizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSynthesizeSkillsManifest_SkillsDir(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "skills", "a", "SKILL.md"), "---\nname: a\n---\nbody")
	mustWrite(t, filepath.Join(dir, "skills", "b.md"), "---\nname: b\n---\nbody")
	mustWrite(t, filepath.Join(dir, "README.md"), "ignore me")

	m, err := SynthesizeSkillsManifest("acme/cool-skills", dir)
	if err != nil {
		t.Fatalf("synthesize: %v", err)
	}
	if m.Name != "cool-skills" || m.DefaultEnabled {
		t.Fatalf("manifest meta wrong: name=%q default=%v", m.Name, m.DefaultEnabled)
	}
	want := []string{"skills/a/SKILL.md", "skills/b.md"}
	if len(m.Contributes.Skills) != 2 || m.Contributes.Skills[0] != want[0] || m.Contributes.Skills[1] != want[1] {
		t.Fatalf("skills = %v, want %v", m.Contributes.Skills, want)
	}
	// Must pass the manifest validator (skills-only is a valid contribution).
	if err := m.Validate(); err != nil {
		t.Fatalf("synthesized manifest invalid: %v", err)
	}
}

func TestSynthesizeSkillsManifest_RootFallback(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "SKILL.md"), "---\nname: solo\n---\nbody")
	mustWrite(t, filepath.Join(dir, "README.md"), "ignored")

	m, err := SynthesizeSkillsManifest("solo-skill", dir)
	if err != nil {
		t.Fatalf("synthesize: %v", err)
	}
	if len(m.Contributes.Skills) != 1 || m.Contributes.Skills[0] != "SKILL.md" {
		t.Fatalf("root fallback skills = %v, want [SKILL.md]", m.Contributes.Skills)
	}
}

func TestSynthesizeSkillsManifest_Empty(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "README.md"), "no skills here")
	if _, err := SynthesizeSkillsManifest("x", dir); err == nil {
		t.Fatal("expected error for a dir with no skills")
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
