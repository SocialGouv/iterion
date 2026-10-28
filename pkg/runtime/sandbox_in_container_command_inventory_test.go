package runtime

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/internal/proc"
)

// The table in sandbox_tool_env_class_test.go claims to name every program
// iterion starts INSIDE a container. This file is what makes the claim
// checkable: the inventory is read from the code with go/ast, over every
// non-test .go file of every package that holds the exec seam, and a command
// it finds that neither the table nor a reviewed line accounts for fails.
//
// It replaces a regexp over five hand-named files, which saw exactly `sh` and
// `tar` from one of them — so 28 of its 30 reviewed lines had no subject and
// four arrival shapes passed green: a command in an unscanned file of the
// same package, a word added to an `sh -c` script, a multi-line []string
// literal, and the generated post-create prologue (mutants mA-mD of #2358's
// round-7 review). An inventory that cannot see an arrival is scaffolding: it
// makes the table look guarded while the hole the table exists for stays
// open.
//
// What it reads: an argv is a []string literal handed to the seam (nested in
// the call, or appended to), a script is the word after a SHELL's -c when its
// text is in the tree, and a script built at run time is RENDERED here by
// calling its generator. A script whose text comes from somewhere else — a
// parameter, a caller's variable, another module — is a frontier, and every
// frontier is named with where its text comes from.
//
// What it cannot read, named rather than hidden, because an inventory that
// hides its edges is the scaffolding this file replaces:
//
//   - a package that drives a sandbox through an interface it declares
//     itself, naming neither sandbox.Run nor sandbox.ExecOpts: the derived
//     set would not hold it, which is why the floor above pins the packages
//     that must be in it;
//   - an argv whose head is a variable (`[]string{bin, "-c", s}`): there is
//     no name to review;
//   - a command inside a QUOTED span of a script (`sh -c "curl …"` within a
//     script): the quoted text is dropped, and the shell that would run it
//     is itself a row of the table;
//   - the program of a `find -exec` or an `xargs`: it comes from the argv,
//     which is iterion's own text, and both are reviewed as such.
//
// Each of those is an over-approximation away from safety, never towards it:
// the shapes it DOES read are the four that mutants mA-mD proved invisible.

// seamNames finds the packages that drive a sandbox: a package that starts a
// command in the container does it through sandbox.Run / sandbox.ExecOpts, or
// is the package that declares them.
var seamNames = regexp.MustCompile(`sandbox\.(Run|ExecOpts)\b|(?m)^type Run interface`)

// commandWord is the shape of a program name on an argv or in a script. A
// word that is not one (a flag, a NAME=value, a path, a placeholder, a
// variable) names no program to review.
var commandWord = regexp.MustCompile(`^[a-z][a-z0-9_.+-]*$`)

// execSelectors are the methods of the exec seam: the argv of a call to one
// of them is a command line that runs in the container.
var execSelectors = map[string]bool{"Exec": true, "Command": true}

// shellPrograms are the programs whose -c argument is a script rather than a
// setting or a file: `git -c core.hooksPath=…` is not a script, and reading
// it as one both misses real scripts and invents frontiers.
var shellPrograms = map[string]bool{"sh": true, "bash": true, "dash": true, "ash": true, "zsh": true}

// scriptFlags are the shell flags whose next argv word is a script.
var scriptFlags = map[string]bool{"-c": true, "-lc": true, "-ec": true, "-ce": true}

// renderedInContainerScripts are the scripts iterion BUILDS at run time and
// runs in the container through a slot no scan can follow. Reading their
// generator's source would read its error messages too, so they are rendered
// — the real text, tokenised like any script. A generator that leaves the
// tree stops compiling here, which is the point.
func renderedInContainerScripts(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{
		// Its text becomes spec.PostCreate, which the driver runs as
		// `sh -c` in the container (sandbox.RunPostCreate).
		"devboxInstallSnippet": devboxInstallSnippet([]devboxProject{{
			label: "repo", dir: filepath.Join(t.TempDir(), "repo"), hostConfig: filepath.Join(t.TempDir(), "devbox.json"),
		}}),
	}
	// The node-session sweep, run as `sh -c` by the completion sweeper and
	// by the sandboxed delegate's kill path.
	sweep, err := proc.NodeSessionSweepScript("iterion-node-token", filepath.Join(t.TempDir(), "since"))
	if err != nil {
		t.Fatalf("render the node-session sweep script: %v", err)
	}
	out["proc.NodeSessionSweepScript"] = sweep
	return out
}

