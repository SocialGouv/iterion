package ciguard

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
)

// workflowScriptRef matches a repository shell script a workflow names:
// a `.sh` path under scripts/ or ci/, with or without a leading `./`. The
// left boundary keeps a nested path (bots/x/scripts/y.sh) from being read as
// the top-level scripts/y.sh.
var workflowScriptRef = regexp.MustCompile(`(?:^|[^A-Za-z0-9_./-])((?:\./)?(?:scripts|ci)/[A-Za-z0-9_./-]+\.sh)\b`)

// executableMode is the git index mode of an executable regular file. The
// index, not the working tree's stat, is what a CI checkout applies, and it
// stays truthful in a clone with core.filemode=false.
const executableMode = "100755"

// scriptModeFinding is one workflow reference to a tracked script whose
// index mode is not executable.
type scriptModeFinding struct {
	workflow, script, mode string
	// target is the path a symlinked script resolves to through the index,
	// empty for a script that is not a symlink; mode is then the target's.
	target string
}

// symlinkMode is the git index mode of a symbolic link; its blob is the
// link target.
const symlinkMode = "120000"

// maxSymlinkHops bounds the resolution of a symlinked script, so a link
// cycle in the index is reported instead of looping.
const maxSymlinkHops = 8

// workflowScriptModes scans the workflows of the repository at root for
// script references and checks each tracked one against the git index. It
// returns the tracked scripts referenced (deduplicated, sorted) and every
// reference whose index mode is not 100755. A symlinked script is judged by
// the index mode of the path it resolves to. A reference to an untracked or
// missing path is not this guard's subject and is skipped.
//
// Callers must not be t.Parallel: the cache-input chdir below is
// process-wide.
func workflowScriptModes(t *testing.T, root string, workflows []string) (tracked []string, findings []scriptModeFinding) {
	t.Helper()
	// The index is read only by the git children below, so `go test` would
	// cache a PASS that an index-only mode change cannot invalidate. The
	// stat makes the index a cache input when it lies under the module root
	// (a primary checkout); `go test` drops stat inputs outside the module
	// root, which is where a linked worktree keeps its index
	// (<primary>/.git/worktrees/<name>/index). A chdir target is hashed with
	// no module-root filter, and every index write renames index.lock inside
	// that git dir, changing the dir's mtime: chdir there and back.
	indexPath := gittest.Run(t, root, "rev-parse", "--path-format=absolute", "--git-path", "index")
	if _, err := os.Stat(indexPath); err != nil {
		t.Fatalf("stat git index %s: %v", indexPath, err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(filepath.Dir(indexPath)); err != nil {
		t.Fatalf("chdir %s: %v", filepath.Dir(indexPath), err)
	}
	if err := os.Chdir(wd); err != nil {
		t.Fatalf("chdir back %s: %v", wd, err)
	}

	modes := map[string]string{}
	for line := range strings.SplitSeq(gittest.Run(t, root, "ls-files", "--stage", "--", "scripts", "ci"), "\n") {
		if line == "" {
			continue
		}
		meta, path, ok := strings.Cut(line, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 {
			t.Fatalf("unparsable `git ls-files --stage` line in %s: %q", root, line)
		}
		modes[path] = fields[0]
	}

	seen := map[string]bool{}
	for _, wf := range workflows {
		src := mustRead(t, wf)
		reported := map[string]bool{}
		for _, m := range workflowScriptRef.FindAllStringSubmatch(string(src), -1) {
			script := strings.TrimPrefix(m[1], "./")
			mode, ok := modes[script]
			if !ok {
				continue
			}
			seen[script] = true
			if reported[script] {
				continue
			}
			target := ""
			if mode == symlinkMode {
				target, mode = resolveIndexSymlink(t, root, modes, script)
			}
			if mode != executableMode {
				reported[script] = true
				findings = append(findings, scriptModeFinding{workflow: wf, script: script, target: target, mode: mode})
			}
		}
	}
	for script := range seen {
		tracked = append(tracked, script)
	}
	slices.Sort(tracked)
	return tracked, findings
}

// resolveIndexSymlink follows the symlink at script through the git index
// and returns the path it resolves to and that path's index mode: "" when
// the target is not tracked, symlinkMode when the chain is longer than
// maxSymlinkHops. modes caches index modes and gains every path looked up.
func resolveIndexSymlink(t *testing.T, root string, modes map[string]string, script string) (target, mode string) {
	t.Helper()
	target, mode = script, symlinkMode
	for range maxSymlinkHops {
		if mode != symlinkMode {
			return target, mode
		}
		link := gittest.Run(t, root, "cat-file", "-p", ":"+target)
		if filepath.IsAbs(link) {
			return link, ""
		}
		target = filepath.ToSlash(filepath.Clean(filepath.Join(filepath.Dir(target), link)))
		var ok bool
		if mode, ok = modes[target]; !ok {
			mode = indexMode(t, root, target)
			modes[target] = mode
		}
	}
	return target, mode
}

// indexMode returns the git index mode of path in the repository at root,
// or "" when path is not tracked.
func indexMode(t *testing.T, root, path string) string {
	t.Helper()
	out := gittest.Run(t, root, "ls-files", "--stage", "--", ":(literal)"+path)
	for line := range strings.SplitSeq(out, "\n") {
		meta, p, ok := strings.Cut(line, "\t")
		if ok && p == path {
			return strings.Fields(meta)[0]
		}
	}
	return ""
}

// String words a finding with the remedy that fits it: a chmod for a
// non-symlinked script, the target for a symlinked one (git refuses
// `update-index --chmod` on a symlink).
func (f scriptModeFinding) String() string {
	if f.target == "" {
		return fmt.Sprintf("%s references %s, whose git index mode is %s, not %s: a step that execs it directly dies with exit 126. Make it executable (git update-index --chmod=+x %s), or invoke it through its interpreter if it is genuinely interpreter-only",
			f.workflow, f.script, f.mode, executableMode, f.script)
	}
	switch f.mode {
	case "":
		return fmt.Sprintf("%s references %s, a symlink to %s, which is not tracked: a step that execs it dies with exit 127. Point the link at a tracked executable script",
			f.workflow, f.script, f.target)
	case symlinkMode:
		return fmt.Sprintf("%s references %s, a symlink chain still unresolved after %d hops at %s: shorten the chain or break the cycle",
			f.workflow, f.script, maxSymlinkHops, f.target)
	default:
		return fmt.Sprintf("%s references %s, a symlink to %s, whose git index mode is %s, not %s: a step that execs it directly dies with exit 126. Make the target executable (git update-index --chmod=+x %s), or invoke it through its interpreter if it is genuinely interpreter-only",
			f.workflow, f.script, f.target, f.mode, executableMode, f.target)
	}
}

// TestWorkflowScriptsAreExecutable pins every repository script a workflow
// references to index mode 100755. A step `run: scripts/x.sh` execs the file
// directly, and a 100644 script dies there with exit 126 before doing any
// work — the a11y-advisory workflow failed that way on every scheduled run.
// Scripts invoked through an interpreter (`bash x.sh`, `sh x.sh`) are held to
// the same rule: the bit costs them nothing, and one uniform invariant needs
// no shell parsing to enforce.
func TestWorkflowScriptsAreExecutable(t *testing.T) {
	root := filepath.Join("..", "..")
	workflows, err := filepath.Glob(filepath.Join(root, ".github", "workflows", "*.yml"))
	if err != nil {
		t.Fatalf("glob workflows: %v", err)
	}
	if len(workflows) == 0 {
		t.Fatalf("no workflow under %s — the guard would check nothing", filepath.Join(root, ".github", "workflows"))
	}

	tracked, findings := workflowScriptModes(t, root, workflows)

	// Blind-guard checks: a renamed directory or a regexp that stops matching
	// must fail here instead of passing on an empty set.
	const minTracked = 5
	if len(tracked) < minTracked {
		t.Fatalf("only %d tracked scripts referenced by the workflows (%v), expected at least %d — the reference scan has gone blind", len(tracked), tracked, minTracked)
	}
	if !slices.Contains(tracked, "scripts/a11y-advisory.sh") {
		t.Fatalf("scripts/a11y-advisory.sh is not among the referenced scripts %v — the scan no longer sees the reference this guard exists for", tracked)
	}

	for _, f := range findings {
		t.Error(f)
	}
}

// TestWorkflowScriptModesFlagsOnlyTrackedNonExecutableScripts is the scan's
// own witness, on a repository it builds: a non-executable tracked script is
// named, an executable one, an untracked one and a nested look-alike are not.
func TestWorkflowScriptModesFlagsOnlyTrackedNonExecutableScripts(t *testing.T) {
	root := t.TempDir()
	gittest.InitRepo(t, root)

	mustMkdir(t, filepath.Join(root, "scripts", "ci"))
	mustMkdir(t, filepath.Join(root, "ci"))
	mustMkdir(t, filepath.Join(root, "bots", "x", "scripts"))
	for _, p := range []string{"scripts/plain.sh", "scripts/ci/exec.sh", "ci/verify.sh", "scripts/nested.sh", "bots/x/scripts/nested.sh"} {
		mustWrite(t, filepath.Join(root, p), "#!/bin/sh\n", 0o644)
	}
	gittest.Run(t, root, "add", "scripts", "ci", "bots")
	// Set the index modes explicitly: what `git add` records from the file
	// mode depends on core.filemode, the index entry does not.
	gittest.Run(t, root, "update-index", "--chmod=-x", "scripts/plain.sh", "scripts/nested.sh")
	gittest.Run(t, root, "update-index", "--chmod=+x", "scripts/ci/exec.sh", "ci/verify.sh", "bots/x/scripts/nested.sh")

	wf := filepath.Join(root, "wf.yml")
	mustWrite(t, wf, strings.Join([]string{
		"steps:",
		"  - run: scripts/plain.sh",
		"  - run: ./scripts/plain.sh --again",
		"  - run: ./scripts/ci/exec.sh",
		"  - run: bash ci/verify.sh",
		"  - run: scripts/untracked.sh",
		"  - run: bots/x/scripts/nested.sh",
		"",
	}, "\n"), 0o644)

	tracked, findings := workflowScriptModes(t, root, []string{wf})

	if want := []string{"ci/verify.sh", "scripts/ci/exec.sh", "scripts/plain.sh"}; !slices.Equal(tracked, want) {
		t.Errorf("tracked references = %v, want %v", tracked, want)
	}
	want := []scriptModeFinding{{workflow: wf, script: "scripts/plain.sh", mode: "100644"}}
	if !slices.Equal(findings, want) {
		t.Errorf("findings = %+v, want %+v", findings, want)
	}
}

// cacheProbeRootEnv carries the repository root into the child process of
// TestWorkflowScriptModesMakesTheGitDirACacheInput.
const cacheProbeRootEnv = "ITERION_CIGUARD_EXEC_BIT_CACHE_PROBE_ROOT"

// TestWorkflowScriptModesCacheProbeHelper is the child process of
// TestWorkflowScriptModesMakesTheGitDirACacheInput; run directly it skips.
func TestWorkflowScriptModesCacheProbeHelper(t *testing.T) {
	root := os.Getenv(cacheProbeRootEnv)
	if root == "" {
		t.Skip("helper process only")
	}
	workflowScriptModes(t, root, nil)
}

// TestWorkflowScriptModesMakesTheGitDirACacheInput runs the scan in a child
// test binary with -test.testlogfile — the log `go test` keys its result
// cache on — inside a linked worktree, whose index lies outside any module
// root, and requires the log to record a chdir into the worktree's git dir.
func TestWorkflowScriptModesMakesTheGitDirACacheInput(t *testing.T) {
	primary := t.TempDir()
	gittest.InitRepo(t, primary)
	linked := filepath.Join(t.TempDir(), "linked")
	gittest.Run(t, primary, "worktree", "add", "-q", "-b", "probe", linked)
	gitDir := gittest.Run(t, linked, "rev-parse", "--path-format=absolute", "--git-dir")
	wantDir, err := filepath.EvalSymlinks(gitDir)
	if err != nil {
		t.Fatalf("resolve git dir %s: %v", gitDir, err)
	}

	logPath := filepath.Join(t.TempDir(), "testlog")
	cmd := exec.Command(os.Args[0], "-test.run=^TestWorkflowScriptModesCacheProbeHelper$", "-test.testlogfile="+logPath) // #nosec G204 -- the test binary itself
	cmd.Env = append(os.Environ(), cacheProbeRootEnv+"="+linked)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("helper process: %v\n%s", err, out)
	}

	var chdirs []string
	for line := range strings.SplitSeq(string(mustRead(t, logPath)), "\n") {
		dir, ok := strings.CutPrefix(line, "chdir ")
		if !ok {
			continue
		}
		chdirs = append(chdirs, dir)
		if real, err := filepath.EvalSymlinks(dir); err == nil && real == wantDir {
			return
		}
	}
	t.Fatalf("the test log records no chdir into the git dir %s (chdirs: %v): go test drops stat inputs outside the module root, so an index-only mode change in a linked worktree would replay a cached PASS", wantDir, chdirs)
}

// TestWorkflowScriptModesResolvesSymlinkedScripts pins the symlink case: a
// referenced script that is a symlink (index mode 120000) is judged by the
// index mode of what it points to, through every hop. A link to an
// executable script passes; a link to a 100644 script and a dangling link
// are reported against their target, which a chmod of the link cannot fix.
func TestWorkflowScriptModesResolvesSymlinkedScripts(t *testing.T) {
	root := t.TempDir()
	gittest.InitRepo(t, root)

	mustMkdir(t, filepath.Join(root, "scripts", "lib"))
	for _, p := range []string{"scripts/lib/real.sh", "scripts/plain.sh"} {
		mustWrite(t, filepath.Join(root, p), "#!/bin/sh\n", 0o644)
	}
	for link, target := range map[string]string{
		"scripts/alias.sh":    "lib/real.sh",
		"scripts/hop.sh":      "alias.sh",
		"scripts/to-plain.sh": "plain.sh",
		"scripts/dangling.sh": "lib/gone.sh",
	} {
		if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
			t.Fatalf("symlink %s -> %s: %v", link, target, err)
		}
	}
	gittest.Run(t, root, "add", "scripts")
	gittest.Run(t, root, "update-index", "--chmod=+x", "scripts/lib/real.sh")
	gittest.Run(t, root, "update-index", "--chmod=-x", "scripts/plain.sh")

	wf := filepath.Join(root, "wf.yml")
	mustWrite(t, wf, strings.Join([]string{
		"steps:",
		"  - run: scripts/alias.sh",
		"  - run: scripts/hop.sh",
		"  - run: scripts/to-plain.sh",
		"  - run: scripts/dangling.sh",
		"",
	}, "\n"), 0o644)

	tracked, findings := workflowScriptModes(t, root, []string{wf})

	if want := []string{"scripts/alias.sh", "scripts/dangling.sh", "scripts/hop.sh", "scripts/to-plain.sh"}; !slices.Equal(tracked, want) {
		t.Errorf("tracked references = %v, want %v", tracked, want)
	}
	want := []scriptModeFinding{
		{workflow: wf, script: "scripts/to-plain.sh", target: "scripts/plain.sh", mode: "100644"},
		{workflow: wf, script: "scripts/dangling.sh", target: "scripts/lib/gone.sh", mode: ""},
	}
	if !slices.Equal(findings, want) {
		t.Errorf("findings = %+v, want %+v", findings, want)
	}
}
