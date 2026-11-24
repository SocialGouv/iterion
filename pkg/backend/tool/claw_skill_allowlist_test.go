package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The pinned skill mode (iterion#1719): armed, the `skill` tool serves
// exactly the host's allowlist and nothing the workspace planted under a
// walked root is reachable; disarmed, the workspace walk is unchanged.
func TestRegisterClawSkillWithSources_PinnedModeServesOnlyTheAllowlist(t *testing.T) {
	// The run's own skill, in the engine-owned channel root.
	channel := t.TempDir()
	pinned := filepath.Join(channel, "review", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(pinned), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pinned, []byte("---\ndescription: owned\n---\nOwned body.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The checkout: a planted skill of the SAME name the walk would serve,
	// plus one the run never owned.
	workspace := t.TempDir()
	for name, body := range map[string]string{"review": "planted review", "planted": "planted other"} {
		dir := filepath.Join(workspace, ".claude", "skills", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	allowlist := map[string]string{"review": pinned}
	r := NewRegistry()
	if err := RegisterClawSkillWithSources(r, func() string { return workspace },
		func() (map[string]string, bool) { return allowlist, true }); err != nil {
		t.Fatalf("RegisterClawSkillWithSources: %v", err)
	}
	td, err := r.Resolve("skill")
	if err != nil {
		t.Fatalf("Resolve(skill): %v", err)
	}
	call := func(name string) (string, error) {
		in, _ := json.Marshal(map[string]any{"skill": name})
		return td.Execute(context.Background(), in)
	}

	out, err := call("review")
	if err != nil {
		t.Fatalf("the pinned skill = %v, want served", err)
	}
	if !strings.Contains(out, "Owned body.") || strings.Contains(out, "planted") {
		t.Errorf("the pinned name served %q — want the channel file's body, never the planted twin", out)
	}
	if _, err := call("planted"); err == nil || !strings.Contains(err.Error(), "not in this run's allowed skills") {
		t.Errorf("a skill the run does not own = %v, want the pinned-mode refusal", err)
	}

	// Disarmed, the same workspace answers by the walk, as it always has.
	r2 := NewRegistry()
	if err := RegisterClawSkillWithSources(r2, func() string { return workspace },
		func() (map[string]string, bool) { return nil, false }); err != nil {
		t.Fatalf("RegisterClawSkillWithSources (disarmed): %v", err)
	}
	td2, _ := r2.Resolve("skill")
	in, _ := json.Marshal(map[string]any{"skill": "planted"})
	out2, err := td2.Execute(context.Background(), in)
	if err != nil || !strings.Contains(out2, "planted other") {
		t.Errorf("disarmed mode = (%q, %v), want the workspace walk serving the repo's own skill", out2, err)
	}
}

// The provider is consulted per call: the engine arms it AFTER the registry
// exists (the mirror pass runs at run start), and a run that flips armed →
// disarmed must not keep serving the pin.
func TestRegisterClawSkillWithSources_TheProviderIsLive(t *testing.T) {
	workspace := t.TempDir()
	dir := filepath.Join(workspace, ".claude", "skills", "s")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("walked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	armed := false
	r := NewRegistry()
	if err := RegisterClawSkillWithSources(r, func() string { return workspace },
		func() (map[string]string, bool) { return map[string]string{}, armed }); err != nil {
		t.Fatal(err)
	}
	td, _ := r.Resolve("skill")
	in, _ := json.Marshal(map[string]any{"skill": "s"})
	if out, err := td.Execute(context.Background(), in); err != nil || !strings.Contains(out, "walked") {
		t.Fatalf("before arming = (%q, %v), want the walk", out, err)
	}
	armed = true
	if _, err := td.Execute(context.Background(), in); err == nil {
		t.Fatal("armed with an empty set, the walk's own skill must refuse")
	}
	armed = false
	if out, err := td.Execute(context.Background(), in); err != nil || !strings.Contains(out, "walked") {
		t.Fatalf("disarmed again = (%q, %v), want the walk back", out, err)
	}
}
