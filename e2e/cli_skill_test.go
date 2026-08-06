package e2e

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/SocialGouv/iterion/pkg/skilllib"
)

// `iterion skill` is the operator's half of the skill library (ADR-059):
// the store a `.bot`'s `skills:` field resolves against at run start. The
// store package is unit-covered and the runtime mirror has its own test;
// what was untested is the COMMAND surface — and, more importantly, that
// what the command writes is what the RUNTIME reads back. The layout is a
// contract between the two halves: the runtime only mirrors the canonical
// `<name>/SKILL.md` directory form.
//
// Mutation check: write the flat `<name>.md` form instead and the
// runtime-resolver assertion fails; stop honouring the project scope and
// the shadowing assertion fails; make `rm` a no-op and the removal
// assertions fail; break frontmatter parsing and `list` loses the
// description an operator picks a skill by.

const (
	skillGlobalBody = `---
name: repo-survey
description: survey a repository before touching it
---

Read the tree before you edit it.
`
	skillProjectBody = `---
name: repo-survey
description: the project override of the survey ritual
---

This repo keeps its map in docs/.
`
)

// isolateSkillLibrary points the machine-global iterion data dir at a temp
// dir so the test never reads or writes the operator's real library.
// Returns the project store dir to pass as --store-dir.
func isolateSkillLibrary(t *testing.T) string {
	t.Helper()
	t.Setenv("ITERION_HOME", t.TempDir())
	return t.TempDir()
}

// skillBodyFile writes a body to a temp file, the way `--from` takes it.
func skillBodyFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "body.md")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write skill body: %v", err)
	}
	return path
}

func runSkill(t *testing.T, fn func(*cli.Printer, cli.SkillOptions) error, opts cli.SkillOptions, format cli.OutputFormat) string {
	t.Helper()
	var buf bytes.Buffer
	p := &cli.Printer{W: &buf, Format: format}
	if err := fn(p, opts); err != nil {
		t.Fatalf("skill command %+v: %v", opts, err)
	}
	return buf.String()
}

// listedSkills decodes `skill list --json`.
func listedSkills(t *testing.T, projectDir string) map[string]skilllib.LibrarySkill {
	t.Helper()
	out := runSkill(t, cli.RunSkillList, cli.SkillOptions{StoreDir: projectDir}, cli.OutputJSON)
	var skills []skilllib.LibrarySkill
	if err := json.Unmarshal([]byte(out), &skills); err != nil {
		t.Fatalf("decode skill list from %q: %v", out, err)
	}
	byName := make(map[string]skilllib.LibrarySkill, len(skills))
	for _, s := range skills {
		byName[s.Name] = s
	}
	return byName
}

