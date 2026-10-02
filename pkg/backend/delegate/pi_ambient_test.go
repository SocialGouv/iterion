package delegate

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/ambient"
)

// piTree lays out pi's precedence cases: an agent dir, a directory above the
// repository, a repository root holding AGENTS.md AND CLAUDE.md (AGENTS.md
// wins), and a work dir holding AGENTS.override.md AND AGENTS.md (the override
// wins).
func piTree(t *testing.T) (agentDir, lab, repo, work string) {
	t.Helper()
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	agentDir = filepath.Join(tmp, "agent")
	lab = filepath.Join(tmp, "home", "lab")
	repo = filepath.Join(lab, "repo")
	work = filepath.Join(repo, "sub")
	mustWriteFile(t, filepath.Join(agentDir, "AGENTS.md"), "AGENT-DIR")
	mustWriteFile(t, filepath.Join(lab, "CLAUDE.md"), "\uFEFFLAB")
	mustWriteFile(t, filepath.Join(repo, "AGENTS.md"), "REPO-AGENTS")
	mustWriteFile(t, filepath.Join(repo, "CLAUDE.md"), "REPO-CLAUDE-SHADOWED")
	mustWriteFile(t, filepath.Join(work, "AGENTS.override.md"), "SUB-OVERRIDE")
	mustWriteFile(t, filepath.Join(work, "AGENTS.md"), "SUB-AGENTS-SHADOWED")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ITERION_PI_AGENT_DIR", agentDir)
	t.Setenv("ITERION_PI_NO_CONTEXT_FILES", "")
	return agentDir, lab, repo, work
}

func paths(files []piContextFile) []string {
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.path
	}
	return out
}

func TestPiAmbientFilesFollowPisOwnRules(t *testing.T) {
	agentDir, lab, repo, work := piTree(t)

	ws := piAmbientFiles(Task{WorkDir: work, AmbientContext: ambient.Workspace})
	if want := []string{filepath.Join(repo, "AGENTS.md"), filepath.Join(work, "AGENTS.override.md")}; !slices.Equal(paths(ws), want) {
		t.Errorf("workspace = %v, want %v (one file per directory, root-most first, nothing above the root)", paths(ws), want)
	}

	op := piAmbientFiles(Task{WorkDir: work, AmbientContext: ambient.Operator})
	if want := []string{filepath.Join(agentDir, "AGENTS.md"), filepath.Join(lab, "CLAUDE.md")}; !slices.Equal(paths(op), want) {
		t.Errorf("operator = %v, want %v (the agent dir first, then the directories above the root)", paths(op), want)
	}
	if len(op) == 2 && op[1].content != "LAB" {
		t.Errorf("the BOM was not stripped: %q", op[1].content)
	}

	for _, p := range []ambient.Policy{ambient.All, ambient.None} {
		if got := piAmbientFiles(Task{WorkDir: work, AmbientContext: p}); got != nil {
			t.Errorf("%v: iterion supplied %v; all is pi's own loading, none is nothing", p, paths(got))
		}
	}
	t.Setenv("ITERION_PI_NO_CONTEXT_FILES", "1")
	if got := piAmbientFiles(Task{WorkDir: work, AmbientContext: ambient.Workspace}); got != nil {
		t.Errorf("ITERION_PI_NO_CONTEXT_FILES=1 must supply nothing, got %v", paths(got))
	}
}

