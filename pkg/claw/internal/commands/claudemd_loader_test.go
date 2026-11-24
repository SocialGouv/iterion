package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeMdLoader_FindsAncestorFiles(t *testing.T) {
	dir := t.TempDir()
	parent := filepath.Join(dir, "parent")
	child := filepath.Join(parent, "child")
	writeFile(t, filepath.Join(parent, "CLAUDE.md"), "## /parent-cmd\nDoes parent stuff.\n")
	writeFile(t, filepath.Join(child, "CLAUDE.md"), "## /child-cmd\nDoes child stuff.\n")

	r := NewRegistry()
	if err := LoadClaudeMdCommands(r, child); err != nil {
		t.Fatalf("LoadClaudeMdCommands: %v", err)
	}
	for _, name := range []string{"child-cmd", "parent-cmd"} {
		if _, err := r.Execute("/"+name, nil); err != nil {
			t.Errorf("/%s execute: %v", name, err)
		}
	}
}

func TestClaudeMdLoader_ParsesSlashBlocks(t *testing.T) {
	src := `# Project

Some intro.

## /alpha
First command body.
Second line of alpha.

## /beta
Beta description.

## Section that is not a slash command
Regular section.

## /gamma
Gamma command body.
`
	r := NewRegistry()
	cmds, err := parseClaudeMdCommands(strings.NewReader(src), "test.md")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	names := make([]string, 0, len(cmds))
	for _, c := range cmds {
		names = append(names, c.Name)
	}
	want := []string{"alpha", "beta", "gamma"}
	if len(names) != len(want) {
		t.Fatalf("expected %d commands, got %d (%v)", len(want), len(names), names)
	}
	for i, n := range want {
		if names[i] != n {
			t.Errorf("at %d: got %q want %q", i, names[i], n)
		}
	}
	// Description should be the first body line.
	for _, c := range cmds {
		if c.Description == "" {
			t.Errorf("command %q has empty description", c.Name)
		}
	}
	_ = r
}

func TestClaudeMdLoader_LeafWinsOnConflict(t *testing.T) {
	dir := t.TempDir()
	parent := filepath.Join(dir, "parent")
	child := filepath.Join(parent, "child")
	writeFile(t, filepath.Join(parent, "CLAUDE.md"), "## /shared\nFROM PARENT\n")
	writeFile(t, filepath.Join(child, "CLAUDE.md"), "## /shared\nFROM CHILD\n")

	r := NewRegistry()
	if err := LoadClaudeMdCommands(r, child); err != nil {
		t.Fatalf("load: %v", err)
	}
	// Capture stdout via a pipe to confirm the child body ran.
	old := os.Stdout
	rPipe, wPipe, _ := os.Pipe()
	os.Stdout = wPipe
	if _, err := r.Execute("/shared", nil); err != nil {
		t.Errorf("execute: %v", err)
	}
	wPipe.Close()
	os.Stdout = old
	out, _ := readAll(rPipe)
	if !strings.Contains(out, "FROM CHILD") {
		t.Errorf("expected child body in output, got:\n%s", out)
	}
}

func TestClaudeMdLoader_RegistersAsDynamicCommands(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "CLAUDE.md"), "## /dynamic\nThis was loaded from CLAUDE.md.\n")
	r := NewRegistry()
	before := r.commands["dynamic"]
	_ = before
	if err := LoadClaudeMdCommands(r, dir); err != nil {
		t.Fatalf("load: %v", err)
	}
	cmd, ok := r.commands["dynamic"]
	if !ok {
		t.Fatal("expected 'dynamic' command registered")
	}
	if cmd.Handler == nil {
		t.Error("expected non-nil handler")
	}
	if !cmd.ResumeSupported {
		t.Error("expected ResumeSupported=true on auto-loaded commands")
	}
}

func TestClaudeMdLoader_HandlesEmptyFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "CLAUDE.md"), "")
	r := NewRegistry()
	if err := LoadClaudeMdCommands(r, dir); err != nil {
		t.Errorf("expected nil error on empty CLAUDE.md, got %v", err)
	}
}

