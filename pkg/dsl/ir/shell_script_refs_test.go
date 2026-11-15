package ir

import (
	"strings"
	"testing"
)

// TestShellScriptRefPositions pins the reading of each reference's position
// — the quoting the shell reads it in, and whether bash evaluates it as
// arithmetic — over the constructs a shell script writes, and where the
// reading knows it lost the shell's.
func TestShellScriptRefPositions(t *testing.T) {
	t.Parallel()
	const (
		W = ShellRefWord
		D = ShellRefDouble
		S = ShellRefSingle
		H = ShellRefHeredoc
		L = ShellRefLiteralHeredoc
		C = ShellRefComment
		A = ShellRefArith
	)
	type pos struct {
		ctx    ShellRefContext
		arith  bool
		unsure bool
	}
	w, ar, un := func(c ShellRefContext) pos { return pos{ctx: c} }, func(c ShellRefContext) pos { return pos{ctx: c, arith: true} }, pos{ctx: W, unsure: true}
	cases := []struct {
		name, script string
		want         []pos
	}{
		{"argument and assignment", "echo {{input.a}}\nX={{input.b}}", []pos{w(W), w(W)}},
		{"double quotes", `echo "pre {{input.a}} post" "{{input.b}}" $"x {{input.c}}"`, []pos{w(D), w(D), w(D)}},
		{"single quotes", `echo '{{input.a}}' x'{{input.b}}' $'{{input.c}}'`, []pos{w(S), w(S), w(S)}},
		{"a substitution in double quotes", `echo "$(printf %s {{input.a}} "{{input.b}}")" {{input.c}}`, []pos{w(W), w(D), w(W)}},
		{"backquotes", "echo `printf %s {{input.a}}` \"`echo {{input.b}}`\"", []pos{w(W), w(W)}},
		{"a comment, and one after a redirection", "echo a # {{input.a}} ' \"\necho b >#{{input.b}}\necho {{input.c}}", []pos{w(C), w(C), w(W)}},
		{"glued to a dollar or a backslash", "echo ${{input.a}} \"${{input.b}}\" \\{{input.c}}", []pos{w(W), w(D), w(W)}},
		{"default words", "echo ${X:-{{input.a}}} \"${X:-{{input.b}}}\"", []pos{w(W), w(D)}},
		{"a subscript and an offset", "echo ${arr[{{input.a}}]} \"${s:{{input.b}}}\" ${s:-{{input.c}}}", []pos{ar(W), ar(D), w(W)}},
		{"unquoted, stripped and quoted heredocs", "cat <<EOF\n\"{{input.a}}\"\nEOF\ncat <<-E\n\t{{input.b}}\n\tE\ncat <<\\Q\n{{input.c}}\nQ\ncat <<E'OF'\n{{input.d}}\nEOF\necho {{input.e}}", []pos{w(H), w(H), w(L), w(L), w(W)}},
		{"two heredocs on a line", "cat <<A - <<'B' {{input.z}}\n{{input.a}}\nA\n{{input.b}}\nB", []pos{w(W), w(H), w(L)}},
		{"a substitution in a heredoc", "cat <<EOF\n$(printf %s {{input.a}} \"{{input.b}}\" '{{input.c}}') {{input.d}}\nEOF", []pos{w(W), w(D), w(S), w(H)}},
		{"arithmetic", "echo $(( {{input.a}} + 1 ))\n(( x = {{input.b}} ))\necho {{input.c}}", []pos{ar(A), ar(A), w(W)}},
		{"[[ ]] integer operands", "[[ {{input.a}} -gt 0 && 0 -lt \"{{input.b}}\" ]] && [[ {{input.c}} == x ]]", []pos{ar(W), ar(D), w(W)}},
		{"let and declare -i", "let n={{input.a}}+1\ndeclare -i m={{input.b}}\nf() { local -ri k={{input.c}}; }\ndeclare v={{input.d}}", []pos{ar(W), ar(W), ar(W), w(W)}},
		{"array element assignments", "arr[{{input.a}}]=1\narr[0]={{input.b}}", []pos{ar(W), w(W)}},
		{"test is not arithmetic", "[ {{input.a}} -eq 0 ]", []pos{w(W)}},
		{"bash ends a heredoc inside $( ) at EOF)", "x=$(cat <<EOF\nhi\nEOF)\necho {{input.a}} \"{{input.b}}\"", []pos{w(W), w(D)}},
		{"a case inside a substitution loses the reading", "x=$(case a in a) echo;; esac); echo {{input.a}}", []pos{un}},
		{"a heredoc that never ends", "echo {{input.z}}\ncat <<EOF\n{{input.a}}", []pos{w(W), un}},
		{"a heredoc line continued", "cat <<EOF\na\\\nEOF\n{{input.a}}\nEOF", []pos{un}},
		{"a reference as a delimiter", "cat <<{{input.a}}\nx\n{{input.b}}", []pos{un, un}},
	}
	for _, c := range cases {
		for _, bash := range []bool{false, true} {
			got := ShellScriptRefPositions(c.script, bash)
			spans := TemplateSpans(c.script)
			if len(spans) != len(c.want) {
				t.Fatalf("%s: %d references, want %d", c.name, len(spans), len(c.want))
			}
			for i, span := range spans {
				p := got[span[0]]
				if (pos{ctx: p.Context, arith: p.ArithEval, unsure: p.Unsure}) != c.want[i] {
					t.Errorf("%s (bash=%v): reference %d read as %+v, want %+v", c.name, bash, i, p, c.want[i])
				}
			}
		}
	}
}

