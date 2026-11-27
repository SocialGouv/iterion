package main

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

// A leg's `go test` lines are judged through the Go selection; everything
// else a job runs — a pnpm install and build in a working directory, a
// script, a node check over fixtures — reads files the import graph never
// sees. These tests read each job's steps and require the leg's paths to
// cover what the steps name, so a step input left out of `legs:` is a red
// guard rather than a leg that stays green on the change that breaks it.

// stepInput is one repository path a workflow step reads.
type stepInput struct {
	Path string // repo-relative
	Dir  bool   // the whole tracked subtree is read
	Why  string
}

// packageManagerWord matches a JavaScript package-manager or bundler
// invocation: it reads the workspace manifests and the lockfile at the root.
var packageManagerWord = regexp.MustCompile(`(^|[\s;&|(])(pnpm|npm|npx|yarn|corepack|vite)(\s|$)`)

// dirFlag matches a directory a command moves to: `cd X`, `pnpm -C X`.
var dirFlag = regexp.MustCompile(`(?:^|[\s;&|(])(?:cd|-C)\s+([^\s;&|)]+)`)

// varPrefix is a `$VAR/` or `${VAR}/` path prefix.
var varPrefix = regexp.MustCompile(`\$\{?[A-Za-z_][A-Za-z0-9_]*\}?/`)

// tokenSplit separates the words of a shell line or a script.
var tokenSplit = regexp.MustCompile("[\\s\"'`()=;|&<>,:\\[\\]{}]+")

// joinArgs matches a path.join / join / resolve call of a script.
var joinArgs = regexp.MustCompile(`(?:join|resolve)\(([^)]*)\)`)

// quoted matches a single- or double-quoted string literal.
var quoted = regexp.MustCompile(`'([^']*)'|"([^"]*)"`)

// jsImport matches the specifier of a JavaScript import or require.
var jsImport = regexp.MustCompile(`(?:from\s+|import\s*\(\s*|require\s*\(\s*)['"]([^'"]+)['"]`)

// shellRun matches a script a shell line runs: at the start of the line or
// of a command substitution, after an optional interpreter.
var shellRun = regexp.MustCompile(`(?m)(?:^\s*|\$\(\s*)(?:exec\s+|bash\s+|sh\s+|source\s+|\.\s+|python3?\s+|node\s+)?"?((?:\$\{?[A-Za-z_][A-Za-z0-9_]*\}?/|\$\(dirname "\$0"\)"?/)?[A-Za-z0-9_./-]+\.(?:sh|bash|py|mjs|js|cjs))`)

// commandWords returns the text of the lines that can read a path: comment
// lines (#, //, /*, *) and the lines that only print or delete (echo,
// printf, rm) are dropped, and the destination of a cp or mv.
func commandWords(text string) string {
	var b strings.Builder
	for _, line := range strings.Split(text, "\n") {
		tl := strings.TrimSpace(line)
		if tl == "" || strings.HasPrefix(tl, "#") || strings.HasPrefix(tl, "//") || strings.HasPrefix(tl, "/*") || strings.HasPrefix(tl, "*") {
			continue
		}
		// The command word follows sudo and NAME=value assignments; a line
		// of assignments alone (`X="$(script.sh)"`) is kept whole.
		f := strings.Fields(tl)
		for len(f) > 0 && (f[0] == "sudo" || (strings.Contains(f[0], "=") && !strings.HasPrefix(f[0], "="))) {
			f = f[1:]
		}
		if len(f) > 0 {
			switch f[0] {
			case "echo", "printf", "rm":
				continue
			case "cp", "mv":
				tl = strings.Join(f[:len(f)-1], " ")
			}
		}
		b.WriteString(tl)
		b.WriteByte('\n')
	}
	return b.String()
}

// jsExts are the scripts whose paths are read from join, resolve and fs
// calls rather than from every word.
var jsExts = map[string]bool{".mjs": true, ".js": true, ".cjs": true, ".ts": true}

