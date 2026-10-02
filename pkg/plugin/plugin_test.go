package plugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseManifestValid(t *testing.T) {
	m, err := ParseManifest([]byte(`
name: demo
version: 1.0.0
default_enabled: true
contributes:
  rewriters:
    - id: demo
      locate: { bin: demo }
      invoke:
        argv: ["rewrite", "{{command}}"]
`))
	if err != nil {
		t.Fatalf("valid manifest rejected: %v", err)
	}
	if m.Name != "demo" || !m.DefaultEnabled || len(m.Contributes.Rewriters) != 1 {
		t.Fatalf("parsed manifest wrong: %+v", m)
	}
}

func TestParseManifestCommandsAndAgents(t *testing.T) {
	m, err := ParseManifest([]byte(`
name: toolkit
contributes:
  commands:
    - commands/ship.md
  agents:
    - agents/reviewer.md
`))
	if err != nil {
		t.Fatalf("commands/agents manifest rejected: %v", err)
	}
	kinds := m.Kinds()
	if len(kinds) != 2 || kinds[0] != "command" || kinds[1] != "agent" {
		t.Fatalf("Kinds = %v, want [command agent]", kinds)
	}
}

func TestParseManifestRejects(t *testing.T) {
	cases := map[string]string{
		"no name":           "contributes:\n  skills: [a.md]\n",
		"contributes none":  "name: x\n",
		"rewriter no cmd":   "name: x\ncontributes:\n  rewriters:\n    - id: y\n      locate: { bin: y }\n      invoke: { argv: [\"rewrite\"] }\n",
		"mcp stdio no cmd":  "name: x\ncontributes:\n  mcp_servers:\n    - { name: s, transport: stdio }\n",
		"mcp bad transport": "name: x\ncontributes:\n  mcp_servers:\n    - { name: s, transport: carrier-pigeon, command: c }\n",
		"run_env bad name":  "name: x\ncontributes:\n  rewriters:\n    - id: y\n      locate: { bin: y }\n      invoke: { argv: [\"rewrite\", \"{{command}}\"] }\n      run_env: { \"A=B\": \"1\" }\n",
		"run_env no name":   "name: x\ncontributes:\n  rewriters:\n    - id: y\n      locate: { bin: y }\n      invoke: { argv: [\"rewrite\", \"{{command}}\"] }\n      run_env: { \"\": \"1\" }\n",
		"run_env shell":     "name: x\ncontributes:\n  rewriters:\n    - id: y\n      locate: { bin: y }\n      invoke: { argv: [\"rewrite\", \"{{command}}\"] }\n      run_env: { \"A;touch /tmp/x;B\": \"1\" }\n",
		"run_env digit":     "name: x\ncontributes:\n  rewriters:\n    - id: y\n      locate: { bin: y }\n      invoke: { argv: [\"rewrite\", \"{{command}}\"] }\n      run_env: { \"1A\": \"1\" }\n",
	}
	for label, doc := range cases {
		if _, err := ParseManifest([]byte(doc)); err == nil {
			t.Errorf("%s: expected error, got nil", label)
		}
	}
}

func TestLoadBuiltinsAndEnableState(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())
	reg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// The three builtins must be present.
	for _, name := range []string{"rtk", "graphify", "repo-falcon"} {
		if _, ok := reg.Get(name); !ok {
			t.Errorf("builtin %q missing", name)
		}
	}
	// rtk enabled by default; KG explorers disabled.
	if !reg.IsEnabled("rtk") {
		t.Error("rtk should be enabled by default")
	}
	if reg.IsEnabled("repo-falcon") {
		t.Error("repo-falcon should be disabled by default")
	}
	// Enable persists and survives a reload.
	if err := reg.SetEnabled("repo-falcon", true); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	reg2, err := Load()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !reg2.IsEnabled("repo-falcon") {
		t.Error("enable state did not persist across reload")
	}
	// A builtin cannot be uninstalled.
	if err := reg2.Remove("rtk"); err == nil {
		t.Error("removing a builtin should error")
	}
}