// unfollowableScriptSlot explains, per site, a shell `-c` whose script the
// scan cannot read: the text comes from somewhere else. A site that is not
// listed fails the test — a new road for a script into the container is
// exactly the arrival this inventory exists to see — and a listed site that
// is no longer one fails too.
var unfollowableScriptSlot = map[string]string{
	"pkg/sandbox/exec_helpers.go:RunPostCreate":                                     "the post-create prologue: iterion's own half is rendered above (devboxInstallSnippet), the rest is the repository's postCreateCommand and the workflow's sandbox.post_create — operator-authored text, which the deny class and the workflow's own env decide",
	"pkg/sandbox/completion.go:Wrap":                                                "the completion wrapper around a node's own argv; the sweep half is proc.NodeSessionSweepScript, rendered above",
	"pkg/runtime/child_sandbox_resources.go:provisionSharedChildDevbox":             "the shared-resource scripts this function itself spells, read from its body, plus devboxInstallSnippet, rendered above",
	"pkg/backend/delegate/claude_code_sandbox_kill.go:wrapSandboxDelegateArgv":      "the kill-and-report script this file concatenates from its own literals, read from the function's body, plus proc.NodeSessionSweepScript, rendered above",
	"pkg/backend/delegate/claude_code_routing.go:claudeSandboxArgv":                 "the delegate CLI's own command line, built from the node's resolved argv",
	"pkg/runtime/child_sandbox_resources.go:snapshotSharedChildResources":           "the snapshot script this function spells as a multi-line literal and hands to its own closure, read from its body",
	"pkg/sandbox/completion.go:sweepOnce":                                           "proc.NodeSessionSweepScript, rendered above",
	"pkg/backend/delegate/claude_code_sandbox_kill.go:killSandboxDelegateReporting": "the kill-and-report script this function concatenates from its own literals, read from its body, plus proc.NodeSessionSweepScript, rendered above",
	"pkg/backend/model/executor_tool.go:toolNodeCommand":                            "a tool node's `command:`, which is operator-authored text; its interpreter is the table's bash row",
}

// inContainerCommand is a program the inventory found, with the first site
// that starts it.
type inContainerCommand struct {
	name string
	site string
	how  string // argv, script, or rendered
}

// TestTheInContainerCommandInventoryIsReadFromTheCode walks the exec seam and
// fails on a command that neither the table nor the reviewed list accounts
// for. The reviewed list is kept honest in both directions: an entry the walk
// never meets is dropped rather than left to suggest a guard with no subject.
func TestTheInContainerCommandInventoryIsReadFromTheCode(t *testing.T) {
	root := repoRootForInventory(t)
	pkgs := seamPackages(t, root)
	// The packages that must be in the derived set: a rename or a prune that
	// hides one would shrink the inventory in silence, which is the failure
	// mode this whole file is about.
	for _, want := range []string{
		filepath.Join("pkg", "runtime"),
		filepath.Join("pkg", "runner"),
		filepath.Join("pkg", "sandbox"),
		filepath.Join("pkg", "sandbox", "kubernetes"),
		filepath.Join("pkg", "backend", "delegate"),
		filepath.Join("pkg", "backend", "model"),
	} {
		if !slicesContains(pkgs, want) {
			t.Fatalf("%s is not in the packages derived from the exec seam (%v): the derivation lost a package that starts commands in the container, so the inventory is smaller than the tree", want, pkgs)
		}
	}

	found, slots := walkSeam(t, root, pkgs)
	for name, script := range renderedInContainerScripts(t) {
		if strings.TrimSpace(script) == "" {
			t.Errorf("%s rendered an empty script: its words left the inventory with it", name)
		}
		for _, w := range commandPositionWords(script) {
			found = append(found, inContainerCommand{name: w, site: name, how: "rendered"})
		}
	}

	// Every unfollowable shell `-c` must be declared, and every declaration
	// must still be a site.
	for site := range slots {
		if unfollowableScriptSlot[site] == "" {
			t.Errorf("%s runs a shell -c whose script the scan cannot read: say where the script comes from (unfollowableScriptSlot), and render its generator above if it is iterion's own text", site)
		}
	}
	for site := range unfollowableScriptSlot {
		if !slots[site] {
			t.Errorf("unfollowableScriptSlot declares %s, which is no longer a shell -c site of the seam: drop the entry rather than keep a reason with no subject", site)
		}
	}

	seen := map[string]inContainerCommand{}
	for _, c := range found {
		if _, ok := seen[c.name]; !ok {
			seen[c.name] = c
		}
	}
	// A command the table names but the tree no longer starts: the row is a
	// claim about THIS tree, so it must still have a site.
	for _, want := range inContainerCommandsWithASiteHere {
		if _, ok := seen[want]; !ok {
			t.Errorf("the table claims iterion runs %q in the container, but no site of the exec seam starts it any more: re-read the table against the tree", want)
		}
	}
	if len(seen) < inventoryFloor {
		t.Fatalf("the inventory found %d commands over %d packages, and the tree has never had fewer than %d: the walk stopped seeing the code, so a green verdict here proves nothing", len(seen), len(pkgs), inventoryFloor)
	}

	var unreviewed []string
	for name, c := range seen {
		if reviewedInContainerCommands[name] == "" {
			unreviewed = append(unreviewed, name+" ("+c.how+" at "+c.site+")")
		}
	}
	sort.Strings(unreviewed)
	if len(unreviewed) > 0 {
		t.Errorf("the exec seam starts these in the container, and neither the table in sandbox_tool_env_class_test.go nor the reviewed list in this file mentions them: say what each reads from the environment, then deny that name in devcontainer.DeniedEnvKey or write down why the program cannot be reached (#2316, #2358)\n  %s",
			strings.Join(unreviewed, "\n  "))
	}
	var withoutASite []string
	for name := range reviewedInContainerCommands {
		if _, ok := seen[name]; !ok {
			withoutASite = append(withoutASite, name)
		}
	}
	sort.Strings(withoutASite)
	if len(withoutASite) > 0 {
		t.Errorf("the reviewed list carries %s, which the walk never meets: a reviewed line with no site makes the list look like a guard over something that is not there — drop it, or say in the table where the command is started from",
			strings.Join(withoutASite, ", "))
	}
}

