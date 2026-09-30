package bots

import (
	"regexp"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// pythonInterpreter finds a python interpreter in a shell body, by name or
// by path; pythonCalls checks what follows the name.
var pythonInterpreter = regexp.MustCompile("(?:^|[\\s;&|(`\"'/])(python3?(?:\\.[0-9]+)?)")

// pythonCall is one python invocation of a shell body.
type pythonCall struct {
	line     string // the line that makes it
	stdin    bool   // its program comes from -c, -m or stdin, not a script file
	isolated bool   // -I is among its flags
}

// pythonCalls lists the python invocations of a shell body, comment lines
// aside. The mode is read the way python reads its arguments: flags,
// clustered or not (-W and -X take a value), until -c, -m, `-`, `--` or the
// first word, a script file. Without a script file python reads stdin: a
// `<` redirect or a heredoc, or a pipe when nothing follows the flags.
func pythonCalls(body string) []pythonCall {
	body = strings.ReplaceAll(body, "\\\n", " ")
	var calls []pythonCall
	for _, m := range pythonInterpreter.FindAllStringSubmatchIndex(body, -1) {
		end := m[3]
		if end < len(body) && !strings.ContainsRune(" \t\n;&|)<>`\"'", rune(body[end])) {
			continue
		}
		start := strings.LastIndexByte(body[:m[2]], '\n') + 1
		if strings.HasPrefix(strings.TrimSpace(body[start:m[2]]), "#") {
			continue
		}
		stop := strings.IndexByte(body[end:], '\n')
		if stop < 0 {
			stop = len(body) - end
		}
		c := pythonCall{line: strings.TrimSpace(body[start : end+stop])}
		var mode pythonProgram
		mode, c.isolated = pythonMode(body[end : end+stop])
		c.stdin = mode == programInline || mode == programNone && pipeFed(strings.TrimRight(body[start:m[2]], "/"+pathChars))
		calls = append(calls, c)
	}
	return calls
}

// pythonProgram is where an invocation takes its program from.
type pythonProgram int

const (
	programScript pythonProgram = iota // a script file
	programInline                      // -c, -m, `-` or a `<` redirect
	programNone                        // nothing after the flags: stdin as inherited
)

// outputRedirect is a shell redirection of an output, which leaves stdin
// alone.
var outputRedirect = regexp.MustCompile(`^(?:[0-9]*>[>&|]?|&>>?)`)

// pythonMode reads the arguments that follow a python interpreter on its line.
// A quote right after the name closes the quoted command it ends.
func pythonMode(args string) (program pythonProgram, isolated bool) {
	if strings.HasPrefix(args, `"`) || strings.HasPrefix(args, "'") {
		return programNone, false
	}
	rest := args
	skipValue := false
	for {
		rest = strings.TrimLeft(rest, " \t")
		if rest == "" || strings.ContainsRune(";&|)`", rune(rest[0])) {
			return programNone, isolated
		}
		if rest[0] == '<' || strings.HasPrefix(rest, "0<") {
			return programInline, isolated
		}
		if r := outputRedirect.FindString(rest); r != "" {
			rest = strings.TrimLeft(rest[len(r):], " \t")
			rest = rest[len(shellWord(rest)):]
			continue
		}
		word := shellWord(rest)
		rest = rest[len(word):]
		switch {
		case skipValue:
			skipValue = false
		case word == "-":
			return programInline, isolated
		case word == "--":
			switch shellWord(strings.TrimLeft(rest, " \t")) {
			case "-":
				return programInline, isolated
			case "":
			default:
				return programScript, isolated
			}
		case strings.HasPrefix(word, "--"):
		case strings.HasPrefix(word, "-"):
			flags := word[1:]
		cluster:
			for i := 0; i < len(flags); i++ {
				switch flags[i] {
				case 'I':
					isolated = true
				case 'c', 'm':
					return programInline, isolated
				case 'W', 'X':
					skipValue = i == len(flags)-1
					break cluster
				}
			}
		default:
			return programScript, isolated
		}
	}
}

// pathChars is what a directory written before an interpreter is made of.
const pathChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-~${}"

// pipeWrapper is what may stand between a pipe and the command it feeds:
// variable assignments and the wrappers that exec their argument.
var pipeWrapper = regexp.MustCompile(`^(?:[A-Za-z_][A-Za-z0-9_]*=\S*|env|exec|nice|nohup|time)$`)

// pipeFed reports that before, the text of a line up to an interpreter,
// ends in a pipe into that interpreter.
func pipeFed(before string) bool {
	i := strings.LastIndexAny(before, "|;&(`")
	if i < 0 || before[i] != '|' || i > 0 && before[i-1] == '|' {
		return false
	}
	for _, w := range strings.Fields(before[i+1:]) {
		if !pipeWrapper.MatchString(w) {
			return false
		}
	}
	return true
}

// shellWord is the word s starts with, up to a blank or a shell operator.
func shellWord(s string) string {
	for i, r := range s {
		if strings.ContainsRune(" \t;&|)<>", r) {
			return s[:i]
		}
	}
	return s
}

// TestCatalogPythonRunsIsolated: every python invocation a shell body of the
// catalog makes with -c, -m or stdin runs isolated (-I).
//
// A tool node runs with the workspace as its working directory. With -c, -m
// or stdin, python puts that directory first on sys.path, so a json.py,
// hashlib.py or subprocess.py at the root of the judged tree, git-ignored or
// not, replaces the standard module inside the node: a gate whose verdict
// the tree under judgement writes. -I drops that entry. A script file puts
// its own directory there instead, which the script's author owns. A
// `script:` body in python is the engine's to isolate (scriptInterpreter);
// this guard reads the shell bodies, where the author writes the invocation.
func TestCatalogPythonRunsIsolated(t *testing.T) {
	found := 0
	for _, path := range catalogWorkflowFiles() {
		pr := parseBotUnit(path)
		if pr.File == nil {
			t.Logf("%s: not inspected (unparseable — the parse/compile test owns that)", path)
			continue
		}
		cr := ir.Compile(pr.File)
		if cr.Workflow == nil {
			t.Logf("%s: not inspected (does not compile to a workflow)", path)
			continue
		}
		for _, n := range cr.Workflow.Nodes {
			tool, ok := n.(*ir.ToolNode)
			if !ok {
				continue
			}
			bodies := []string{tool.Command, tool.Postcondition}
			switch tool.Language {
			case "", "sh", "bash":
				bodies = append(bodies, tool.Script)
			}
			for _, body := range bodies {
				for _, c := range pythonCalls(body) {
					if !c.stdin {
						continue
					}
					found++
					if !c.isolated {
						t.Errorf("%s: node %q runs python without -I in %q: the workspace it runs in comes first on sys.path",
							path, tool.ID, c.line)
					}
				}
			}
		}
	}
	if found < 100 {
		t.Fatalf("found %d python invocations in the catalog's shell bodies, want the catalog's ~150 — the scan no longer reads them", found)
	}
}

// TestPythonCallsReadTheMode pins the guard's reading of an invocation, both
// ways: every form that puts the working directory on sys.path is read as
// such, with its -I or without, and a script file is not.
func TestPythonCallsReadTheMode(t *testing.T) {
	cases := []struct {
		body            string
		stdin, isolated bool
	}{
		{`python3 -c "import json"`, true, false},
		{`python3 -I -c "import json"`, true, true},
		{`python3 -Ic "import json"`, true, true},
		{`python3 -Bc "import json"`, true, false},
		{`python3 -m json.tool`, true, false},
		{`python3 -I -m json.tool`, true, true},
		{`python3 - <<'PY'`, true, false},
		{"python3 <<'PY'\nimport json\nPY", true, false},
		{"python3 -I <<'PY'\nimport json\nPY", true, true},
		{`python3<<'PY'`, true, false},
		{`python3 < gate.py`, true, false},
		{`python3 0< gate.py`, true, false},
		{`python3 <<< "import json"`, true, false},
		{`python3 2>/dev/null <<'PY'`, true, false},
		{`python3 > out.json <<'PY'`, true, false},
		{`python3 -I 2>&1 <<'PY'`, true, true},
		{`cat gate.py | python3`, true, false},
		{`cat gate.py | python3 | jq .`, true, false},
		{`cat gate.py | python3 -I`, true, true},
		{`(cat gate.py | python3)`, true, false},
		{`cat gate.py | /usr/bin/python3`, true, false},
		{`cat gate.py | ${PY_DIR}/python3 -I`, true, true},
		{`cat gate.py | env LC_ALL=C python3`, true, false},
		{`cat gate.py | PYTHONUTF8=1 python3 -B`, true, false},
		{`sh -c "cat gate.py | python3"`, true, false},
		{`python3 -I -- <<'PY'`, true, true},
		{`cat gate.py | python3 --`, true, false},
		{`python3`, false, false},
		{`false || python3`, false, false},
		{`cat x | grep python3`, false, false},
		{`if ! command -v python3 >/dev/null 2>&1; then exit 0; fi`, false, false},
		{`/usr/bin/python3 -c "import json"`, true, false},
		{`/usr/bin/python3.12 -I -c "import json"`, true, true},
		{`python -c "import json"`, true, false},
		{`X="$(python3 -c 'print(1)')"`, true, false},
		{`python3 -W ignore -c "import json"`, true, false},
		{`python3 -W ignore -I -c "import json"`, true, true},
		{`python3 -Wignore -c "import json"`, true, false},
		{`python3 -X utf8 -c "import json"`, true, false},
		{"python3 \\\n  -c 'import json'", true, false},
		{`python3 -- -`, true, false},
		{`python3 gate.py`, false, false},
		{`python3 -I gate.py`, false, true},
		{`python3 "$BOT_DIR/gate.py" --out x`, false, false},
		{`python3 -W ignore gate.py`, false, false},
		{`python3 -- gate.py`, false, false},
	}
	for _, tc := range cases {
		calls := pythonCalls(tc.body)
		if len(calls) != 1 {
			t.Errorf("%q: read %d python invocations, want 1", tc.body, len(calls))
			continue
		}
		if calls[0].stdin != tc.stdin || calls[0].isolated != tc.isolated {
			t.Errorf("%q: read stdin=%v isolated=%v, want stdin=%v isolated=%v",
				tc.body, calls[0].stdin, calls[0].isolated, tc.stdin, tc.isolated)
		}
	}
	for _, body := range []string{`echo python3.txt`, `pip install python3-dateutil`, `mypython3 -c x`, `python3x -c x`, `  # cat gate.py | python3 -c x`} {
		if calls := pythonCalls(body); len(calls) != 0 {
			t.Errorf("%q: read %d python invocations, want none", body, len(calls))
		}
	}
}
