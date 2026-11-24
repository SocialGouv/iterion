package bots

import (
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// A tool node's stdout is its answer. A `command:` / `script:` that declares
// `output:` and prints anything beside that ONE JSON object fails its node
// (model.ToolStdoutNotJSONError, #3421) — the engine no longer wraps the text
// as `{"result": …}` and lets the run go on. So a recipe that writes to its
// stdout from two places is a node that fails on the day both of them speak:
// measured on feed-watch, whose three state commits ran `git commit` as a
// child of the script that then printed the answer — `[main 1a2b3c4] chore…`
// ahead of the JSON, on every run that had something to commit.
//
// The engine's refusal at the node is the guard. This is a LINT beside it: it
// reads the text of every such recipe the corpus ships for the few shapes
// that have already cost a run, so those are met at authoring time instead
// of at the node. It does not prove a recipe prints one object — a reviewer
// planted eleven recipes that print beside their JSON, the engine refused all
// eleven and this reading named two. What it reads:
//
//   - an emitter: the recipe carries at least one statement that prints a
//     JSON object (toolStdoutEmitters);
//   - Python: every `print(` either names another stream (`file=`) or prints
//     `json.dumps(…)`, and every child process names where its stdout goes
//     (`capture_output=`, `stdout=`) unless its argv is one of git's silent
//     forms (silentGitChild);
//   - JavaScript: every `console.log(` prints `JSON.stringify(…)`, and no
//     child inherits the node's stdio;
//   - a recipe written in shell alone — a `command:`, a `language: sh|bash`
//     script, with no embedded interpreter: every git command that prints and
//     every `curl` / `wget` sends its stdout somewhere — a redirect, a
//     capture, or a pipeline that ends in one.
//
// What it does NOT read: the inside of a program a recipe calls (a scanner,
// a project script, rgaa-audit's run library behind `L.out`), the shell
// around an embedded interpreter, any other command that prints (`echo`,
// `cat`, a package manager, a build), what a value interpolated into a
// printed object does to it, and whether two emitters can both run in one
// execution.
var toolStdoutEmitters = []*regexp.Regexp{
	regexp.MustCompile(`print\(json\.dumps\(`),
	regexp.MustCompile(`process\.stdout\.write\(\s*JSON\.stringify\(`),
	regexp.MustCompile(`console\.log\(\s*JSON\.stringify\(`),
	// rgaa-audit's run library: `out` and `refuse` print the object.
	regexp.MustCompile(`\bL\.(out|refuse)\(`),
	regexp.MustCompile(`\bjq\b`),
	regexp.MustCompile(`printf\s+['"](%s\\n['"]\s+['"])?\{`),
}

// toolStdoutExceptions are the sites the reading above flags and a person
// has read, keyed by `<file>::<node>::<rule>`: the count of sites allowed and
// why each is not a write to the node's stdout. A count that no longer
// matches reddens, so an exception cannot outlive its site or cover a new
// one.
var toolStdoutExceptions = map[string]struct {
	sites int
	why   string
}{
	"sec-audit-source/main.bot::run_deepsec_scanner::python-print": {2,
		"both programs run inside a `$(python3 -I -c …)` command substitution: the shell captures what they print"},
	"golden-master/main.bot::oracle_run::python-child": {1,
		"`os.system(` is spelled inside the selftest's refusal message, not called"},
	"golden-master/sync-harness.bot::sync_harness::python-child": {1,
		"`os.system(` is spelled inside the selftest's refusal message, not called"},
	"product-docs/main.bot::route_table::python-child": {1,
		"a line of the guardian program the node starts with `stdout=subprocess.PIPE`: the guardian's child writes into that pipe"},
}

var (
	pyPrintCall    = regexp.MustCompile(`(?:^|[^A-Za-z0-9_.])print\(`)
	pyChildCall    = regexp.MustCompile(`subprocess\.(?:run|call|check_call|Popen)\(`)
	pyOSChildCall  = regexp.MustCompile(`\bos\.(?:system|popen|exec[lv]p?e?|spawn[a-z]+)\(`)
	pyNamesStdout  = regexp.MustCompile(`capture_output\s*=|stdout\s*=`)
	pyNamesFile    = regexp.MustCompile(`\bfile\s*=`)
	jsConsoleCall  = regexp.MustCompile(`console\.(?:log|info)\(`)
	jsStdio        = regexp.MustCompile(`stdio\s*:\s*(\[[^\]]*\]|'[a-z]+'|"[a-z]+")`)
	embeddedInterp = regexp.MustCompile(`\b(?:python3|node)\b`)
	// A git command that prints on stdout, or a transfer whose body does,
	// in the shell's own spelling.
	shellChatty = regexp.MustCompile(`(?:^|[^\w'"./-])(git(?: -[cC] \S+)* (?:commit|pull|merge|rebase|checkout|reset|stash|init|clean|clone|switch|restore|revert|cherry-pick)\b|curl\b|wget\b)`)
	// `>file`, `>>file`, `1>file`, `&>file`, `>&2` — never `2>…`, which
	// moves stderr and leaves stdout where it was.
	stdoutRedirect = regexp.MustCompile(`(?:^|[^0-9&>])(?:1?>>?|&>)\s*(?:&2|[^\s&>]+)`)
	shellQuiet     = regexp.MustCompile(`(?:^|\s)(?:-q|--quiet|-o|--output)(?:\s|=|$)`)
)

// callText returns the parenthesised argument list that opens at src[open],
// quotes respected, so a `)` inside a string does not end it.
func callText(src string, open int) string {
	depth := 0
	var quote byte
	for i := open; i < len(src); i++ {
		c := src[i]
		switch {
		case quote != 0:
			switch c {
			case '\\':
				i++
			case quote:
				quote = 0
			}
		case c == '\'' || c == '"' || c == '`':
			quote = c
		case c == '(':
			depth++
		case c == ')':
			depth--
			if depth == 0 {
				return src[open : i+1]
			}
		}
	}
	return src[open:]
}

// silentGitChild reports whether a subprocess call's argv is one of the git
// forms that print nothing on stdout: `add`, `config`, a `diff` asked to be
// `--quiet`, `rebase --abort` — with or without a `-C <dir>`.
func silentGitChild(call string) bool {
	norm := strings.NewReplacer(`"`, `'`, " ", "", "\n", "", "\t", "").Replace(call)
	rest, ok := strings.CutPrefix(norm, "(['git',")
	if !ok {
		return false
	}
	if after, cut := strings.CutPrefix(rest, "'-C',"); cut {
		if comma := strings.IndexByte(after, ','); comma >= 0 {
			rest = after[comma+1:]
		}
	}
	switch {
	case strings.HasPrefix(rest, "'add'"), strings.HasPrefix(rest, "'config',"), strings.HasPrefix(rest, "'rebase','--abort'"):
		return true
	case strings.HasPrefix(rest, "'diff',"):
		return strings.Contains(rest, "'--quiet'")
	}
	return false
}

// shellPipeline returns the pipeline that starts at src[from] — up to the
// next `;`, `&&`, `||`, newline, closing parenthesis or lone `&`, quotes
// respected — split into its stages.
func shellPipeline(src string, from int) []string {
	var stages []string
	start := from
	var quote byte
	i := from
scan:
	for ; i < len(src); i++ {
		c := src[i]
		switch {
		case quote != 0:
			if c == '\\' && quote == '"' {
				i++
			} else if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '\\':
			i++
		case c == ';' || c == '\n' || c == ')':
			break scan
		case c == '|':
			if i+1 < len(src) && src[i+1] == '|' {
				break scan
			}
			stages = append(stages, src[start:i])
			start = i + 1
		case c == '&':
			if i > from && src[i-1] == '>' {
				continue // `>&2`
			}
			if i+1 < len(src) && src[i+1] == '>' {
				continue // `&>file`
			}
			break scan
		}
	}
	return append(stages, src[start:i])
}

// withoutCommentLines drops the lines of a recipe that are a comment whole —
// `#` in shell and Python, `//` in JavaScript — so a rule reads statements,
// never the prose about them: an emitter named in a comment prints nothing.
func withoutCommentLines(body string) string {
	lines := strings.Split(body, "\n")
	kept := lines[:0]
	for _, line := range lines {
		if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "//") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// toolStdoutFindings reads one recipe and returns what it finds, by rule.
func toolStdoutFindings(body, language string) map[string][]string {
	body = withoutCommentLines(body)
	found := map[string][]string{}
	flag := func(rule, site string) {
		site = strings.Join(strings.Fields(site), " ")
		if len(site) > 160 {
			site = site[:160] + "…"
		}
		found[rule] = append(found[rule], site)
	}

	emits := false
	for _, rx := range toolStdoutEmitters {
		if rx.MatchString(body) {
			emits = true
			break
		}
	}
	if !emits {
		flag("emitter", "no statement of the recipe prints a JSON object")
	}

	for _, m := range pyPrintCall.FindAllStringIndex(body, -1) {
		call := callText(body, m[1]-1)
		arg := strings.TrimSpace(strings.TrimPrefix(call, "("))
		if pyNamesFile.MatchString(call) || strings.HasPrefix(arg, "json.dumps(") {
			continue
		}
		flag("python-print", "print"+call)
	}
	for _, m := range pyChildCall.FindAllStringIndex(body, -1) {
		call := callText(body, m[1]-1)
		if pyNamesStdout.MatchString(call) || silentGitChild(call) {
			continue
		}
		flag("python-child", body[m[0]:m[1]-1]+call)
	}
	for _, m := range pyOSChildCall.FindAllStringIndex(body, -1) {
		flag("python-child", body[m[0]:m[1]-1]+callText(body, m[1]-1))
	}

	for _, m := range jsConsoleCall.FindAllStringIndex(body, -1) {
		call := callText(body, m[1]-1)
		if strings.Contains(call, "JSON.stringify(") {
			continue
		}
		flag("js-console", body[m[0]:m[1]-1]+call)
	}
	for _, m := range jsStdio.FindAllStringSubmatch(body, -1) {
		if strings.Contains(m[1], "inherit") {
			flag("js-stdio", m[0])
		}
	}

	// The shell's own spelling is only read where the recipe is shell alone:
	// around an embedded program the same words are prose in its strings.
	if (language == "" || language == "sh" || language == "bash") && !embeddedInterp.MatchString(body) {
		for _, m := range shellChatty.FindAllStringSubmatchIndex(body, -1) {
			at := m[2]
			if before := strings.TrimRight(body[:at], " "); strings.HasSuffix(before, "$(") || strings.HasSuffix(before, "`") {
				continue // captured
			}
			stages := shellPipeline(body, at)
			last := stages[len(stages)-1]
			switch {
			case stdoutRedirect.MatchString(last):
			case len(stages) == 1 && shellQuiet.MatchString(last):
			case len(stages) > 1 && strings.HasPrefix(strings.TrimSpace(last), "jq "):
			default:
				flag("shell-stdout", strings.Join(stages, "|"))
			}
		}
	}
	return found
}

// productRunFixtures are the bots outside the shipped corpus that this
// repository's own suites run with the PRODUCTION executor, so the engine's
// refusal is a red suite there and nowhere a per-change job looks: the
// studio's e2e seeds — its server aborts the whole Playwright run when a
// seed run does not finish — and the fixture the live-tagged e2e test
// executes. The other bots under e2e/testdata/ are not read here: most are
// compiled, or run against a scenario executor that stubs their tool nodes;
// the `validate_syntax` nodes of dual_model_plan_implement_review.bot and
// session_review_fix.bot are run by live-tagged tests too, through a
// `_tools/validate_syntax.js` those tests copy from
// `examples/validate_syntax.js`, a file this tree does not hold — so what
// they print cannot be read from here.
var productRunFixtures = []catalogRoot{
	{"../studio/e2e/fixtures/bots/*/*.bot", 5, "the studio e2e suite's seed bots"},
	{"../e2e/testdata/exhaustive_dsl_coverage.bot", 1, "the fixture the live-tagged e2e test runs"},
}

func productRunFixtureBots(t *testing.T) []string {
	t.Helper()
	var paths []string
	for _, root := range productRunFixtures {
		paths = append(paths, catalogRootFiles(t, root)...)
	}
	sort.Strings(paths)
	return paths
}

// TestCatalogToolStdoutIsItsJSONAnswer reads every shipped tool recipe that
// declares `output:`, and the fixtures this repository runs with the real
// executor, for the shapes described above. Green means none of those shapes
// is there — not that every recipe prints one object.
func TestCatalogToolStdoutIsItsJSONAnswer(t *testing.T) {
	checked := 0
	used := map[string]bool{}
	for _, path := range append(catalogWorkflowFiles(t), productRunFixtureBots(t)...) {
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
		ids := make([]string, 0, len(cr.Workflow.Nodes))
		for id := range cr.Workflow.Nodes {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			tn, ok := cr.Workflow.Nodes[id].(*ir.ToolNode)
			// The refusal's own scope: a command or a script that declares
			// `output:`. A node with a `postcondition:` publishes a merge and
			// is judged by its oracle; a registry tool's answer is the
			// tool's; an action's is the operation's.
			if !ok || tn.OutputSchema == "" || tn.Postcondition != "" {
				continue
			}
			kind := ir.ToolRecipeKindOf(tn)
			if kind != ir.ToolRecipeShell && kind != ir.ToolRecipeScript {
				continue
			}
			body := tn.Command
			if kind == ir.ToolRecipeScript {
				body = tn.Script
			}
			checked++
			findings := toolStdoutFindings(body, tn.Language)
			rules := make([]string, 0, len(findings))
			for rule := range findings {
				rules = append(rules, rule)
			}
			sort.Strings(rules)
			for _, rule := range rules {
				sites := findings[rule]
				key := strings.TrimPrefix(path, "../") + "::" + id + "::" + rule
				if allowed, listed := toolStdoutExceptions[key]; listed {
					used[key] = true
					if len(sites) != allowed.sites {
						t.Errorf("%s: %d site(s), and toolStdoutExceptions allows %d (%s):\n  %s", key, len(sites), allowed.sites, allowed.why, strings.Join(sites, "\n  "))
					}
					continue
				}
				t.Errorf("%s: node %q declares `output: %s`, so its stdout must be ONE JSON object — the engine fails the node on anything else — and this recipe can write beside it (%s):\n  %s\nSend it to stderr (`>&2`, `stdout=sys.stderr`), capture it, or — if a person has read that it never reaches the node's stdout — list it in toolStdoutExceptions with the reason.",
					path, id, tn.OutputSchema, rule, strings.Join(sites, "\n  "))
			}
		}
	}
	for key := range toolStdoutExceptions {
		if !used[key] {
			t.Errorf("toolStdoutExceptions lists %s, which the corpus no longer flags — drop it", key)
		}
	}
	// 252 recipes when this guard was written (265 tool nodes, 13 of them
	// without `output:`). A walk that reads a handful read nothing.
	if checked < 200 {
		t.Fatalf("only %d tool recipes inspected — the walk no longer covers the corpus", checked)
	}
}

// The reading's own witnesses: each rule on the shape it exists for, and on
// the nearest shape it must leave alone.
func TestToolStdoutFindingsReadTheShapesTheyName(t *testing.T) {
	cases := []struct {
		name, body, language string
		want                 []string
	}{
		{"the incident: a bare git commit ahead of nothing", "git -C {{input.ws}} add -- f && (git -C {{input.ws}} diff --cached --quiet || git -C {{input.ws}} commit -m {{input.msg}})", "",
			[]string{"emitter", "shell-stdout"}},
		{"the same commit sent to stderr, then the object", "git -C {{input.ws}} commit -m {{input.msg}} >&2 && printf '{\"success\": true}'", "", nil},
		{"a redirect of stderr alone leaves stdout where it was", "git commit -m x 2>/dev/null; printf '{\"ok\": true}'", "", []string{"shell-stdout"}},
		{"a transfer whose body is the node's stdout", "curl -sS http://localhost/x 2>&1 | head -c 1000 || true; printf '{\"ok\": true}'", "", []string{"shell-stdout"}},
		{"a transfer tested for its status only", "if curl -sS http://localhost/x >&2; then printf '{\"ok\": true}'; fi", "", nil},
		{"a captured command", "SHA=$(git -C {{input.ws}} checkout -q main && git rev-parse HEAD); printf '{\"sha\": \"%s\"}' \"$SHA\"", "", nil},
		{"a pipeline that ends in a redirect", "printf '%s' \"$MSG\" | git commit -F - >&2; printf '{\"ok\": true}'", "", nil},
		{"a python child that inherits the node's stdout", "import subprocess, json\nsubprocess.run(['git', 'commit', '-m', msg], check=True)\nprint(json.dumps({'ok': True}))", "py",
			[]string{"python-child"}},
		{"the same child sent to stderr", "import subprocess, json, sys\nsubprocess.run(['git', 'commit', '-m', msg], check=True, stdout=sys.stderr)\nprint(json.dumps({'ok': True}))", "py", nil},
		{"git's silent forms", "import subprocess, json\nsubprocess.run(['git', '-C', ws, 'add', d], check=True)\nsubprocess.run([\"git\", \"diff\", \"--cached\", \"--quiet\"])\nsubprocess.run(['git', 'rebase', '--abort'])\nprint(json.dumps({'ok': True}))", "py", nil},
		{"a diff that is not asked to be quiet prints", "import subprocess, json\nsubprocess.run(['git', 'diff', '--cached'])\nprint(json.dumps({'ok': True}))", "py", []string{"python-child"}},
		{"a print that is not the object", "import json\nprint('done')\nprint(json.dumps({'ok': True}))", "py", []string{"python-print"}},
		{"a print to another stream", "import json, sys\nprint('done', file=sys.stderr)\nprint(json.dumps({'ok': True}))", "py", nil},
		{"a console.log that is not the object", "console.log('done'); process.stdout.write(JSON.stringify({ok: true}));", "js", []string{"js-console"}},
		{"a child that inherits the node's stdio", "require('child_process').execSync('make', {stdio: 'inherit'}); console.log(JSON.stringify({ok: true}));", "js", []string{"js-stdio"}},
		{"a git configured on its command line", "git -c user.name=bot -C {{input.ws}} -c commit.gpgsign=false commit -m x; printf '{\"ok\": true}'", "", []string{"shell-stdout"}},
		{"a shell script is shell too", "git commit -m x\nprintf '{\"ok\": true}'\n", "sh", []string{"shell-stdout"}},
		{"a bash script that redirects", "git commit -m x >&2\nprintf '{\"ok\": true}'\n", "bash", nil},
		{"plain text under a schema", "printf 'file: %s\\n' {{input.name}}", "", []string{"emitter"}},
		{"an emitter named in a comment prints nothing", "## jq makes it the declared field\nprintf 'file: %s\\n' {{input.name}}", "", []string{"emitter"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			findings := toolStdoutFindings(c.body, c.language)
			got := make([]string, 0, len(findings))
			for rule := range findings {
				got = append(got, rule)
			}
			sort.Strings(got)
			if strings.Join(got, ",") != strings.Join(c.want, ",") {
				t.Errorf("rules = %v, want %v (%v)", got, c.want, findings)
			}
		})
	}
}