func TestClaudeMdLoader_HandlesFileWithNoSlashHeaders(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "CLAUDE.md"), `# Project

Just regular markdown — no slash commands here.

## Architecture

Some prose about how things work.

## /not-a-cmd-because-no-slash-trailer
Wait, this one IS a slash command actually.
`)
	r := NewRegistry()
	if err := LoadClaudeMdCommands(r, dir); err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, ok := r.commands["not-a-cmd-because-no-slash-trailer"]; !ok {
		t.Error("expected the one slash header to register")
	}
}

func TestClaudeMdLoader_IgnoresHeaderWithOnlySlash(t *testing.T) {
	// A bare "## /" should not register a command with empty name.
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "CLAUDE.md"), `## /
This is body without a name.

## /valid
With a name.
`)
	r := NewRegistry()
	if err := LoadClaudeMdCommands(r, dir); err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, ok := r.commands[""]; ok {
		t.Error("registry must not contain a zero-name command")
	}
	if _, ok := r.commands["valid"]; !ok {
		t.Error("expected /valid to register")
	}
}

func TestClaudeMdLoader_PreservesBodyInHandler(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "CLAUDE.md"), `## /multi-line
First line.

Second line after blank.

Third line.
`)
	r := NewRegistry()
	if err := LoadClaudeMdCommands(r, dir); err != nil {
		t.Fatalf("load: %v", err)
	}

	old := os.Stdout
	rPipe, wPipe, _ := os.Pipe()
	os.Stdout = wPipe
	if _, err := r.Execute("/multi-line", nil); err != nil {
		t.Errorf("execute: %v", err)
	}
	wPipe.Close()
	os.Stdout = old
	out, _ := readAll(rPipe)
	for _, want := range []string{"First line.", "Second line", "Third line."} {
		if !strings.Contains(out, want) {
			t.Errorf("body line %q missing from output:\n%s", want, out)
		}
	}
}

func TestClaudeMdLoader_PassesArgsToHandler(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "CLAUDE.md"), "## /takes-args\nDoes a thing.\n")
	r := NewRegistry()
	if err := LoadClaudeMdCommands(r, dir); err != nil {
		t.Fatalf("load: %v", err)
	}

	old := os.Stdout
	rPipe, wPipe, _ := os.Pipe()
	os.Stdout = wPipe
	if _, err := r.Execute("/takes-args foo bar baz", nil); err != nil {
		t.Errorf("execute: %v", err)
	}
	wPipe.Close()
	os.Stdout = old
	out, _ := readAll(rPipe)
	if !strings.Contains(out, "args: foo bar baz") {
		t.Errorf("expected args echoed, got:\n%s", out)
	}
}

func TestClaudeMdLoader_HonoursResumeSupported(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "CLAUDE.md"), "## /x\nbody\n")
	r := NewRegistry()
	if err := LoadClaudeMdCommands(r, dir); err != nil {
		t.Fatalf("load: %v", err)
	}
	cmd := r.commands["x"]
	if !cmd.ResumeSupported {
		t.Error("expected ResumeSupported=true on auto-loaded commands so they survive resume")
	}
}

func TestClaudeMdLoader_NilRegistryReturnsError(t *testing.T) {
	if err := LoadClaudeMdCommands(nil, t.TempDir()); err == nil {
		t.Error("expected error on nil registry")
	}
}

func TestExtractCommandName(t *testing.T) {
	cases := []struct{ in, out string }{
		{"## /foo", "foo"},
		{"## /foo bar baz", "foo"},
		{"## /Camel-Case", "camel-case"},
		{"## not-a-cmd", "not-a-cmd"},
	}
	for _, tc := range cases {
		if got := extractCommandName(tc.in); got != tc.out {
			t.Errorf("extractCommandName(%q) = %q, want %q", tc.in, got, tc.out)
		}
	}
}

func readAll(r *os.File) (string, error) {
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			sb.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}
	return sb.String(), nil
}