// fsCall matches a literal path handed to a node:fs read.
var fsCall = regexp.MustCompile(`(?:readFileSync|readdirSync|readFile|readdir|existsSync|statSync|createReadStream|opendirSync)\(\s*['"]([^'"]+)['"]`)

// scriptExts are the files a step or a script runs whose text names further
// inputs.
var scriptExts = map[string]bool{".sh": true, ".bash": true, ".mjs": true, ".js": true, ".cjs": true, ".py": true, ".ts": true}

// treeIndex answers what the tracked tree holds.
type treeIndex struct {
	tracked []string // sorted
}

func (ix treeIndex) file(p string) bool { return isTracked(ix.tracked, p) }
func (ix treeIndex) dir(p string) bool  { return len(filesUnder(ix.tracked, p)) > 0 }

// resolve returns the tracked path tok names from one of bases, and whether
// it is a directory.
func (ix treeIndex) resolve(tok string, bases []string) (string, bool, bool) {
	for _, b := range bases {
		p := path.Clean(path.Join(b, tok))
		if p == "." || p == ".." || strings.HasPrefix(p, "../") {
			continue
		}
		if ix.file(p) {
			return p, false, true
		}
		if ix.dir(p) {
			return p, true, true
		}
	}
	return "", false, false
}

// pathTokens returns the words of text that may name a path: they hold a
// slash or a dot. A `$VAR/` prefix is dropped when the rest still holds a
// slash (`$ROOT/scripts/x.sh`), the word skipped otherwise (`$DIR/x.json`
// names a file of an unknown directory).
func pathTokens(text string) []string {
	var out []string
	for _, w := range tokenSplit.Split(text, -1) {
		if loc := varPrefix.FindAllStringIndex(w, -1); len(loc) > 0 {
			w = w[loc[len(loc)-1][1]:]
			if !strings.Contains(w, "/") {
				continue
			}
		}
		w = strings.TrimSuffix(strings.TrimSuffix(w, "/..."), "/")
		if strings.HasSuffix(w, ".") && !strings.HasSuffix(w, "..") {
			w = strings.TrimSuffix(w, ".") // a sentence's full stop
		}
		// `$(cd "$(dirname "$0")/.." && pwd)/bots/x.pin` splits to
		// `/bots/x.pin`: the slash is the join, not the filesystem root
		// (an absolute path like /usr/bin/env names no tracked file).
		w = strings.TrimLeft(strings.TrimPrefix(w, "./"), "/")
		if w == "" || strings.HasPrefix(w, "-") || strings.ContainsAny(w, "$*?#%") {
			continue
		}
		if strings.Contains(w, "/") || strings.Contains(w, ".") {
			out = append(out, w)
		}
	}
	return out
}