// rtk keeps every command it runs and the lines a compressed output left out,
// in stores of its own that outlive the run: the builtin turns both off for
// the shells iterion compresses. Its history has no switch: the database path
// is one nothing can create — its parent exists and is no directory, so rtk
// neither makes the database nor chmods the directory it would live in (it
// makes that directory owner-only first: /dev itself, for /dev/null).
func TestTheBuiltinRtkRunsWithItsStoresOff(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())
	reg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	specs := reg.EnabledRewriterSpecs()
	if len(specs) != 1 || specs[0].ID != "rtk" {
		t.Fatalf("EnabledRewriterSpecs = %+v, want [rtk]", specs)
	}
	got := specs[0].RunEnv
	if len(got) != 2 || got["RTK_RECALL"] != "0" {
		t.Errorf("rtk's run_env = %v, want its recall store off", got)
	}
	db := got["RTK_DB_PATH"]
	if !filepath.IsAbs(db) {
		t.Fatalf("rtk's history path %q is not absolute", db)
	}
	if fi, err := os.Stat(filepath.Dir(db)); err != nil || fi.IsDir() {
		t.Errorf("rtk's history path %q: its parent must exist and be no directory (stat: %v, %v) — rtk would create the database there, and chmod the directory", db, fi, err)
	}
}

func TestEnabledRewritersAndSkills(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())
	reg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Only rtk is enabled by default and it is the lone rewriter.
	rw := reg.EnabledRewriters()
	if len(rw) != 1 || rw[0].Plugin != "rtk" {
		t.Fatalf("EnabledRewriters = %+v, want [rtk]", rw)
	}
	// repo-falcon's skill is readable from the embedded FS.
	p, _ := reg.Get("repo-falcon")
	files, err := p.SkillFiles()
	if err != nil {
		t.Fatalf("SkillFiles: %v", err)
	}
	if len(files) != 1 || files[0].Name != "code-knowledge-graph.md" || len(files[0].Content) == 0 {
		t.Fatalf("repo-falcon skill wrong: %+v", files)
	}
}

