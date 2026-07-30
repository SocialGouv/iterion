package runtime

import (
	"os"
	"path/filepath"
	"testing"
)

// The regression these tests guard: a cloud runner pod's iterion home is empty,
// so mirrorPluginContributions / mirrorLibrarySkills used to resolve NOTHING
// there and an operator-installed plugin's skill silently never reached the
// workspace. Injecting a pre-resolved payload must reproduce the local result.

func TestMirrorInjectedPluginFiles_WritesEachKind(t *testing.T) {
	workDir := t.TempDir()
	files := []ContributionFile{
		{Kind: "skills", Name: "deploy-target.md", Content: []byte("# deploy\nplaybook\n")},
		{Kind: "commands", Name: "ship.md", Content: []byte("ship it\n")},
		{Kind: "agents", Name: "auditor.md", Content: []byte("audit\n")},
	}
	if _, err := mirrorInjectedPluginFiles(workDir, files, nil); err != nil {
		t.Fatalf("mirror: %v", err)
	}
	for _, f := range files {
		got, err := os.ReadFile(filepath.Join(workDir, ".claude", f.Kind, f.Name))
		if err != nil {
			t.Fatalf("%s/%s not mirrored: %v", f.Kind, f.Name, err)
		}
		if string(got) != string(f.Content) {
			t.Errorf("%s/%s content = %q, want %q", f.Kind, f.Name, got, f.Content)
		}
	}
}

func TestMirrorInjectedPluginFiles_NoopOnEmpty(t *testing.T) {
	workDir := t.TempDir()
	if _, err := mirrorInjectedPluginFiles(workDir, nil, nil); err != nil {
		t.Fatalf("mirror: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workDir, ".claude")); !os.IsNotExist(err) {
		t.Errorf("empty payload should touch no filesystem, got err=%v", err)
	}
}

// A hand-authored workspace file must WIN over an injected plugin file, exactly
// as it does on the local path (precedence: bundle > plugin > library > hand).
func TestMirrorInjectedPluginFiles_ShadowedByWorkspaceFile(t *testing.T) {
	workDir := t.TempDir()
	dest := filepath.Join(workDir, ".claude", "skills")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	mine := []byte("MY hand-authored version\n")
	if err := os.WriteFile(filepath.Join(dest, "deploy-target.md"), mine, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := mirrorInjectedPluginFiles(workDir, []ContributionFile{
		{Kind: "skills", Name: "deploy-target.md", Content: []byte("plugin version\n")},
	}, nil)
	if err != nil {
		t.Fatalf("mirror: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(dest, "deploy-target.md"))
	if string(got) != string(mine) {
		t.Errorf("workspace file was clobbered: got %q, want %q", got, mine)
	}
}

// Library skills must land as <name>/SKILL.md (the directory form is the only
// shape claude_code's Skill tool discovers), and carry their description hint.
func TestMirrorInjectedLibrarySkills_DirectoryFormAndHints(t *testing.T) {
	workDir := t.TempDir()
	hints, err := mirrorInjectedLibrarySkills(workDir, []LibrarySkillFile{
		{Name: "deploy-target", Description: "Deploy an app and return a URL", Content: []byte("---\nname: deploy-target\n---\nbody\n")},
	}, nil)
	if err != nil {
		t.Fatalf("mirror: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(workDir, ".claude", "skills", "deploy-target", "SKILL.md"))
	if err != nil {
		t.Fatalf("skill not mirrored in directory form: %v", err)
	}
	if want := "---\nname: deploy-target\n---\nbody\n"; string(got) != want {
		t.Errorf("content = %q, want %q", got, want)
	}
	if hints["deploy-target"] != "Deploy an app and return a URL" {
		t.Errorf("hint = %q, want the frontmatter description", hints["deploy-target"])
	}
}

// A non-nil payload is AUTHORITATIVE: it must suppress local resolution
// entirely, so an empty payload mirrors nothing even if plugins are installed
// on the host running the test.
func TestMirrorPluginContributions_InjectedSuppressesLocalResolution(t *testing.T) {
	workDir := t.TempDir()
	if _, err := mirrorPluginContributions(workDir, &Contributions{}, nil); err != nil {
		t.Fatalf("mirror: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workDir, ".claude")); !os.IsNotExist(err) {
		t.Errorf("an empty injected payload must mirror nothing (no local fallback), got err=%v", err)
	}
}

func TestContributions_IsEmpty(t *testing.T) {
	var nilC *Contributions
	if !nilC.IsEmpty() {
		t.Error("nil should be empty")
	}
	if !(&Contributions{}).IsEmpty() {
		t.Error("zero value should be empty")
	}
	if (&Contributions{Plugin: []ContributionFile{{Kind: "skills", Name: "a.md"}}}).IsEmpty() {
		t.Error("payload with a plugin file should not be empty")
	}
	if (&Contributions{Library: []LibrarySkillFile{{Name: "a"}}}).IsEmpty() {
		t.Error("payload with a library skill should not be empty")
	}
}