func TestSkillLibraryAddListShowExportRemove(t *testing.T) {
	projectDir := isolateSkillLibrary(t)

	// --- add (global scope) ---
	runSkill(t, cli.RunSkillAdd, cli.SkillOptions{
		Name:     "repo-survey",
		From:     skillBodyFile(t, skillGlobalBody),
		StoreDir: projectDir,
	}, cli.OutputHuman)

	// The runtime resolves library skills through exactly this call at run
	// start. If the command wrote a shape the resolver does not recognise,
	// the skill would be silently absent from every run that references it.
	resolver := skilllib.LocalStoreForProject(projectDir)
	srcPath, ok := resolver.Resolve("repo-survey")
	if !ok {
		t.Fatalf("the runtime resolver does not see the skill the CLI just added")
	}
	if filepath.Base(srcPath) != "SKILL.md" || filepath.Base(filepath.Dir(srcPath)) != "repo-survey" {
		t.Errorf("stored at %s, want the canonical <name>/SKILL.md directory form", srcPath)
	}
	if data, err := os.ReadFile(srcPath); err != nil {
		t.Fatalf("read stored skill: %v", err)
	} else if string(data) != skillGlobalBody {
		t.Errorf("stored body = %q, want the body passed to --from", string(data))
	}

	// --- list surfaces it with its scope and frontmatter description ---
	{
		got := listedSkills(t, projectDir)
		sk, present := got["repo-survey"]
		if !present {
			t.Fatalf("skill list omitted the added skill; got %v", got)
		}
		if sk.Scope != skilllib.ScopeGlobal {
			t.Errorf("scope = %q, want %q", sk.Scope, skilllib.ScopeGlobal)
		}
		if sk.Description != "survey a repository before touching it" {
			t.Errorf("description = %q, want the frontmatter description", sk.Description)
		}
	}

	// --- show returns the full body ---
	{
		out := runSkill(t, cli.RunSkillShow, cli.SkillOptions{
			Name: "repo-survey", StoreDir: projectDir,
		}, cli.OutputJSON)
		var sk skilllib.LibrarySkill
		if err := json.Unmarshal([]byte(out), &sk); err != nil {
			t.Fatalf("decode skill show: %v", err)
		}
		if !strings.Contains(sk.Body, "Read the tree before you edit it.") {
			t.Errorf("show body = %q, want the stored body", sk.Body)
		}
	}

	// --- export writes the skill out where the operator asked ---
	exportDir := filepath.Join(t.TempDir(), "nested", "out")
	{
		runSkill(t, cli.RunSkillExport, cli.SkillOptions{
			Name: "repo-survey", StoreDir: projectDir, Dir: exportDir,
		}, cli.OutputHuman)
		data, err := os.ReadFile(filepath.Join(exportDir, "repo-survey.md"))
		if err != nil {
			t.Fatalf("read exported skill: %v", err)
		}
		if string(data) != skillGlobalBody {
			t.Errorf("exported body = %q, want the stored body", string(data))
		}
	}

	// --- rm deletes it from the library AND from the runtime resolver ---
	runSkill(t, cli.RunSkillRemove, cli.SkillOptions{
		Name: "repo-survey", StoreDir: projectDir,
	}, cli.OutputHuman)

	if _, stillThere := skilllib.LocalStoreForProject(projectDir).Resolve("repo-survey"); stillThere {
		t.Errorf("the runtime resolver still sees a removed skill")
	}
	if got := listedSkills(t, projectDir); len(got) != 0 {
		t.Errorf("skill list after rm = %v, want empty", got)
	}
	// The export copy is the operator's, not the library's — removal must
	// not reach outside the store.
	if _, err := os.Stat(filepath.Join(exportDir, "repo-survey.md")); err != nil {
		t.Errorf("rm deleted the exported copy too: %v", err)
	}
}

// The library is layered: a project skill shadows a same-named global one,
// which is the whole point of the per-project override. A run in this
// project must see the project body, and removing the project layer must
// uncover the global one rather than leaving a hole.
func TestSkillLibraryProjectScopeShadowsGlobal(t *testing.T) {
	projectDir := isolateSkillLibrary(t)

	runSkill(t, cli.RunSkillAdd, cli.SkillOptions{
		Name: "repo-survey", From: skillBodyFile(t, skillGlobalBody), StoreDir: projectDir,
	}, cli.OutputHuman)
	runSkill(t, cli.RunSkillAdd, cli.SkillOptions{
		Name: "repo-survey", From: skillBodyFile(t, skillProjectBody), StoreDir: projectDir, Project: true,
	}, cli.OutputHuman)

	// One entry, not two: the name is shadowed, not duplicated.
	got := listedSkills(t, projectDir)
	if len(got) != 1 {
		t.Fatalf("skill list = %v, want a single shadowed entry", got)
	}
	if sk := got["repo-survey"]; sk.Scope != skilllib.ScopeProject {
		t.Errorf("scope = %q, want the project layer to win", sk.Scope)
	}

	// What a run would actually read.
	resolved, ok := skilllib.LocalStoreForProject(projectDir).Resolve("repo-survey")
	if !ok {
		t.Fatalf("resolver lost the skill")
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		t.Fatalf("read resolved skill: %v", err)
	}
	if !strings.Contains(string(data), "This repo keeps its map in docs/.") {
		t.Errorf("a run would read %q, want the project override body", string(data))
	}

	// Removing the project layer uncovers the global one.
	runSkill(t, cli.RunSkillRemove, cli.SkillOptions{
		Name: "repo-survey", StoreDir: projectDir, Project: true,
	}, cli.OutputHuman)

	after := listedSkills(t, projectDir)
	sk, present := after["repo-survey"]
	if !present {
		t.Fatalf("removing the project override also removed the global skill; list = %v", after)
	}
	if sk.Scope != skilllib.ScopeGlobal {
		t.Errorf("after removing the override, scope = %q, want %q", sk.Scope, skilllib.ScopeGlobal)
	}
	if sk.Description != "survey a repository before touching it" {
		t.Errorf("uncovered description = %q, want the global one", sk.Description)
	}
}