// stepInputs returns what one step reads: its working directory and the
// directories it moves to, the root manifests when it runs a package
// manager, every tracked path its commands name, and — for each script
// among those, and each script those run or import in turn — the paths the
// script's code names (relative to the root or to the script). Comments,
// printed messages and deleted or written paths are not reads.
func stepInputs(ix treeIndex, read func(string) (string, error), wd, run string) ([]stepInput, error) {
	var out []stepInput
	run = commandWords(run)
	bases := []string{"."}
	if wd != "" {
		out = append(out, stepInput{Path: path.Clean(wd), Dir: true, Why: "working-directory " + wd})
		bases = append(bases, path.Clean(wd))
	}
	for _, m := range dirFlag.FindAllStringSubmatch(run, -1) {
		for _, b := range bases {
			if p, isDir, ok := ix.resolve(m[1], []string{b}); ok && isDir {
				out = append(out, stepInput{Path: p, Dir: true, Why: "moves to " + m[1]})
				bases = append(bases, p)
				break
			}
		}
	}
	if packageManagerWord.MatchString(run) {
		for _, f := range []string{"package.json", "pnpm-lock.yaml", "pnpm-workspace.yaml"} {
			if ix.file(f) {
				out = append(out, stepInput{Path: f, Why: "runs a package manager"})
			}
		}
	}
	var scripts []string
	for _, tok := range pathTokens(run) {
		if p, isDir, ok := ix.resolve(tok, bases); ok {
			out = append(out, stepInput{Path: p, Dir: isDir, Why: "named in the step"})
			if !isDir && scriptExts[path.Ext(p)] {
				scripts = append(scripts, p)
			}
		}
	}
	seen := map[string]bool{}
	for len(scripts) > 0 {
		s := scripts[0]
		scripts = scripts[1:]
		if seen[s] {
			continue
		}
		seen[s] = true
		text, err := read(s)
		if err != nil {
			return nil, err
		}
		var runs []string
		for _, m := range jsImport.FindAllStringSubmatch(text, -1) {
			runs = append(runs, m[1])
		}
		text = commandWords(text)
		for _, m := range shellRun.FindAllStringSubmatch(text, -1) {
			runs = append(runs, m[1])
		}
		for _, tok := range runs {
			tok = varPrefix.ReplaceAllString(strings.Replace(tok, `$(dirname "$0")"/`, "", 1), "")
			tok = strings.Replace(tok, `$(dirname "$0")/`, "", 1)
			if p, isDir, ok := ix.resolve(tok, []string{path.Dir(s), "."}); ok && !isDir && p != s {
				out = append(out, stepInput{Path: p, Why: "run by " + s})
				scripts = append(scripts, p)
			}
		}
		// A shell or Python script names its files as words; a JavaScript
		// one through a join, a resolve or an fs call — its other string
		// literals are data (a test's fixture names, a URL under test).
		var words []string
		if jsExts[path.Ext(s)] {
			for _, m := range fsCall.FindAllStringSubmatch(text, -1) {
				words = append(words, m[1])
			}
		} else {
			words = pathTokens(text)
		}
		for _, m := range joinArgs.FindAllStringSubmatch(text, -1) {
			var parts []string
			for _, q := range quoted.FindAllStringSubmatch(m[1], -1) {
				parts = append(parts, q[1]+q[2])
			}
			if len(parts) > 0 {
				words = append(words, path.Join(parts...))
			}
		}
		for _, tok := range words {
			p, isDir, ok := ix.resolve(tok, []string{".", path.Dir(s)})
			if !ok || p == s {
				continue
			}
			out = append(out, stepInput{Path: p, Dir: isDir, Why: "named in " + s})
		}
	}
	return out, nil
}

// uncoveredStepInputs returns the inputs a leg's declaration leaves out. A
// file is covered by a leg path or a global trigger. A directory that is a
// Go package the leg tests (`packages`) or builds (`reach`) is covered by
// the Go selection; any other directory needs every tracked file under it
// covered.
func uncoveredStepInputs(in *inputs, ix treeIndex, goDirs map[string]bool, l leg, ins []stepInput) []string {
	covered := func(p string) bool {
		for _, r := range in.Global {
			if matchGlob(r.Pattern, p) {
				return true
			}
		}
		return firstMatch(l.Paths, p) != ""
	}
	var out []string
	for _, si := range ins {
		if !si.Dir {
			if !covered(si.Path) {
				out = append(out, fmt.Sprintf("%s (%s)", si.Path, si.Why))
			}
			continue
		}
		if goDirs[si.Path] && (slices.Contains(l.Reach, si.Path) || slices.ContainsFunc(l.Packages, func(spec string) bool { return dirMatches(spec, si.Path) })) {
			continue
		}
		var missing []string
		for _, t := range filesUnder(ix.tracked, si.Path) {
			if !covered(t) {
				missing = append(missing, t)
			}
		}
		if len(missing) > 0 {
			out = append(out, fmt.Sprintf("%s/ (%s): %d tracked file(s) no leg path covers (first: %s)", si.Path, si.Why, len(missing), missing[0]))
		}
	}
	return sortedUnique(out)
}

