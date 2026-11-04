package delegate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pi's project_context block is text pi would have loaded itself, supplied
// by iterion instead for every policy but `all`: the bytes come off the
// workspace's own disk, so no Task FIELD ever held them and the executor's
// field pass never saw them. They cross the model boundary through the pass
// the task carries (Task.ScreenModelText), applied once in
// piSystemPromptSuffix — the one function both transports route through.
//
// The pass here is a stand-in that maps the known value to its placeholder,
// exactly what Guard.RedactForModel does to it; that the task carries the
// GUARD's pass is pinned on the executor's side
// (TestTheBoundaryHandsTheTaskItsOwnPass).
func TestThePiProjectContextCrossesTheBoundaryTheTaskCarries(t *testing.T) {
	// t.Setenv pins piAgentDir, so this one is not parallel.
	const sentinel = "ghp-pi-postseam-7a1b3d4f9c2e"
	const placeholder = "__ITERION_SECRET_api_token__"

	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatalf("workspace: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"),
		[]byte("# rules\n\ndeploy with "+sentinel+"\n"), 0o600); err != nil {
		t.Fatalf("workspace AGENTS.md: %v", err)
	}
	// ITERION_PI_AGENT_DIR is read by piAgentDir: pinned at a directory with
	// no context file so the operator's own never joins this assertion.
	t.Setenv("ITERION_PI_AGENT_DIR", t.TempDir())
	t.Setenv("ITERION_PI_NO_CONTEXT_FILES", "")

	task := Task{
		SystemPrompt:    "base",
		WorkDir:         dir,
		ScreenModelText: func(s string) string { return strings.ReplaceAll(s, sentinel, placeholder) },
	}
	// Premise: without the pass the file's text is what pi would read.
	bare := task
	bare.ScreenModelText = nil
	if !strings.Contains(piSystemPromptSuffix(bare), sentinel) {
		t.Fatalf("the walk read no AGENTS.md, so this test proves nothing: %q", piSystemPromptSuffix(bare))
	}

	// The RPC transport, the print transport's protocol hook, and the
	// suffix itself: one function, so one pass covers all three.
	for name, got := range map[string]string{
		"piSystemPromptSuffix":          piSystemPromptSuffix(task),
		"piComposeSystemPrompt (RPC)":   piComposeSystemPrompt(task),
		"piProtocol.SystemPromptSuffix": piProtocol.SystemPromptSuffix(task),
	} {
		if strings.Contains(got, sentinel) {
			t.Errorf("%s hands pi the value the workspace file carries: %q", name, got)
		}
		if !strings.Contains(got, placeholder) {
			t.Errorf("%s lost the value without leaving its placeholder: %q", name, got)
		}
		if !strings.Contains(got, "<project_context>") {
			t.Errorf("%s no longer renders pi's own block shape: %q", name, got)
		}
	}
}