// inventoryFloor is the order of magnitude the walk has always found (52 at
// the time of writing, over 8 packages). It turns a walk that stopped
// matching the code — a renamed seam, a parser that reads nothing — into a
// failure instead of a green run over an empty set, with enough margin that
// deleting a command does not ask for an edit here.
const inventoryFloor = 40

// inContainerCommandsWithASiteHere are the table's commands that the exec
// seam itself starts, so the walk must still find them. The table's other
// commands (npx, node, go, python3, kubectl, docker) are started by an
// operator-authored node's command or by a CLI a backend spawns, where the
// argv is not iterion's own text.
var inContainerCommandsWithASiteHere = []string{"sh", "tar", "git", "devbox", "iterion", "bash"}

// reviewedInContainerCommands carries, for every command the walk finds that
// the table does not name, why it needs no deny-class row: a program that
// takes no options, no helper program and no configuration file from the
// environment cannot be steered by a reviewed repository's containerEnv.
// Shell keywords and builtins are named as such — they are not programs, and
// the shell that runs them is itself a row of the table.
var reviewedInContainerCommands = map[string]string{
	// In the table.
	"sh":      "in the table (ENV, BASH_ENV, PATH)",
	"bash":    "in the table (BASH_FUNC_x%%, SHELLOPTS)",
	"tar":     "in the table (TAR_OPTIONS)",
	"git":     "in the table (GIT_SSH_COMMAND, GIT_CONFIG_GLOBAL, EDITOR)",
	"devbox":  "its nix is in the table (NIX_CONFIG); DEVBOX_NO_PROMPT is set by the prologue itself",
	"iterion": "in the table (the claw runner)",
	// Shell keywords and builtins: the program is the shell, and it is in
	// the table.
	"if":       "a shell keyword",
	"then":     "a shell keyword",
	"else":     "a shell keyword",
	"elif":     "a shell keyword",
	"fi":       "a shell keyword",
	"for":      "a shell keyword",
	"while":    "a shell keyword",
	"do":       "a shell keyword",
	"done":     "a shell keyword",
	"case":     "a shell keyword",
	"esac":     "a shell keyword",
	"set":      "a shell builtin",
	"export":   "a shell builtin",
	"exec":     "a shell builtin: what it execs is a word of the same script, already in this inventory",
	"exit":     "a shell builtin",
	"echo":     "a shell builtin",
	"printf":   "a shell builtin",
	"test":     "a shell builtin",
	"read":     "a shell builtin",
	"trap":     "a shell builtin: what it runs is a word of the same script",
	"eval":     "a shell builtin: what it evaluates is a word of the same script",
	"kill":     "a shell builtin, and a signal carries no program",
	"umask":    "a shell builtin",
	"break":    "a shell builtin",
	"continue": "a shell builtin",
	"return":   "a shell builtin",
	"true":     "a no-op probe",
	// Programs reviewed as reading nothing from the environment that names
	// options, a helper program or a configuration file.
	"rm":       "coreutils reads no env for options",
	"rmdir":    "coreutils reads no env for options",
	"mkdir":    "coreutils reads no env for options",
	"cp":       "coreutils reads no env for options",
	"mv":       "coreutils reads no env for options",
	"ln":       "coreutils reads no env for options",
	"cat":      "coreutils reads no env for options",
	"chmod":    "coreutils reads no env for options",
	"sleep":    "coreutils reads no env for options",
	"readlink": "coreutils reads no env for options",
	"cmp":      "reads no env for options; the devbox prologue uses it to compare a lock with its pre-install copy",
	"command":  "a shell builtin (`command -v`)",
	"setsid":   "reads no env for options; it only detaches the program named on its own argv",
	"find":     "reads no env for options, and its -exec program comes from the argv, which is iterion's own text",
	"sed":      "reads no env for options (POSIXLY_CORRECT only changes argument parsing)",
	"tr":       "coreutils reads no env for options",
	"grep":     "GREP_OPTIONS was removed in GNU grep 2.21, and GREP_COLORS names no program",
	"wc":       "coreutils reads no env for options",
	"date":     "coreutils reads no env that names a program",
}