// TestEachLegCoversItsStepsInputs holds every leg's paths over what its
// job's steps read outside the Go selection.
func TestEachLegCoversItsStepsInputs(t *testing.T) {
	root := repoRoot(t)
	in := realInputs(t, root)
	w := readWorkflow(t, root)
	_, tracked := realScan(t, root)
	ix := treeIndex{tracked: tracked}
	dirs, err := packageDirs(root)
	if err != nil {
		t.Fatal(err)
	}
	goDirs := map[string]bool{}
	for _, d := range dirs {
		goDirs[d] = true
	}
	read := func(p string) (string, error) {
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(p)))
		return string(b), err
	}
	steps := 0
	for _, name := range sortedKeys(w.Jobs) {
		job := w.Jobs[name]
		var ins []stepInput
		for _, s := range job.Steps {
			if s.Run == "" {
				continue
			}
			steps++
			wd := s.WorkingDirectory
			if wd == "" {
				wd = job.Defaults.Run.WorkingDirectory
			}
			got, err := stepInputs(ix, read, wd, s.Run)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			ins = append(ins, got...)
		}
		for _, u := range uncoveredStepInputs(in, ix, goDirs, in.Legs[name], ins) {
			t.Errorf("leg %s: a step reads %s", name, u)
		}
	}
	if steps < 30 {
		t.Fatalf("read %d run steps from tests.yml; the workflow holds many more — is the parse still right?", steps)
	}
	if t.Failed() {
		t.Log("add the inputs to the leg's `paths:` in " + inputsFile)
	}
}

// TestTheStepGuardSeesEachInputKind proves each kind of step input can
// redden the guard: a working directory, a package manager's root
// manifests, a script the step names, a fixture the script reads through a
// join, and a Go package directory the leg neither tests nor builds.
func TestTheStepGuardSeesEachInputKind(t *testing.T) {
	tracked := []string{
		"package.json", "pnpm-lock.yaml", "pnpm-workspace.yaml",
		"web/package.json", "web/src/main.ts",
		"tools/check.mjs", "tools/lib.sh",
		"fixtures/anchors/a.tsv",
		"cmd/tool/main.go",
		"docs/x.md",
	}
	sort.Strings(tracked)
	ix := treeIndex{tracked: tracked}
	files := map[string]string{
		"tools/check.mjs": "const root = join(here, '..')\nconst fx = join(root, 'fixtures', 'anchors')\nimport { x } from './lib.sh'\n",
		"tools/lib.sh":    "cat \"$ROOT/docs/x.md\"\n",
	}
	read := func(p string) (string, error) {
		if s, ok := files[p]; ok {
			return s, nil
		}
		return "", fmt.Errorf("no file %s", p)
	}
	in := &inputs{Global: []rule{{Pattern: "go.mod", Why: "module"}}}
	goDirs := map[string]bool{"cmd/tool": true}
	for _, tc := range []struct {
		name, wd, run string
		want          string
	}{
		{"working directory", "web", "make", "web/ (working-directory web)"},
		{"package manager", "", "pnpm install --frozen-lockfile", "package.json (runs a package manager)"},
		{"moved-to directory", "", "pnpm -C web exec tsc", "web/ (moves to web)"},
		{"script", "", "node tools/check.mjs", "tools/check.mjs (named in the step)"},
		{"fixture a script joins", "", "node tools/check.mjs", "fixtures/anchors/ (named in tools/check.mjs)"},
		{"a script a script names", "", "node tools/check.mjs", "docs/x.md (named in tools/lib.sh)"},
		{"go package outside the leg", "", "go run ./cmd/tool", "cmd/tool/ (named in the step)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ins, err := stepInputs(ix, read, tc.wd, tc.run)
			if err != nil {
				t.Fatal(err)
			}
			got := uncoveredStepInputs(in, ix, goDirs, leg{}, ins)
			if !slices.ContainsFunc(got, func(s string) bool { return strings.HasPrefix(s, tc.want) }) {
				t.Errorf("uncovered = %q, want one starting %q", got, tc.want)
			}
			full := leg{Paths: []string{"**"}, Reach: []string{"cmd/tool"}}
			if got := uncoveredStepInputs(in, ix, goDirs, full, ins); len(got) > 0 {
				t.Errorf("a leg covering everything left %q uncovered", got)
			}
		})
	}
}