// A pack shipping skills in the Agent Skills directory form carries each
// skill's name on its DIRECTORY. Mirroring on the base name alone collapses
// every such pack onto one file named "SKILL.md" — the second skill overwrites
// the first, and both are named "SKILL".
func TestMirrorFilesNamesADirectoryFormSkillAfterItsDirectory(t *testing.T) {
	dir := t.TempDir()
	write := func(rel string) {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		if err := os.WriteFile(full, []byte("# "+rel), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	rels := []string{
		"skills/adversarial-review-loop/SKILL.md",
		"skills/house-style/SKILL.md",
		"skills/graphify.md",         // flat form: the name is already the file's
		"skills/lower-case/skill.md", // a pack authored on a case-insensitive filesystem
		"skills/skill.md",            // no directory to name it: a file called "skill"
		"skills/SKILL.md",            // under the kind dir: the path carries no name
		"SKILL.md",                   // root form: the whole pack is one file
	}
	for _, rel := range rels {
		write(rel)
	}
	write("commands/skill.md") // read by the command assertion at the end

	p := &Plugin{
		Manifest: Manifest{Name: "pack", Contributes: Contributes{Skills: rels}},
		fsys:     os.DirFS(dir),
	}
	files, err := p.SkillFiles()
	if err != nil {
		t.Fatalf("SkillFiles: %v", err)
	}
	// The last two carry no name in their path, so they fall back to the
	// PLUGIN's name — never to the constant "SKILL.md", which would make every
	// root-form pack collide with every other one. Both landing on "pack.md"
	// is a SELF-collision, resolved last-wins like any other: one pack has no
	// reason to ship both shapes, and the mirror says so out loud.
	want := []string{
		"adversarial-review-loop.md", "house-style.md", "graphify.md",
		"lower-case.md", "skill.md", "pack.md", "pack.md",
	}
	if len(files) != len(want) {
		t.Fatalf("SkillFiles returned %d files, want %d: %+v", len(files), len(want), files)
	}
	for i, f := range files {
		if f.Name != want[i] {
			t.Errorf("file %d mirrors as %q, want %q", i, f.Name, want[i])
		}
		if len(f.Content) == 0 {
			t.Errorf("file %d (%s) mirrored empty", i, f.Name)
		}
	}
	// The two directory-form packs must not collide on one destination name.
	if files[0].Name == files[1].Name {
		t.Fatalf("both directory-form skills mirror as %q — one overwrites the other", files[0].Name)
	}

	// A command literally called "skill" must stay `/skill`, not become
	// `/<plugin>`: with no directory to carry a name, only the exact spelling
	// is the Agent Skills sentinel.
	cmd := &Plugin{
		Manifest: Manifest{Name: "pack", Contributes: Contributes{Commands: []string{"commands/skill.md"}}},
		fsys:     os.DirFS(dir),
	}
	cmds, err := cmd.MirrorFiles(MirrorKind{Name: "command", Dir: "commands"})
	if err != nil {
		t.Fatalf("MirrorFiles(command): %v", err)
	}
	if len(cmds) != 1 || cmds[0].Name != "skill.md" {
		t.Errorf("commands/skill.md mirrors as %+v, want skill.md", cmds)
	}
}

// A mirrored name is joined onto the workspace's .claude/<kind>/. A base name
// is structurally one path element; a PLUGIN name is not — a hand-written
// plugin.yaml carries whatever its author typed, and only synthesized
// manifests are normalized. Unnormalized, "../x" writes outside the directory
// iterion owns and is then reported as owned, and an ordinary scoped name like
// "org/pack" fails run setup on a directory nobody created.
func TestMirrorFilesNeverLetsAPluginNameLeaveItsDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("body"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ pluginName, want string }{
		{"../escaped", "escaped.md"},
		{"../../../etc/passwd", "passwd.md"},
		{"org/pack", "pack.md"},
		{"Weird Name!", "weird-name.md"},
		{"..", "skill-library.md"},
		{"plain", "plain.md"},
	} {
		p := &Plugin{
			Manifest: Manifest{Name: tc.pluginName, Contributes: Contributes{Skills: []string{"SKILL.md"}}},
			fsys:     os.DirFS(dir),
		}
		files, err := p.SkillFiles()
		if err != nil {
			t.Fatalf("%q: SkillFiles: %v", tc.pluginName, err)
		}
		if len(files) != 1 {
			t.Errorf("plugin %q produced %d files, want 1", tc.pluginName, len(files))
			continue
		}
		// Asserted FIRST and unconditionally: gated behind the equality check
		// below, this could only ever run on names already known to be safe.
		if got := files[0].Name; strings.ContainsAny(got, `/\`) || strings.Contains(got, "..") {
			t.Errorf("plugin %q produced a traversable name %q", tc.pluginName, got)
		}
		if files[0].Name != tc.want {
			t.Errorf("plugin %q mirrors as %q, want %q", tc.pluginName, files[0].Name, tc.want)
		}
	}
}

func TestExpandContext(t *testing.T) {
	e := ExpandContext{Workspace: "/ws", PluginDir: "/p", CacheDir: "/c"}
	got := e.Expand("serve {{workspace}}/.falcon {{plugin.cache}} {{plugin.dir}}")
	want := "serve /ws/.falcon /c /p"
	if got != want {
		t.Fatalf("Expand = %q, want %q", got, want)
	}
}

// A run_env value rides every compressed command (an export prefix): a
// control character in it makes the claude_code CLI refuse the command.
func TestARunEnvValueWithAControlCharacterIsRejected(t *testing.T) {
	for _, v := range []string{`"a\x1bb"`, `"a\u0000b"`, `"a\nb"`} {
		doc := "name: x\ncontributes:\n  rewriters:\n    - id: y\n      locate: { bin: y }\n      invoke: { argv: [\"rewrite\", \"{{command}}\"] }\n      run_env: { A: " + v + " }\n"
		if _, err := ParseManifest([]byte(doc)); err == nil {
			t.Errorf("run_env value %s accepted", v)
		}
	}
	ok := "name: x\ncontributes:\n  rewriters:\n    - id: y\n      locate: { bin: y }\n      invoke: { argv: [\"rewrite\", \"{{command}}\"] }\n      run_env: { A: \"/dev/null/x.db\" }\n"
	if _, err := ParseManifest([]byte(ok)); err != nil {
		t.Errorf("a plain value refused: %v", err)
	}
}
