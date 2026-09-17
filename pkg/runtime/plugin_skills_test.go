package runtime

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// installPack writes an enabled single-skill plugin into an ITERION_HOME.
func installPack(t *testing.T, home, name, rel, body string) {
	t.Helper()
	full := filepath.Join(home, "plugins", name, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := "name: " + name + "\nversion: 1.0.0\nschema_version: 1\ndefault_enabled: true\n" +
		"contributes:\n  skills:\n    - " + rel + "\n"
	if err := os.WriteFile(filepath.Join(home, "plugins", name, "plugin.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Two plugins contributing ONE name land on one destination at the same tier,
// where reconcileSkillFile's precedence guard does not apply: it overwrites
// without a word, and the winner is the order Enabled() returns. The overwrite
// is left alone — refusing it would break a workspace relying on the incumbent
// — but a silent content substitution between plugins must not be how an
// operator finds out. The destination must also be reported once, not twice.
func TestMirrorPluginContributions_SameNameCollisionIsLoudAndReportedOnce(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ITERION_HOME", home)
	installPack(t, home, "aa-pack", "skills/graphify.md", "AA BODY\n")
	installPack(t, home, "zz-pack", "skills/graphify.md", "ZZ BODY\n")

	var buf bytes.Buffer
	logger := iterlog.New(iterlog.LevelWarn, &buf)
	workDir := t.TempDir()
	owned, err := mirrorPluginContributions(workDir, nil, logger)
	if err != nil {
		t.Fatalf("mirrorPluginContributions: %v", err)
	}

	logs := buf.String()
	for _, want := range []string{"graphify.md", "aa-pack", "zz-pack"} {
		if !strings.Contains(logs, want) {
			t.Errorf("collision warning does not name %q; logs = %q", want, logs)
		}
	}
	if len(owned) != 1 {
		t.Errorf("owned = %v, want the one destination reported once", owned)
	}
	// The collision itself is unchanged: last mirror still wins.
	got, err := os.ReadFile(filepath.Join(workDir, ".claude", "skills", "graphify.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "ZZ BODY\n" {
		t.Errorf("mirrored body = %q, want the last-mirrored plugin's", got)
	}
}

// A pack can collide with ITSELF: two files of one plugin that mirror to one
// name. Naming "both aa-pack and aa-pack" would be unactionable noise, so that
// case gets its own sentence.
func TestMirrorPluginContributions_SelfCollisionNamesThePluginOnce(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ITERION_HOME", home)
	for _, rel := range []string{"skills/dup.md", "skills/nested/dup.md"} {
		full := filepath.Join(home, "plugins", "solo", filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("body of "+rel+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	manifest := "name: solo\nversion: 1.0.0\nschema_version: 1\ndefault_enabled: true\n" +
		"contributes:\n  skills:\n    - skills/dup.md\n    - skills/nested/dup.md\n"
	if err := os.WriteFile(filepath.Join(home, "plugins", "solo", "plugin.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	logger := iterlog.New(iterlog.LevelWarn, &buf)
	owned, err := mirrorPluginContributions(t.TempDir(), nil, logger)
	if err != nil {
		t.Fatalf("mirrorPluginContributions: %v", err)
	}
	if len(owned) != 1 {
		t.Errorf("owned = %v, want the one destination reported once", owned)
	}
	logs := buf.String()
	if !strings.Contains(logs, "contributes two skills that mirror to the same name") {
		t.Errorf("self-collision is not reported as one plugin's own; logs = %q", logs)
	}
	if strings.Contains(logs, `both "solo" and "solo"`) {
		t.Errorf("self-collision names the plugin twice; logs = %q", logs)
	}
}

// Same collision, but a workspace copy differs from both contributions and
// wins. "The later mirror replaces the earlier" is then FALSE — neither
// plugin's bytes land — and a warning that names the wrong winner is worse
// than no warning, because it is what the operator will act on.
func TestMirrorPluginContributions_CollisionWarningTellsTheTruthWhenTheWorkspaceWins(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ITERION_HOME", home)
	installPack(t, home, "aa-pack", "skills/graphify.md", "AA BODY\n")
	installPack(t, home, "zz-pack", "skills/graphify.md", "ZZ BODY\n")

	workDir := t.TempDir()
	dest := filepath.Join(workDir, ".claude", "skills")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, "graphify.md"), []byte("WORKSPACE BODY\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	logger := iterlog.New(iterlog.LevelWarn, &buf)
	owned, err := mirrorPluginContributions(workDir, nil, logger)
	if err != nil {
		t.Fatalf("mirrorPluginContributions: %v", err)
	}
	if len(owned) != 0 {
		t.Errorf("owned = %v, want none: the workspace copy won", owned)
	}
	got, err := os.ReadFile(filepath.Join(dest, "graphify.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "WORKSPACE BODY\n" {
		t.Fatalf("workspace copy was overwritten: %q", got)
	}
	logs := buf.String()
	if !strings.Contains(logs, "is contributed by both") {
		t.Errorf("the collision itself went unreported; logs = %q", logs)
	}
	// The warning must not name a winner — here neither plugin is one, and an
	// earlier version that inferred the winner from the outcome enum said so
	// wrongly in both directions.
	for _, lie := range []string{"wins this pass", "NEITHER landed", "aa-pack wins", "zz-pack wins"} {
		if strings.Contains(logs, lie) {
			t.Errorf("collision warning claims an outcome it cannot know (%q); logs = %q", lie, logs)
		}
	}
}

// Two plugins shipping byte-identical content collide on a destination and
// lose nothing. Telling the operator to "rename one" would be noise, and the
// identity is knowable for certain — unlike the winner.
func TestMirrorPluginContributions_IdenticalContributionsCollideSilently(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ITERION_HOME", home)
	installPack(t, home, "aa-pack", "skills/graphify.md", "SAME BODY\n")
	installPack(t, home, "zz-pack", "skills/graphify.md", "SAME BODY\n")

	var buf bytes.Buffer
	logger := iterlog.New(iterlog.LevelWarn, &buf)
	workDir := t.TempDir()
	if _, err := mirrorPluginContributions(workDir, nil, logger); err != nil {
		t.Fatalf("mirrorPluginContributions: %v", err)
	}
	if logs := buf.String(); strings.Contains(logs, "is contributed by both") {
		t.Errorf("identical contributions warned about; logs = %q", logs)
	}
}

// End of the chain, not one link of it: a pack installed in the Agent Skills
// directory form ("skills/<name>/SKILL.md" — what `npx skills add` publishes
// and claude_code's Skill tool discovers) must reach the workspace under its
// OWN name. On the file's base name alone every such pack mirrors as
// ".claude/skills/SKILL.md": the skill is called "SKILL", and a second pack of
// the same shape silently replaces the first.
func TestMirrorPluginContributions_DirectoryFormSkillKeepsItsName(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ITERION_HOME", home)

	skillDir := filepath.Join(home, "plugins", "pack", "skills", "adversarial-review-loop")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: adversarial-review-loop\ndescription: break your own change\n---\nbody\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := "name: pack\nversion: 1.0.0\nschema_version: 1\ndefault_enabled: true\n" +
		"contributes:\n  skills:\n    - skills/adversarial-review-loop/SKILL.md\n"
	if err := os.WriteFile(filepath.Join(home, "plugins", "pack", "plugin.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	workDir := t.TempDir()
	owned, err := mirrorPluginContributions(workDir, nil, nil)
	if err != nil {
		t.Fatalf("mirrorPluginContributions: %v", err)
	}
	want := filepath.Join(workDir, ".claude", "skills", "adversarial-review-loop.md")
	if len(owned) != 1 || owned[0] != want {
		t.Fatalf("owned = %v, want [%s]", owned, want)
	}
	got, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("the skill did not reach the workspace under its own name: %v", err)
	}
	if string(got) != body {
		t.Errorf("mirrored content = %q, want the pack's own body", got)
	}
	// The name the pack would have collapsed to must NOT be what landed.
	if _, err := os.Stat(filepath.Join(workDir, ".claude", "skills", "SKILL.md")); err == nil {
		t.Error("mirrored as SKILL.md — the skill is named after the file, not its pack")
	}
}