func TestPiAmbientFilesSkipANestedWorktreesMainCheckout(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ITERION_PI_AGENT_DIR", filepath.Join(tmp, "no-agent-dir"))
	t.Setenv("ITERION_PI_NO_CONTEXT_FILES", "")
	main := filepath.Join(tmp, "main")
	wt := filepath.Join(main, ".iterion", "worktrees", "run1")
	for _, args := range [][]string{
		{"init", "-q", "-b", "main", main},
		{"-C", main, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init"},
		{"-C", main, "worktree", "add", "-q", "--detach", wt},
	} {
		if out, err := exec.Command(git, args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	mustWriteFile(t, filepath.Join(main, "AGENTS.md"), "MAIN")
	mustWriteFile(t, filepath.Join(wt, "AGENTS.md"), "WORKTREE")

	if got := paths(piAmbientFiles(Task{WorkDir: wt, AmbientContext: ambient.Workspace})); !slices.Equal(got, []string{filepath.Join(wt, "AGENTS.md")}) {
		t.Errorf("workspace = %v, want the worktree's own file only", got)
	}
	if got := paths(piAmbientFiles(Task{WorkDir: wt, AmbientContext: ambient.Operator})); slices.Contains(got, filepath.Join(main, "AGENTS.md")) {
		t.Errorf("operator = %v: the main checkout is the same repository, not the operator's", got)
	}
}

func TestPiContextSectionIsPisOwnShape(t *testing.T) {
	got := piContextSection([]piContextFile{{"/a/AGENTS.md", "one"}, {"/b/CLAUDE.md", "two"}})
	want := "\n\n<project_context>\n\nProject-specific instructions and guidelines:\n\n" +
		"<project_instructions path=\"/a/AGENTS.md\">\none\n</project_instructions>\n\n" +
		"<project_instructions path=\"/b/CLAUDE.md\">\ntwo\n</project_instructions>\n\n" +
		"</project_context>\n"
	if got != want {
		t.Errorf("section =\n%q\nwant\n%q", got, want)
	}
	if piContextSection(nil) != "" {
		t.Error("no file must render nothing")
	}
}

// TestPiPrintTransportSuppliesThePolicysFiles drives the REAL Execute against a
// stand-in pi that copies the --append-system-prompt file before iterion
// removes it.
func TestPiPrintTransportSuppliesThePolicysFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake CLI is POSIX-only")
	}
	t.Setenv("ITERION_PI_MODE", "print")
	t.Setenv("ITERION_PI_NO_CONTEXT_FILES", "")
	t.Setenv("ITERION_PI_AGENT_DIR", filepath.Join(t.TempDir(), "no-agent-dir"))
	for _, c := range []struct {
		policy     ambient.Policy
		wantFlag   bool
		wantInject bool
	}{
		{ambient.Workspace, true, true},
		{ambient.All, false, false},
	} {
		t.Run(c.policy.String(), func(t *testing.T) {
			workDir, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(workDir, ".git"), 0o755); err != nil {
				t.Fatal(err)
			}
			mustWriteFile(t, filepath.Join(workDir, "AGENTS.md"), "MARKER-PI-REPO-P1")
			binDir := t.TempDir()
			argvDump, promptDump := filepath.Join(binDir, "argv"), filepath.Join(binDir, "prompt")
			script := `#!/bin/sh
printf '%s\n' "$@" > ` + argvDump + `
prev=
for a in "$@"; do if [ "$prev" = --append-system-prompt ]; then cp "$a" ` + promptDump + `; fi; prev=$a; done
cat > /dev/null
printf '%s\n' '{"type":"session","version":3,"id":"s","timestamp":"t","cwd":"/w"}'
printf '%s\n' '{"type":"agent_end","willRetry":false,"messages":[{"role":"assistant","model":"m","responseId":"r","content":[{"type":"text","text":"ok"}],"stopReason":"stop","usage":{"input":1,"output":1,"totalTokens":2,"cost":{"total":0}}}]}'
printf '%s\n' '{"type":"agent_settled"}'
`
			fake := filepath.Join(binDir, "fakepi")
			if err := os.WriteFile(fake, []byte(script), 0o755); err != nil { // #nosec G306 — test fixture must be executable
				t.Fatal(err)
			}
			b := NewPiBackend(testLogger(), fake)
			if _, err := b.Execute(context.Background(), Task{
				NodeID: "n", WorkDir: workDir, BaseDir: workDir,
				SystemPrompt: "be terse", SystemPromptMode: SystemPromptAppendToNative,
				UserPrompt: "x", Model: "openai/gpt-5.5", AmbientContext: c.policy,
			}); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			argv, _ := os.ReadFile(argvDump)
			if got := slices.Contains(strings.Split(string(argv), "\n"), "--no-context-files"); got != c.wantFlag {
				t.Errorf("--no-context-files present=%v, want %v", got, c.wantFlag)
			}
			prompt, _ := os.ReadFile(promptDump)
			injected := strings.Contains(string(prompt), `<project_instructions path="`+filepath.Join(workDir, "AGENTS.md")+`">`) &&
				strings.Contains(string(prompt), "MARKER-PI-REPO-P1")
			if injected != c.wantInject {
				t.Errorf("the repository's AGENTS.md injected=%v, want %v; prompt:\n%s", injected, c.wantInject, prompt)
			}
		})
	}
}

func TestPiRPCSystemPromptCarriesThePolicysFiles(t *testing.T) {
	_, _, repo, work := piTree(t)
	task := Task{WorkDir: work, SystemPrompt: "be terse", AmbientContext: ambient.Workspace}
	got := piComposeSystemPrompt(task)
	if !strings.HasPrefix(got, task.BuildSystemPrompt()) {
		t.Errorf("the composed prompt must lead, unchanged: %q", got)
	}
	if !strings.Contains(got, `<project_instructions path="`+filepath.Join(repo, "AGENTS.md")+`">`) {
		t.Errorf("the RPC prompt lacks the repository's AGENTS.md:\n%s", got)
	}
	if all := piComposeSystemPrompt(Task{WorkDir: work, SystemPrompt: "be terse", AmbientContext: ambient.All}); strings.Contains(all, "<project_context>") {
		t.Errorf("all: pi loads its own files; iterion must add none:\n%s", all)
	}
}
