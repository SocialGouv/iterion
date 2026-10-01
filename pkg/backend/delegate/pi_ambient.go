package delegate

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/ambient"
)

// piContextCandidates is pi's per-directory precedence: the first existing
// regular file wins, one file per directory (pi 0.84.3,
// dist/core/resource-loader.js, loadContextFileFromDir).
var piContextCandidates = []string{"AGENTS.override.md", "AGENTS.md", "AGENTS.MD", "CLAUDE.md", "CLAUDE.MD"}

// piContextFile is one context file as pi renders it.
type piContextFile struct {
	path    string
	content string
}

// piNoContextFiles reports the operator's raw switch, ITERION_PI_NO_CONTEXT_FILES=1:
// no context file at all, the policy notwithstanding (a cost lever).
func piNoContextFiles() bool {
	return strings.TrimSpace(os.Getenv("ITERION_PI_NO_CONTEXT_FILES")) == "1"
}

// piUsesNativeContextFiles reports whether pi may load context files itself.
// Its walk always reaches the filesystem root and adds the agent dir's file,
// which is exactly workspace and operator together: only `all` maps onto it.
// Every other policy turns pi's loading off (--no-context-files) and has
// iterion supply the files it allows (piAmbientFiles).
func piUsesNativeContextFiles(task Task) bool {
	return task.AmbientContext == ambient.All && !piNoContextFiles()
}

// piAgentDir is the directory pi reads its global context file from: the
// directory iterion pins (ITERION_PI_AGENT_DIR, see piResolveEnv), else the
// operator's PI_CODING_AGENT_DIR, else pi's default ~/.pi/agent.
func piAgentDir() string {
	for _, name := range []string{"ITERION_PI_AGENT_DIR", "PI_CODING_AGENT_DIR"} {
		if dir := strings.TrimSpace(os.Getenv(name)); dir != "" {
			return dir
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".pi", "agent")
}

// piContextFileFromDir applies pi's per-directory precedence.
func piContextFileFromDir(dir string) (piContextFile, bool) {
	for _, name := range piContextCandidates {
		path := filepath.Join(dir, name)
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		content, err := os.ReadFile(path)
		if err != nil {
			continue // pi warns and moves on to the next candidate
		}
		return piContextFile{path: path, content: strings.TrimPrefix(string(content), "\uFEFF")}, true
	}
	return piContextFile{}, false
}

// piAmbientFiles resolves, with pi's own rules, the context files the node's
// policy lets through when pi's loading is off, in pi's order: the agent dir's
// file first, then directories root-most first.
//
//   - workspace: the directories from the work dir up to the repository root;
//   - operator: the agent dir, and the directories above the operator
//     boundary (ambient.OperatorBoundary).
//
// The directories between the repository root and the boundary — a nested
// worktree's main checkout — are the same repository's, shadowed by the
// worktree's own files, as pi's native loader treats them: never loaded here.
func piAmbientFiles(task Task) []piContextFile {
	if piUsesNativeContextFiles(task) || piNoContextFiles() || task.AmbientContext == ambient.None {
		return nil
	}
	workDir, err := filepath.Abs(task.WorkDir)
	if err != nil {
		return nil
	}
	root := ambient.RepoRoot(workDir)
	boundary := ambient.OperatorBoundary(root)
	sep := string(filepath.Separator)

	var files []piContextFile
	seen := map[string]bool{}
	add := func(f piContextFile, ok bool) {
		if ok && !seen[f.path] {
			seen[f.path] = true
			files = append(files, f)
		}
	}
	if task.AmbientContext.IncludesOperator() {
		if dir := piAgentDir(); dir != "" {
			add(piContextFileFromDir(dir))
		}
	}
	dirs := append([]string{workDir}, ambient.AncestorsAbove(workDir)...)
	for i := len(dirs) - 1; i >= 0; i-- {
		d := dirs[i]
		switch {
		case d == root || strings.HasPrefix(d, strings.TrimSuffix(root, sep)+sep):
			if task.AmbientContext.IncludesWorkspace() {
				add(piContextFileFromDir(d))
			}
		case d != boundary && (d == sep || strings.HasPrefix(boundary, strings.TrimSuffix(d, sep)+sep)):
			if task.AmbientContext.IncludesOperator() {
				add(piContextFileFromDir(d))
			}
		}
	}
	return files
}

// piContextSection renders files exactly as pi 0.84.3 renders its own
// (dist/core/system-prompt.js): appended to the --append-system-prompt text,
// it lands where pi's native block would, so the agent reads the same shape.
func piContextSection(files []piContextFile) string {
	if len(files) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n<project_context>\n\n")
	b.WriteString("Project-specific instructions and guidelines:\n\n")
	for _, f := range files {
		b.WriteString(`<project_instructions path="` + f.path + "\">\n" + f.content + "\n</project_instructions>\n\n")
	}
	b.WriteString("</project_context>\n")
	return b.String()
}

// piSystemPromptSuffix is pi's contribution to the composed system prompt:
// the context files its policy allows, rendered, when pi's own loading is off.
func piSystemPromptSuffix(task Task) string {
	return piContextSection(piAmbientFiles(task))
}

// piComposeSystemPrompt is the system prompt the RPC transport hands pi: the
// composed prompt plus the policy's context files. The print transport gets
// the same suffix through the CLI-agent protocol hook (SystemPromptSuffix).
func piComposeSystemPrompt(task Task) string {
	return task.BuildSystemPrompt() + piSystemPromptSuffix(task)
}