// C343 / C344: a data reference in a sh/bash script where the shell reads
// nothing, or where bash evaluates the value as arithmetic, is named at
// authoring time; the positions a shell expands, the raw form, a secret, a
// js script and a reading the scanner lost stay silent.
func TestShellScriptDataPositionsWarnAtAuthoring(t *testing.T) {
	t.Parallel()
	script := func(language, body string) string {
		return "tool post:\n  language: " + language + "\n  script: |\n    " + strings.ReplaceAll(body, "\n", "\n    ") + "\n  output: out\n"
	}
	for name, c := range map[string]struct {
		body       string
		c343, c344 int
	}{
		"single quotes":                   {script("sh", "printf '%s' '{{run.id}}'"), 1, 0},
		"a quoted heredoc":                {script("sh", "cat <<'EOF'\n{{run.id}}\nEOF"), 1, 0},
		"a comment":                       {script("bash", "true # {{run.id}}"), 1, 0},
		"arithmetic":                      {script("sh", "echo $(( {{vars.n}} + 1 ))"), 0, 1},
		"a [[ -gt ]] operand":             {script("bash", "[[ {{vars.n}} -gt 0 ]] && echo pos"), 0, 1},
		"let, declare -i and a subscript": {script("bash", "let x={{vars.n}}\ndeclare -i y={{vars.n}}\na[{{run.id}}]=1"), 0, 2},
		"once per reference":              {script("sh", "echo '{{run.id}}' '{{run.id}}'"), 1, 0},
		"positions the shell expands":     {script("sh", "printf '%s' {{run.id}} \"{{run.id}}\"\ncat <<EOF\n{{run.id}}\nEOF"), 0, 0},
		"the raw form":                    {script("sh", "echo '{{!run.id}}' $(( {{!vars.n}} ))"), 0, 0},
		"a js script":                     {script("js", "const x = '{{run.id}}';"), 0, 0},
		"a lost reading":                  {script("sh", "x=$(case a in a) echo;; esac)\necho '{{run.id}}'"), 0, 0},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			src := "dsl: 2\n\nschema out:\n  ok: bool\n\n" + c.body + "\nworkflow w:\n  worktree: none\n  sandbox: none\n  vars:\n    n: string = \"1\"\n  entry: post\n  post -> done\n"
			res := compileText(t, src)
			var c343, c344 []Diagnostic
			for _, d := range res.Diagnostics {
				if d.Severity == SeverityError {
					t.Errorf("the fixture does not compile clean: %s", d.Error())
				}
				switch d.Code {
				case DiagShellScriptDataNotArriving:
					c343 = append(c343, d)
				case DiagShellScriptDataArith:
					c344 = append(c344, d)
				}
			}
			if len(c343) != c.c343 || len(c344) != c.c344 {
				t.Fatalf("C343 %d, C344 %d, want %d and %d:\n%v", len(c343), len(c344), c.c343, c.c344, res.Diagnostics)
			}
			for _, d := range append(c343, c344...) {
				if d.Severity != SeverityWarning || d.NodeID != "post" || d.Line == 0 || strings.TrimSpace(d.Hint) == "" || !strings.Contains(d.Message, "body line") {
					t.Errorf("%s = %+v, want a positioned warning with a fix line", d.Code, d)
				}
			}
		})
	}
}