// walkSeam parses every non-test .go file of pkgs and returns the commands it
// found with the shell -c sites whose script it could not read.
func walkSeam(t *testing.T, root string, pkgs []string) (found []inContainerCommand, slots map[string]bool) {
	t.Helper()
	slots = map[string]bool{}
	type pkgSource struct {
		fset  *token.FileSet
		files []*ast.File
		// decls maps a package-level name to the expression its
		// declaration spells, so a script held in a const is read whole —
		// including one concatenated from other constants.
		decls map[string]ast.Expr
	}
	sources := map[string]*pkgSource{}
	for _, pkg := range pkgs {
		src := &pkgSource{fset: token.NewFileSet(), decls: map[string]ast.Expr{}}
		for _, path := range nonTestGoFiles(t, filepath.Join(root, pkg)) {
			f, err := parser.ParseFile(src.fset, path, nil, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			src.files = append(src.files, f)
			for _, decl := range f.Decls {
				gen, ok := decl.(*ast.GenDecl)
				if !ok {
					continue
				}
				for _, spec := range gen.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for i, name := range vs.Names {
						if i < len(vs.Values) {
							src.decls[name.Name] = vs.Values[i]
						}
					}
				}
			}
		}
		sources[pkg] = src
	}

	for _, pkg := range pkgs {
		src := sources[pkg]
		for _, f := range src.files {
			rel := filepath.Join(pkg, filepath.Base(src.fset.Position(f.Pos()).Filename))
			// argvLiteral reads one command line: its head, and the script
			// a shell is handed.
			argvLiteral := func(x ast.Node) {
				elts, ok := stringSliceLiteral(x)
				if !ok {
					return
				}
				head, ok := stringLiteral(elts[0])
				if !ok || !commandWord.MatchString(head) {
					return
				}
				site := rel + ":" + strconv.Itoa(src.fset.Position(x.Pos()).Line)
				found = append(found, inContainerCommand{name: head, site: site, how: "argv"})
				if !shellPrograms[head] {
					return
				}
				for i, elt := range elts {
					flag, ok := stringLiteral(elt)
					if !ok || !scriptFlags[flag] || i+1 >= len(elts) {
						continue
					}
					// The script, read whole: a literal, a constant, or a
					// concatenation of both. Reading the pieces apart would
					// put a command position in the middle of a line.
					text := scriptText(elts[i+1], src.decls, map[string]bool{})
					if strings.TrimSpace(text) == "" {
						// The script's text is not in this package's
						// declarations: a parameter, a caller's variable,
						// another module. Name the site by its enclosing
						// function so the declaration survives an edit
						// above it, and read that function's own scripts.
						fn := enclosingFuncName(f, x.Pos())
						slots[rel+":"+fn] = true
						for _, piece := range functionScripts(f, fn) {
							for _, w := range commandPositionWords(piece) {
								found = append(found, inContainerCommand{name: w, site: rel + ":" + fn, how: "script"})
							}
						}
						continue
					}
					for _, w := range commandPositionWords(text) {
						found = append(found, inContainerCommand{name: w, site: site, how: "script"})
					}
				}
			}
			ast.Inspect(f, func(n ast.Node) bool {
				// A command line handed to the seam, however it is wrapped
				// on the way (`wrap([]string{…})`, `append([]string{…}, …)`).
				if call, ok := n.(*ast.CallExpr); ok && execSelectors[calleeName(call)] {
					for _, arg := range call.Args {
						ast.Inspect(arg, func(x ast.Node) bool {
							argvLiteral(x)
							return true
						})
					}
					return true
				}
				// A shell command line built anywhere in a package that
				// drives sandboxes: a helper that wraps an argv, a tool
				// node's command. Its head is a shell and it carries a
				// script flag, so it is a script run somewhere — the
				// cheapest honest over-approximation, and the shape the
				// previous regexp inventory could not see.
				if elts, ok := stringSliceLiteral(n); ok {
					if head, ok := stringLiteral(elts[0]); ok && shellPrograms[head] && hasScriptFlag(elts) {
						argvLiteral(n)
					}
				}
				return true
			})
		}
	}
	return found, slots
}

