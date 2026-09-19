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

// Same collision, but the workspace has ITS OWN copies of both discovery
// shapes and wins — neither plugin's bytes land — and a warning that names
// the wrong winner is worse than no warning, because it is what the operator
// will act on.
//
// The workspace override must supply BOTH shapes to defeat the mirror in
// full, per the "each form is independent" contract mirrorFileSkill states
// on itself (bundle.go). A workspace with only the flat alias still lets the
// directory form land, because the two forms are two distinct destinations —
// which is documented and mirrored across all three tiers (bundle, plugin,
// library).
func TestMirrorPluginContributions_CollisionWarningTellsTheTruthWhenTheWorkspaceWins(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ITERION_HOME", home)
	installPack(t, home, "aa-pack", "skills/graphify.md", "AA BODY\n")
	installPack(t, home, "zz-pack", "skills/graphify.md", "ZZ BODY\n")

	workDir := t.TempDir()
	dest := filepath.Join(workDir, ".claude", "skills")
	if err := os.MkdirAll(filepath.Join(dest, "graphify"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, "graphify.md"), []byte("WORKSPACE FLAT\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, "graphify", "SKILL.md"), []byte("WORKSPACE DIR\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	logger := iterlog.New(iterlog.LevelWarn, &buf)
	owned, err := mirrorPluginContributions(workDir, nil, logger)
	if err != nil {
		t.Fatalf("mirrorPluginContributions: %v", err)
	}
	if len(owned) != 0 {
		t.Errorf("owned = %v, want none: the workspace copies won", owned)
	}
	gotFlat, err := os.ReadFile(filepath.Join(dest, "graphify.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(gotFlat) != "WORKSPACE FLAT\n" {
		t.Fatalf("workspace flat alias was overwritten: %q", gotFlat)
	}
	gotDir, err := os.ReadFile(filepath.Join(dest, "graphify", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(gotDir) != "WORKSPACE DIR\n" {
		t.Fatalf("workspace directory form was overwritten: %q", gotDir)
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
	// The DIRECTORY form is the one claude_code's Skill tool discovers (Agent
	// Skills spec — bundle.go doc + ADR-079), and the one a backend is
	// handed. The flat alias must also land so a prompt Reading the skill by
	// path resolves.
	wantOwned := filepath.Join(workDir, ".claude", "skills", "adversarial-review-loop", "SKILL.md")
	if len(owned) != 1 || owned[0] != wantOwned {
		t.Fatalf("owned = %v, want [%s]", owned, wantOwned)
	}
	got, err := os.ReadFile(wantOwned)
	if err != nil {
		t.Fatalf("the skill's directory form did not reach the workspace: %v", err)
	}
	if string(got) != body {
		t.Errorf("directory-form content = %q, want the pack's own body", got)
	}
	flat := filepath.Join(workDir, ".claude", "skills", "adversarial-review-loop.md")
	gotFlat, err := os.ReadFile(flat)
	if err != nil {
		t.Fatalf("the flat alias did not reach the workspace under its own name: %v", err)
	}
	if string(gotFlat) != body {
		t.Errorf("flat alias content = %q, want the pack's own body", gotFlat)
	}
	// The name the pack would have collapsed to must NOT be what landed at
	// the skills-dir root — that was the pre-#1372 collapse (the skill is
	// named after the file, not its pack).
	if _, err := os.Stat(filepath.Join(workDir, ".claude", "skills", "SKILL.md")); err == nil {
		t.Error("mirrored as SKILL.md at the skills-dir root — the skill is named after the file, not its pack")
	}
}

// #1373: a plugin skill must land as BOTH the directory form <name>/SKILL.md
// (the only shape claude_code's Skill tool discovers, per the Agent Skills
// spec — bundle.go doc + ADR-079) and the flat alias <name>.md that prompt
// Reads by path resolve. Before this change, the plugin mirror wrote only
// the flat form, which claude_code did NOT discover — so every plugin-
// contributed skill was invisible to it, and reachable only by claw or by a
// prompt that Read the path explicitly. That would have been a silent
// backend-parity gap, exactly what CLAUDE.md's backend-parity addendum names.
//
// Mutation: replace mirrorFileSkill with a bare reconcileSkillFile at the
// flat destination (the pre-#1373 shape) and the directory-form assertion
// goes red, along with the owned path.
func TestMirrorPluginContributions_SkillLandsInBothForms(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ITERION_HOME", home)
	installPack(t, home, "the-pack", "skills/graphify.md", "content\n")

	workDir := t.TempDir()
	owned, err := mirrorPluginContributions(workDir, nil, nil)
	if err != nil {
		t.Fatalf("mirrorPluginContributions: %v", err)
	}
	dest := filepath.Join(workDir, ".claude", "skills")
	dirForm := filepath.Join(dest, "graphify", "SKILL.md")
	flat := filepath.Join(dest, "graphify.md")

	// The directory form MUST be on disk — this is what claude_code
	// discovers, and iterion's backend-parity contract requires it.
	if _, err := os.Stat(dirForm); err != nil {
		t.Fatalf("the directory form is missing at %s (claude_code Skill tool would not discover this skill): %v", dirForm, err)
	}
	if _, err := os.Stat(flat); err != nil {
		t.Fatalf("the flat alias is missing at %s (path-based prompt Reads would fail): %v", flat, err)
	}

	// owned reports the directory form — the discoverable one a backend is
	// handed. The flat file is a convenience for explicit-path reads and is
	// not owned (backends discover the directory form).
	if len(owned) != 1 || owned[0] != dirForm {
		t.Fatalf("owned = %v, want [%s]", owned, dirForm)
	}

	// The marker layout mirrors mirrorBundleSkills: <stem>.SKILL.md.sha256
	// keys the directory form; <stem>.md.sha256 keys the flat alias.
	markerDir := filepath.Join(dest, ".iterion-managed")
	for _, m := range []string{"graphify.SKILL.md.sha256", "graphify.md.sha256"} {
		if _, err := os.Stat(filepath.Join(markerDir, m)); err != nil {
			t.Errorf("marker %s is missing (%v) — a next run cannot tell iterion's own file from a workspace edit", m, err)
		}
	}
}

// The cloud-path twin of #1373: a plugin skill arriving on the wire from the
// publisher (queue.Contributions) must also land in BOTH forms. Before this
// change, mirrorInjectedPluginFiles wrote only the flat form — so a cloud
// runner-pod run would have had a plugin skill claude_code did not discover,
// while a local run got the same skill in both shapes. The two paths were
// asymmetric on the same defect.
//
// Mutation: skip the mirrorFileSkill branch (route through the flat
// reconcileSkillFile) and both the directory-form file and the owned path go
// red.
func TestMirrorInjectedPluginFiles_SkillLandsInBothForms(t *testing.T) {
	workDir := t.TempDir()
	owned, err := mirrorInjectedPluginFiles(workDir, []ContributionFile{
		{Kind: "skills", Name: "deploy.md", Content: []byte("playbook\n")},
	}, nil)
	if err != nil {
		t.Fatalf("mirrorInjectedPluginFiles: %v", err)
	}
	dest := filepath.Join(workDir, ".claude", "skills")
	dirForm := filepath.Join(dest, "deploy", "SKILL.md")
	flat := filepath.Join(dest, "deploy.md")
	if _, err := os.Stat(dirForm); err != nil {
		t.Fatalf("cloud path did not write the directory form: %v", err)
	}
	if _, err := os.Stat(flat); err != nil {
		t.Fatalf("cloud path did not write the flat alias: %v", err)
	}
	if len(owned) != 1 || owned[0] != dirForm {
		t.Fatalf("owned = %v, want [%s]", owned, dirForm)
	}
}

// Commands and agents keep the FLAT shape. claude_code discovers
// .claude/commands/<name>.md and .claude/agents/<name>.md directly; there is
// no directory-form contract for those kinds, and shipping one would be dead
// files.
//
// Mutation: route commands and agents through mirrorFileSkill too and the
// directory-form Stat below goes red on files that must not exist.
func TestMirrorInjectedPluginFiles_CommandsAndAgentsAreFlat(t *testing.T) {
	workDir := t.TempDir()
	_, err := mirrorInjectedPluginFiles(workDir, []ContributionFile{
		{Kind: "commands", Name: "deploy.md", Content: []byte("a command")},
		{Kind: "agents", Name: "scout.md", Content: []byte("an agent")},
	}, nil)
	if err != nil {
		t.Fatalf("mirrorInjectedPluginFiles: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workDir, ".claude", "commands", "deploy.md")); err != nil {
		t.Fatalf("command should be flat: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workDir, ".claude", "agents", "scout.md")); err != nil {
		t.Fatalf("agent should be flat: %v", err)
	}
	// The directory form must NOT exist for a non-skill kind — a
	// commands/deploy/SKILL.md is a discovered SKILL, not a slash command.
	if _, err := os.Stat(filepath.Join(workDir, ".claude", "commands", "deploy", "SKILL.md")); err == nil {
		t.Error("command was mirrored in the skill directory form")
	}
	if _, err := os.Stat(filepath.Join(workDir, ".claude", "agents", "scout", "SKILL.md")); err == nil {
		t.Error("agent was mirrored in the skill directory form")
	}
}

// The cloud-injected mirror shares the same collision guard as the local
// path — a same-(kind,name) duplicate in the queue payload is reported once
// and warned about, so a publisher regression that stops deduping cannot
// silently substitute two teams' skills on the runner. This is the defence
// in depth #1374 asked for; the primary line of defence is the publisher
// dedup (see cloudpublisher tests).
//
// Mutation: remove the contribClaimTracker calls in mirrorInjectedPluginFiles
// and either the duplicate log OR the len(owned)==1 assertion goes red.
func TestMirrorInjectedPluginFiles_DuplicatePayloadIsLoudAndReportedOnce(t *testing.T) {
	var buf bytes.Buffer
	logger := iterlog.New(iterlog.LevelWarn, &buf)
	workDir := t.TempDir()
	owned, err := mirrorInjectedPluginFiles(workDir, []ContributionFile{
		{Kind: "skills", Name: "deploy.md", Content: []byte("A version\n")},
		{Kind: "skills", Name: "deploy.md", Content: []byte("B version\n")},
	}, logger)
	if err != nil {
		t.Fatalf("mirrorInjectedPluginFiles: %v", err)
	}
	dirForm := filepath.Join(workDir, ".claude", "skills", "deploy", "SKILL.md")
	if len(owned) != 1 || owned[0] != dirForm {
		t.Fatalf("owned = %v, want the one destination reported once", owned)
	}
	logs := buf.String()
	if !strings.Contains(logs, "duplicate skills contribution") || !strings.Contains(logs, "deploy.md") {
		t.Errorf("duplicate not warned about; logs = %q", logs)
	}
	// The winner is the LATER entry in the payload — that mirrors the write
	// order of the publisher and a redelivery replays it the same way.
	got, _ := os.ReadFile(dirForm)
	if string(got) != "B version\n" {
		t.Errorf("dirForm content = %q, want the later entry's", got)
	}
}

// The injected mirror's duplicate WARN fires EVEN on byte-identical entries.
// A duplicate reaching this path is by definition a publisher-dedup
// regression (see resolveContributionsFor step-0/step-1), so silence would
// hide exactly the case the WARN was added to catch: identical bytes were
// what F1 of the round-1 adversarial review found — the publisher missed a
// dedup and the runtime WARN swallowed the miss because the second write
// returned skillOutcomeUpToDate.
//
// Mutation: reintroduce `outcome != skillOutcomeUpToDate` on the duplicate
// branch of mirrorInjectedPluginFiles and this test goes red.
func TestMirrorInjectedPluginFiles_DuplicateIdenticalBytesStillWarn(t *testing.T) {
	var buf bytes.Buffer
	logger := iterlog.New(iterlog.LevelWarn, &buf)
	workDir := t.TempDir()
	if _, err := mirrorInjectedPluginFiles(workDir, []ContributionFile{
		{Kind: "skills", Name: "deploy.md", Content: []byte("same body\n")},
		{Kind: "skills", Name: "deploy.md", Content: []byte("same body\n")},
	}, logger); err != nil {
		t.Fatalf("mirrorInjectedPluginFiles: %v", err)
	}
	logs := buf.String()
	if !strings.Contains(logs, "duplicate skills contribution") || !strings.Contains(logs, "deploy.md") {
		t.Errorf("identical-byte duplicate went unwarned; logs = %q", logs)
	}
}

// A plugin skill named with a case-insensitive .md extension (`Deploy.MD`,
// `Whats-Next.Md`) must land in both discovery shapes just like a lowercase
// one. Regression test for revi/review PR #1479 medium
// (R6178bd/Rda9e59): `collectSkillFiles` accepts `.MD` via `EqualFold`,
// but `skillDestDirForm` used a case-sensitive `TrimSuffix(name, ".md")`
// leaving stem == name — `skillDir` and `flatDest` resolved to the SAME
// path, `mkdirAll` made it a directory, `reconcileSkillFile` then hashed a
// directory and returned EISDIR, and the whole plugin mirror aborted.
//
// Mutation: revert skillDestDirForm to case-sensitive TrimSuffix and this
// test reddens (mkdir-then-hash-directory error abandons the mirror).
func TestMirrorPluginContributions_UppercaseMdExtensionMirrorsBothShapes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ITERION_HOME", home)
	// Install a plugin whose only skill has an uppercase .MD extension.
	pluginDir := filepath.Join(home, "plugins", "shouty")
	if err := os.MkdirAll(filepath.Join(pluginDir, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "skills", "Deploy.MD"), []byte("body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := "name: shouty\nversion: 1.0.0\nschema_version: 1\ndefault_enabled: true\n" +
		"contributes:\n  skills:\n    - skills/Deploy.MD\n"
	if err := os.WriteFile(filepath.Join(pluginDir, "plugin.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	workDir := t.TempDir()
	owned, err := mirrorPluginContributions(workDir, nil, nil)
	if err != nil {
		t.Fatalf("mirrorPluginContributions returned an error on an .MD-extension skill: %v", err)
	}
	dest := filepath.Join(workDir, ".claude", "skills")
	dirForm := filepath.Join(dest, "Deploy", "SKILL.md")
	flat := filepath.Join(dest, "Deploy.MD")
	if _, err := os.Stat(dirForm); err != nil {
		t.Fatalf("directory form missing at %s: %v", dirForm, err)
	}
	if _, err := os.Stat(flat); err != nil {
		t.Fatalf("flat alias missing at %s: %v", flat, err)
	}
	if len(owned) != 1 || owned[0] != dirForm {
		t.Fatalf("owned = %v, want [%s]", owned, dirForm)
	}
}

// One malformed contribution (no .md extension) must be skipped with a
// WARN naming it, never abort the whole pass. Before the fix,
// `mirrorPluginContributions` returned `(nil, err)` on the FIRST such
// name and every OTHER plugin's skills/commands/agents never landed.
// Regression test for revi/review PR #1479 medium (R6178bd/Rda9e59).
//
// Mutation: put the `return nil, fmt.Errorf(...)` back on the per-file
// path and the assertion "the sibling plugin's skill landed" reddens.
func TestMirrorPluginContributions_OneMalformedNameDoesNotDiscardOtherPlugins(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ITERION_HOME", home)

	// Plugin A ships a name with no .md extension at all — that name is
	// invalid for skillDestDirForm and previously killed the whole pass.
	brokenDir := filepath.Join(home, "plugins", "a-broken")
	if err := os.MkdirAll(filepath.Join(brokenDir, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(brokenDir, "skills", "no-extension"), []byte("body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(brokenDir, "plugin.yaml"),
		[]byte("name: a-broken\nversion: 1.0.0\nschema_version: 1\ndefault_enabled: true\ncontributes:\n  skills:\n    - skills/no-extension\n"),
		0o644); err != nil {
		t.Fatal(err)
	}

	// Plugin B ships a legitimate skill and MUST land.
	goodDir := filepath.Join(home, "plugins", "z-good")
	if err := os.MkdirAll(filepath.Join(goodDir, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(goodDir, "skills", "kept.md"), []byte("kept-body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(goodDir, "plugin.yaml"),
		[]byte("name: z-good\nversion: 1.0.0\nschema_version: 1\ndefault_enabled: true\ncontributes:\n  skills:\n    - skills/kept.md\n"),
		0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	logger := iterlog.New(iterlog.LevelWarn, &buf)
	workDir := t.TempDir()
	owned, err := mirrorPluginContributions(workDir, nil, logger)
	if err != nil {
		t.Fatalf("one malformed plugin file aborted the whole pass: %v", err)
	}

	dirForm := filepath.Join(workDir, ".claude", "skills", "kept", "SKILL.md")
	if _, err := os.Stat(dirForm); err != nil {
		t.Fatalf("z-good's skill did not land — a-broken's failure discarded it: %v", err)
	}
	found := false
	for _, p := range owned {
		if p == dirForm {
			found = true
		}
	}
	if !found {
		t.Errorf("owned = %v does not name z-good's skill", owned)
	}
	if logs := buf.String(); !strings.Contains(logs, "no-extension") || !strings.Contains(logs, "a-broken") {
		t.Errorf("the malformed file was not named in the WARN; logs = %q", logs)
	}
}

// Same class on the CLOUD (injected) path. One malformed entry in the
// queue payload must not discard every subsequent (kind, name) —
// mirrorInjectedPluginFiles had the same abort pattern as its local twin
// and the same fix applies (WARN + continue).
//
// Mutation: put the `return nil, fmt.Errorf(...)` back in
// mirrorInjectedPluginFiles and this test reddens on the "kept.md" file
// not landing.
func TestMirrorInjectedPluginFiles_OneMalformedEntryDoesNotDiscardTheRest(t *testing.T) {
	var buf bytes.Buffer
	logger := iterlog.New(iterlog.LevelWarn, &buf)
	workDir := t.TempDir()
	_, err := mirrorInjectedPluginFiles(workDir, []ContributionFile{
		{Kind: "skills", Name: "broken", Content: []byte("no extension")},
		{Kind: "skills", Name: "kept.md", Content: []byte("still lands\n")},
	}, logger)
	if err != nil {
		t.Fatalf("one malformed entry aborted the whole pass: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workDir, ".claude", "skills", "kept", "SKILL.md")); err != nil {
		t.Fatalf("subsequent entry did not land: %v", err)
	}
	if logs := buf.String(); !strings.Contains(logs, "broken") {
		t.Errorf("malformed entry not named in the WARN; logs = %q", logs)
	}
}