// hasScriptFlag reports whether an argv carries a shell's script flag.
func hasScriptFlag(elts []ast.Expr) bool {
	for _, elt := range elts {
		if flag, ok := stringLiteral(elt); ok && scriptFlags[flag] {
			return true
		}
	}
	return false
}

// functionScripts returns the multi-line string literals a function spells:
// where the scan cannot follow the script slot, the scripts the function
// itself writes are the candidates — a closure's argument, a piece
// concatenated into the script. Prose is single-line in this tree; a script
// is not.
func functionScripts(f *ast.File, name string) []string {
	var out []string
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != name || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if s, ok := stringLiteral(n); ok && strings.Contains(s, "\n") {
				out = append(out, s)
			}
			return true
		})
	}
	return out
}

// calleeName is the name a call resolves to by its syntax: the selector of a
// method call, or the identifier of a plain one.
func calleeName(call *ast.CallExpr) string {
	switch fun := call.Fun.(type) {
	case *ast.SelectorExpr:
		return fun.Sel.Name
	case *ast.Ident:
		return fun.Name
	}
	return ""
}

// scriptText is the text an expression spells: a literal, a constant of this
// package, or a concatenation of both, in order. An operand it cannot read
// (a call, a parameter, a field) contributes nothing, so a script built
// around one is still read for the parts that are in the tree.
func scriptText(e ast.Expr, decls map[string]ast.Expr, seen map[string]bool) string {
	switch v := e.(type) {
	case *ast.BasicLit:
		if s, ok := stringLiteral(v); ok {
			return s
		}
	case *ast.BinaryExpr:
		if v.Op == token.ADD {
			return scriptText(v.X, decls, seen) + scriptText(v.Y, decls, seen)
		}
	case *ast.ParenExpr:
		return scriptText(v.X, decls, seen)
	case *ast.Ident:
		if decl, ok := decls[v.Name]; ok && !seen[v.Name] {
			seen[v.Name] = true
			return scriptText(decl, decls, seen)
		}
	}
	return ""
}

// stringSliceLiteral reports whether n is a non-empty []string{...} literal,
// however it is laid out: a multi-line literal is the shape that passed the
// regexp this walk replaces.
func stringSliceLiteral(n ast.Node) ([]ast.Expr, bool) {
	lit, ok := n.(*ast.CompositeLit)
	if !ok || len(lit.Elts) == 0 {
		return nil, false
	}
	arr, ok := lit.Type.(*ast.ArrayType)
	if !ok || arr.Len != nil {
		return nil, false
	}
	id, ok := arr.Elt.(*ast.Ident)
	if !ok || id.Name != "string" {
		return nil, false
	}
	return lit.Elts, true
}

func stringLiteral(n ast.Node) (string, bool) {
	lit, ok := n.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return s, true
}

// enclosingFuncName names the function a position falls in, so a frontier
// declaration survives the lines above it moving.
func enclosingFuncName(f *ast.File, pos token.Pos) string {
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || pos < fn.Pos() || pos > fn.End() {
			continue
		}
		return fn.Name.Name
	}
	return "<file scope>"
}

// commandPositionWords tokenises a shell script and returns the words in
// command position: the first word of the script, and the first word after
// every separator. An option, a NAME=value prefix, a path, a variable and a
// comment are not command positions.
func commandPositionWords(script string) []string {
	// A call to a function the script itself defines runs no program.
	defined := map[string]bool{}
	for _, m := range shellFuncDef.FindAllStringSubmatch(script, -1) {
		defined[m[1]] = true
	}
	var out []string
	for _, line := range strings.Split(script, "\n") {
		line = unquoted(line)
		atCommand := true
		for _, tok := range strings.Fields(line) {
			if strings.HasPrefix(tok, "#") {
				break // a comment runs to the end of the line
			}
			trimmed := strings.TrimRight(tok, ";&|")
			next := strings.HasSuffix(tok, ";") || strings.HasSuffix(tok, "|") || strings.HasSuffix(tok, "&")
			switch trimmed {
			case "", "{", "}", "(", ")", "!", "||", "&&", "|", ";", "then", "do", "else", "elif":
				// A separator, a grouping token or a keyword that
				// introduces a command: the next word is a command.
				if trimmed != "" {
					out = append(out, trimmed)
				}
				next = true
			}
			if atCommand && trimmed != "" {
				word := strings.TrimLeft(trimmed, "$(`")
				if defined[word] {
					atCommand = next
					continue
				}
				// An assignment prefix keeps the next word in command
				// position (`LC_ALL=C tar …`) and is not a program itself.
				if i := strings.IndexByte(word, '='); i > 0 && assignmentName.MatchString(word[:i]) {
					next = true
				} else if commandWord.MatchString(word) {
					out = append(out, word)
				}
			}
			atCommand = next
		}
	}
	// A keyword emitted as a separator above is not a command word; the
	// reviewed list names them as keywords, so keep only the shapes that
	// could be a program.
	kept := out[:0]
	for _, w := range out {
		if commandWord.MatchString(w) {
			kept = append(kept, w)
		}
	}
	return kept
}

// assignmentName is the left side of a shell assignment: a word of that
// shape at command position sets a variable, it starts no program.
var assignmentName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// shellFuncDef matches a shell function definition: a call to one runs no
// program of its own.
var shellFuncDef = regexp.MustCompile(`(?m)^[ \t]*([a-z_][a-z0-9_]*)[ \t]*\(\)`)

// unquoted drops the quoted spans of a script line: a program name is never
// quoted in this tree's scripts, and the words inside a message are not
// command positions.
func unquoted(line string) string {
	// shellquote.Quote ends and reopens the single-quoted run around an
	// apostrophe; read as quotes, that idiom flips the state and leaves the
	// rest of a message looking like commands.
	line = strings.ReplaceAll(line, `'\''`, "")
	var b strings.Builder
	var quote rune
	for _, r := range line {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
			continue
		case r == '\'' || r == '"':
			quote = r
			b.WriteByte(' ')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// seamPackages returns, relative to root, every package that holds the exec
// seam: derived from the code, so a package that starts commands in a
// container is scanned the day it appears.
func seamPackages(t *testing.T, root string) []string {
	t.Helper()
	dirs := map[string]bool{}
	for _, top := range []string{"pkg", "cmd", "internal"} {
		base := filepath.Join(root, top)
		if _, err := os.Stat(base); err != nil {
			continue
		}
		if err := filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if !seamNames.Match(body) {
				return nil
			}
			rel, err := filepath.Rel(root, filepath.Dir(path))
			if err != nil {
				return err
			}
			dirs[rel] = true
			return nil
		}); err != nil {
			t.Fatalf("walk %s: %v", base, err)
		}
	}
	out := make([]string, 0, len(dirs))
	for d := range dirs {
		out = append(out, d)
	}
	sort.Strings(out)
	if len(out) == 0 {
		t.Fatalf("no package of this module names the exec seam (%s): the derivation no longer matches the code", seamNames)
	}
	return out
}

func slicesContains(haystack []string, want string) bool {
	for _, s := range haystack {
		if s == want {
			return true
		}
	}
	return false
}

func nonTestGoFiles(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	sort.Strings(out)
	return out
}

// repoRootForInventory walks up from the test's working directory to the
// module root, so the walk reads the tree it is part of.
func repoRootForInventory(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test's working directory: cannot locate the module root")
		}
		dir = parent
	}
}
