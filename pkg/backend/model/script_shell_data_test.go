package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// hostileShellValues are data a model or a webhook could hand a shell
// script: every expansion the shell has, the quotes, the escapes, the
// bytes a JSON literal would cook, the text that ends a heredoc or a
// comment, and the reads of the interpreter's environment the secret path
// cares about (#3291).
var hostileShellValues = []string{
	"",
	"plain",
	"two  words",
	"$(touch pwned)",
	"`touch pwned`",
	"$HOME",
	"${HOME}",
	"${!ITERION_@}",
	"${x:=${!__ITERION_AUTHORED_@}}${!x}",
	"$__ITERION_SECRET_x__",
	"$(env)",
	"'",
	`"`,
	`it's "quoted"`,
	`'\''`,
	`'$(touch pwned)'"$(touch pwned)"`,
	`\`,
	`a\\b`,
	`\n`,
	`trailing\`,
	"line1\nline2",
	"\n",
	"a\n\n",
	"\nEOF\n$(touch pwned)\nEOF\n",
	"EOF",
	"#;touch pwned",
	"x\n#\ntouch pwned",
	"*",
	"?",
	"[a]",
	"*?[",
	"{a,b}",
	"~/x",
	"-n",
	"%s%n",
	"\t\r",
	"é ☃ 𝄞",
	"<>&",
	"\u2028\u2029",
	"$((1+1))",
	"$'\\x41'",
	"!!",
}

// shellCanaryEnv is the environment the inertness checks run their scripts
// under: values a data expansion would read, each a marker the output must
// never carry.
func shellCanaryEnv(home string) []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"LC_ALL=C.UTF-8",
		"ITERION_CANARY=leak-canary",
		"__ITERION_SECRET_x__=leak-secret",
		"__ITERION_AUTHORED_m=leak-authored",
	}
}

// shellsFor is the interpreters a script in language may meet: `sh` is
// dash on a Debian host and bash under devbox, so a sh script is held to
// both; a bash script to bash.
func shellsFor(t *testing.T, language string) []string {
	t.Helper()
	candidates := []string{"bash"}
	if language == "sh" {
		candidates = []string{"dash", "bash", "sh"}
	}
	var found []string
	for _, c := range candidates {
		if _, err := exec.LookPath(c); err == nil {
			found = append(found, c)
		}
	}
	if len(found) == 0 || (language == "sh" && !slices.Contains(found, "bash")) {
		t.Fatalf("no interpreter for %s among %v (found %v)", language, candidates, found)
	}
	return found
}

// runShellScript runs script with interpreter in a scratch directory it owns,
// under shellCanaryEnv, and returns its stdout and whether any `pwned` file
// appeared.
func runShellScript(t *testing.T, interpreter, script string) (string, bool) {
	t.Helper()
	dir := t.TempDir()
	home := t.TempDir()
	path := filepath.Join(dir, "script.sh")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(interpreter, path)
	cmd.Dir = dir
	cmd.Env = shellCanaryEnv(home)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s failed on\n%s\nerr=%v stderr=%s", interpreter, script, err, stderr.String())
	}
	pwned := false
	for _, d := range []string{dir, home} {
		if _, err := os.Stat(filepath.Join(d, "pwned")); err == nil {
			pwned = true
		}
	}
	return stdout.String(), pwned
}

func jsonText(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// shellPositions are the positions a shell script holds a reference at, each
// with what the script prints for value v: the value's bytes where the
// reference is a word or a string's content, its JSON literal where the
// position has always received one (single quotes, a heredoc body).
var shellPositions = []struct {
	name   string
	script string
	want   func(t *testing.T, v string) string
}{
	{"argument", "printf '%s' {{input.v}}", func(_ *testing.T, v string) string { return v }},
	{"assignment", "X={{input.v}}\nprintf '%s' \"$X\"", func(_ *testing.T, v string) string { return v }},
	{"inside a substitution", "X=$(printf '%s|' {{input.v}})\nprintf '%s' \"${X%|}\"", func(_ *testing.T, v string) string { return v }},
	{"a whole double-quoted string", "printf '%s' \"{{input.v}}\"", func(_ *testing.T, v string) string { return v }},
	{"inside a double-quoted string", "printf '%s' \"<{{input.v}}> and {{input.v}}\"", func(_ *testing.T, v string) string { return "<" + v + "> and " + v }},
	{"quoted inside a substitution inside quotes", "X=\"$(printf '%s|' \"{{input.v}}\")\"\nprintf '%s' \"${X%|}\"", func(_ *testing.T, v string) string { return v }},
	{"a here-string", "cat <<<{{input.v}}", func(_ *testing.T, v string) string { return v + "\n" }},
	{"inside single quotes", "printf '%s' '{{input.v}}'", func(t *testing.T, v string) string { return jsonText(t, v) }},
	{"inside single quotes beside text", "printf '%s' 'x = {{input.v}};'", func(t *testing.T, v string) string { return "x = " + jsonText(t, v) + ";" }},
	{"an unquoted heredoc", "cat <<EOF\n{\"v\": {{input.v}}, \"home\": \"$HOME\"}\nEOF", func(t *testing.T, v string) string { return `{"v": ` + jsonText(t, v) + `, "home": "HOMEDIR"}` + "\n" }},
	{"a stripped heredoc", "cat <<-EOF\n\t{{input.v}}\n\tEOF", func(t *testing.T, v string) string { return jsonText(t, v) + "\n" }},
	{"a quoted heredoc", "cat <<'EOF'\n{{input.v}} $HOME\nEOF", func(t *testing.T, v string) string { return jsonText(t, v) + " $HOME\n" }},
	{"a backslash-quoted heredoc", "cat <<\\EOF\n{{input.v}}\nEOF", func(t *testing.T, v string) string { return jsonText(t, v) + "\n" }},
	{"a partly quoted heredoc", "cat <<E'OF'\n{{input.v}}\nEOF", func(t *testing.T, v string) string { return jsonText(t, v) + "\n" }},
	{"single quotes in a substitution in a heredoc", "cat <<EOF\n[$(printf '%s' '{{input.v}}')]\nEOF", func(t *testing.T, v string) string { return "[" + jsonText(t, v) + "]\n" }},
	{"a delimiter's escape in double quotes", "cat <<\"E\\OF\"\nEOF\n{{input.v}}\nE\\OF", func(t *testing.T, v string) string { return "EOF\n" + jsonText(t, v) + "\n" }},
	{"a comment", "printf 'x' # {{input.v}}\nprintf 'y'", func(_ *testing.T, _ string) string { return "xy" }},
}

// bashOnlyPositions are the positions only bash parses (dash has no
// here-string): a sh script that writes one runs under bash alone.
var bashOnlyPositions = map[string]bool{"a here-string": true}

// TestShellScriptDataIsInert is the guarantee of #3291: in a sh or a bash
// `script:`, a data reference at every position a shell script holds one
// arrives as the bytes the position promises — the value, or its JSON
// literal — and nothing in it expands: no command runs, no variable of the
// interpreter's environment is read. Each script runs under the real
// interpreters (dash and bash for sh), rendered by the production renderer.
func TestShellScriptDataIsInert(t *testing.T) {
	t.Parallel()
	for _, language := range []string{"sh", "bash"} {
		for _, pos := range shellPositions {
			refs := mustRefs(pos.script)
			for _, interpreter := range shellsFor(t, language) {
				if bashOnlyPositions[pos.name] && interpreter != "bash" {
					continue
				}
				t.Run(language+"/"+interpreter+"/"+pos.name, func(t *testing.T) {
					t.Parallel()
					for _, v := range hostileShellValues {
						rendered, err := renderScriptIn(pos.script, language, refs, map[string]any{"v": v}, nil, nil, "run", nil, nil, nil, nil)
						if err != nil {
							t.Fatalf("value %q: render refused: %v", v, err)
						}
						got, pwned := runShellScript(t, interpreter, rendered)
						if pwned {
							t.Fatalf("value %q ran a command:\n%s", v, rendered)
						}
						if strings.Contains(got, "leak-") {
							t.Fatalf("value %q read the environment: %q\n%s", v, got, rendered)
						}
						want := pos.want(t, v)
						if strings.Contains(want, "HOMEDIR") {
							got = strings.Replace(got, `"home": "`+homeOf(got)+`"`, `"home": "HOMEDIR"`, 1)
						}
						if got != want {
							t.Fatalf("value %q arrived as %q, want %q\nrendered:\n%s", v, got, want, rendered)
						}
					}
				})
			}
		}
	}
}

// homeOf is the $HOME the unquoted-heredoc position printed: the author's
// own `$HOME` there still expands, which is what an unquoted heredoc is for.
func homeOf(out string) string {
	_, rest, ok := strings.Cut(out, `"home": "`)
	if !ok {
		return ""
	}
	home, _, _ := strings.Cut(rest, `"`)
	return home
}

// TestShellScriptDataRendersPerQuoting pins the rendered text of each
// position and value shape: an unquoted word reads a reference as a
// `command:` does (shellEscapeValue, by declaration), a double-quoted string
// gets the value's text between a closing and a reopening quote, single
// quotes and heredoc bodies get the JSON literal they always received.
func TestShellScriptDataRendersPerQuoting(t *testing.T) {
	t.Parallel()
	shapes := (&Shapes{}).WithInputSchema(&ir.Schema{Fields: []*ir.SchemaField{
		{Name: "files", Type: ir.FieldTypeStringArray},
		{Name: "doc", Type: ir.FieldTypeJSON},
	}})
	input := map[string]any{
		"s":     "it's",
		"n":     float64(3),
		"b":     true,
		"m":     map[string]any{"k": "v"},
		"l":     []any{"a", "b"},
		"files": []any{"a.go", "b.go"},
		"doc":   []any{"a", "b"},
	}
	cases := []struct {
		script, want string
	}{
		{"echo {{input.s}}", `echo 'it'\''s'`},
		{"echo {{input.n}} {{input.b}}", `echo '3' 'true'`},
		{"echo {{input.missing}}", `echo 'null'`},
		{"echo {{input.m}}", `echo '{"k":"v"}'`},
		{"echo {{input.l}}", `echo 'a' 'b'`},
		{"echo {{input.files}}", `echo 'a.go' 'b.go'`},
		{"echo {{input.doc}}", `echo '["a","b"]'`},
		{`echo "x={{input.s}}"`, `echo "x="'it'\''s'""`},
		{`echo "{{input.m}} {{input.missing}}"`, `echo ""'{"k":"v"}'" "'null'""`},
		{`echo "{{input.files}}"`, `echo ""'["a.go","b.go"]'""`},
		{"echo 'x = {{input.s}}'", `echo 'x = ''"it'\''s"'''`},
		{"echo '{{input.m}} {{input.n}}'", `echo '''{"k":"v"}'' ''3'''`},
		{"cat <<EOF\n{{input.s}} {{input.m}}\nEOF", "cat <<EOF\n\"it's\" {\"k\":\"v\"}\nEOF"},
		{"cat <<'EOF'\n{{input.n}}\nEOF", "cat <<'EOF'\n3\nEOF"},
		{"true # {{input.s}}", `true # "it's"`},
	}
	for _, c := range cases {
		got, err := renderScriptIn(c.script, "sh", mustRefs(c.script), input, nil, nil, "run", nil, shapes, nil, nil)
		if err != nil {
			t.Fatalf("%q: %v", c.script, err)
		}
		if got != c.want {
			t.Errorf("%q rendered\n%s\nwant\n%s", c.script, got, c.want)
		}
	}
	// A heredoc body escapes what it reads, and only that.
	got, err := renderScriptIn("cat <<EOF\n{{input.v}}\nEOF", "bash", mustRefs("cat <<EOF\n{{input.v}}\nEOF"), map[string]any{"v": "$(x) `y` \\"}, nil, nil, "run", nil, nil, nil, nil)
	if err != nil || got != "cat <<EOF\n\"\\$(x) \\`y\\` \\\\\\\\\"\nEOF" {
		t.Errorf("unquoted heredoc rendered %q (err %v)", got, err)
	}
}

// TestShellScriptKeepsTheAuthorsOwnText: what is not data keeps the
// rendering every script body gives it — the raw form is the author's text
// (in any position, refused ones included), a secret reference its
// placeholder's JSON literal, the literal open delimiter its braces — and a
// js/py body is not read as a shell at all.
func TestShellScriptKeepsTheAuthorsOwnText(t *testing.T) {
	t.Parallel()
	script := `echo {{!input.cmd}} $(( {{!input.n}} + 1 )) {{secrets.tok}} {{"{{"}} "{{secrets.tok}}"`
	got, err := renderScriptIn(script, "bash", mustRefs(script), map[string]any{"cmd": "a | b", "n": 2}, nil, nil, "run", nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ph := secretRefText(nil, "tok")
	if want := `echo a | b $(( 2 + 1 )) "` + ph + `" {{ ""` + ph + `""`; got != want {
		t.Fatalf("rendered\n%s\nwant\n%s", got, want)
	}
	for _, language := range []string{"js", "py"} {
		body := "x = {{input.v}}; y = $(( {{input.v}} ))"
		got, err := renderScriptIn(body, language, mustRefs(body), map[string]any{"v": "$(a)"}, nil, nil, "run", nil, nil, nil, nil)
		if err != nil || got != `x = "$(a)"; y = $(( "$(a)" ))` {
			t.Fatalf("%s rendered %q (err %v), want the JSON literals", language, got, err)
		}
	}
}

// TestShellScriptSlotContexts pins the reading of each reference's position
// — the quoting the shell reads it in, or why it is refused — over the
// constructs a shell script writes.
func TestShellScriptSlotContexts(t *testing.T) {
	t.Parallel()
	const (
		W = slotWord
		D = slotDouble
		S = slotSingle
		H = slotHeredoc
		L = slotLiteral
		C = slotComment
		R = slotRefused
	)
	cases := []struct {
		name   string
		script string
		sh     []shellSlotContext
		bash   []shellSlotContext // nil: as sh
	}{
		{"argument", "echo {{input.a}}", []shellSlotContext{W}, nil},
		{"assignment", "X={{input.a}}", []shellSlotContext{W}, nil},
		{"double quotes", `echo "pre {{input.a}} post" "{{input.b}}"`, []shellSlotContext{D, D}, nil},
		{"single quotes", `echo '{{input.a}}' x'{{input.b}}'`, []shellSlotContext{S, S}, nil},
		{"an escaped quote in double quotes", `echo "a\"{{input.a}}" {{input.b}}`, []shellSlotContext{D, W}, nil},
		{"an escaped backslash before a reference", `echo "\\{{input.a}}" \\{{input.b}}`, []shellSlotContext{D, W}, nil},
		{"a substitution in double quotes", `echo "$(printf %s {{input.a}} "{{input.b}}")" {{input.c}}`, []shellSlotContext{W, D, W}, nil},
		{"nested parentheses in a substitution", "x=$( (cd /; echo {{input.a}}) ); echo \"{{input.b}}\"", []shellSlotContext{W, D}, nil},
		{"process substitution", `diff <(printf %s {{input.a}}) <(echo "{{input.b}}")`, []shellSlotContext{W, D}, nil},
		{"array and function", "a=( {{input.a}} \"{{input.b}}\" )\nf() { echo {{input.c}}; }", []shellSlotContext{W, D, W}, nil},
		{"a comment", "echo a # {{input.a}} ' \"\necho {{input.b}}", []shellSlotContext{C, W}, nil},
		{"a hash inside a word", "echo a#{{input.a}} $#{{input.b}}", []shellSlotContext{W, W}, nil},
		{"a comment after a redirection", "echo a >#{{input.a}}\necho {{input.b}}", []shellSlotContext{C, W}, nil},
		{"a comment in a substitution", "x=$(echo a # ) {{input.a}}\n)\necho {{input.b}}", []shellSlotContext{C, W}, nil},
		{"a line continuation", "echo a \\\n{{input.a}}", []shellSlotContext{W}, nil},
		{"a here-string", "cat <<<{{input.a}}", []shellSlotContext{W}, nil},
		{"an unquoted heredoc", "cat <<EOF\n\"{{input.a}}\" '{{input.b}}'\nEOF\necho {{input.c}}", []shellSlotContext{H, H, W}, nil},
		{"a stripped heredoc", "cat <<-EOF\n\t{{input.a}}\n\tEOF\necho {{input.b}}", []shellSlotContext{H, W}, nil},
		{"a single-quoted delimiter", "cat <<'EOF'\n{{input.a}}\nEOF\necho {{input.b}}", []shellSlotContext{L, W}, nil},
		{"a double-quoted delimiter", "cat << \"E F\"\n{{input.a}}\nE F\necho {{input.b}}", []shellSlotContext{L, W}, nil},
		{"a backslash-quoted delimiter", "cat <<\\EOF\n{{input.a}}\nEOF\necho {{input.b}}", []shellSlotContext{L, W}, nil},
		{"a partly quoted delimiter", "cat <<E'OF'\n{{input.a}}\nEOF\necho {{input.b}}", []shellSlotContext{L, W}, nil},
		{"a delimiter's escapes in double quotes", "cat <<\"E\\OF\"\nEOF\n{{input.a}}\nE\\OF\necho {{input.b}}", []shellSlotContext{L, W}, nil},
		{"two heredocs on a line", "cat <<A - <<'B' {{input.z}}\n{{input.a}}\nA\n{{input.b}}\nB\necho {{input.c}}", []shellSlotContext{W, H, L, W}, nil},
		{"a heredoc in a substitution", "x=$(cat <<EOF\n{{input.a}}\nEOF\n)\necho {{input.b}}", []shellSlotContext{H, W}, nil},
		{"a substitution in a heredoc", "cat <<EOF\n$(printf %s {{input.a}} \"{{input.b}}\" '{{input.c}}') {{input.d}}\nEOF", []shellSlotContext{R, R, S, H}, nil},
		{"a word on a heredoc's operator line", "cat <<EOF - {{input.a}} \"{{input.b}}\"\nbody\nEOF", []shellSlotContext{W, D}, nil},
		{"arithmetic", "echo $(( {{input.a}} + 1 )) {{input.b}}", []shellSlotContext{R, W}, nil},
		{"an arithmetic command", "(( x = {{input.a}} ))\necho {{input.b}}", []shellSlotContext{R, W}, nil},
		{"arithmetic in a heredoc", "cat <<EOF\n$(( {{input.a}} ))\nEOF", []shellSlotContext{R}, nil},
		{"a parameter expansion", "echo ${X:-{{input.a}}} \"${X:-{{input.b}}}\" {{input.c}}", []shellSlotContext{R, R, W}, nil},
		{"a backquote substitution", "echo `printf %s {{input.a}}` \"`echo {{input.b}}`\" {{input.c}}", []shellSlotContext{R, R, W}, nil},
		{"glued to a dollar", "echo ${{input.a}} \"${{input.b}}\" {{input.c}}", []shellSlotContext{R, R, W}, nil},
		{"glued to a backslash", "echo \\{{input.a}} \"\\{{input.b}}\" {{input.c}}", []shellSlotContext{R, R, W}, nil},
		{"a locale string", "echo $\"pre {{input.a}}\"", []shellSlotContext{D}, nil},
		{"an ANSI-C string", "echo $'{{input.a}}' {{input.b}}", []shellSlotContext{R, W}, nil},
		{"an ANSI-C escape before data", "x=$'a\\'b'; echo {{input.a}}", []shellSlotContext{R}, []shellSlotContext{W}},
		{"a case at the top", "case {{input.a}} in a) echo {{input.b}};; esac", []shellSlotContext{W, W}, nil},
		{"a case inside a substitution", "x=$(case a in a) echo;; esac); echo {{input.a}}", []shellSlotContext{R}, nil},
		{"a heredoc that never ends", "echo {{input.z}}\ncat <<EOF\n{{input.a}}", []shellSlotContext{W, R}, nil},
		{"a heredoc line continued", "cat <<EOF\na\\\nEOF\n{{input.a}}\nEOF", []shellSlotContext{R}, nil},
		{"a reference as a delimiter", "cat <<{{input.a}}\nx\n{{input.b}}", []shellSlotContext{R, R}, nil},
		{"an escaped reference as a delimiter", "cat <<\\{{input.a}}\nx\n{{input.b}}", []shellSlotContext{R, R}, nil},
		{"escaped references in backquotes and $'…'", "echo `a\\{{input.a}}` $'\\{{input.b}}' {{input.c}}", []shellSlotContext{R, R, R}, []shellSlotContext{R, R, W}},
		{"a delimiter a value could spell", "cat <<'E1'\n{{input.a}}\nE1\ncat <<null\n{{input.b}}\nnull\necho {{input.c}}", []shellSlotContext{R, R, W}, nil},
		{"a substitution left open in a heredoc", "cat <<EOF\n$(echo\nEOF\n)\necho {{input.a}}", []shellSlotContext{R}, nil},
		{"a single quote in a quoted expansion", "echo \"${X:-it's}\" {{input.a}}", []shellSlotContext{R}, nil},
		{"a brace inside an expansion", "echo ${X:-{a}} {{input.a}}", []shellSlotContext{R}, nil},
		{"a reference across lines", "echo {{input.a}} {{input.\nb}}", []shellSlotContext{R, R}, nil},
		{"a legacy arithmetic", "echo $[1] {{input.a}}", []shellSlotContext{R}, nil},
	}
	for _, c := range cases {
		for _, bash := range []bool{false, true} {
			want := c.sh
			if bash && c.bash != nil {
				want = c.bash
			}
			slots := scanShellSlots(c.script, bash)
			var got []shellSlotContext
			for _, span := range templateSpans(c.script) {
				got = append(got, slots[span[0]].ctx)
			}
			if len(got) != len(want) {
				t.Errorf("%s (bash=%v): %d references read, want %d", c.name, bash, len(got), len(want))
				continue
			}
			for i := range want {
				if got[i] != want[i] {
					t.Errorf("%s (bash=%v): reference %d read as %d, want %d (%q)", c.name, bash, i, got[i], want[i], slots[templateSpans(c.script)[i][0]].reason)
				}
			}
		}
	}
}

// TestShellScriptRefusesDataWhereNoQuotingHolds: a refused position fails
// the render, naming every reference it refused and why, whatever the
// value — the text returned keeps them as written — and the executor
// refuses the node before the script exists, so nothing runs.
func TestShellScriptRefusesDataWhereNoQuotingHolds(t *testing.T) {
	t.Parallel()
	script := "echo ok > ran.txt\necho $(( {{input.n}} + 1 )) `echo {{input.v}}` {{input.v}}"
	got, err := renderScriptIn(script, "sh", mustRefs(script), map[string]any{"n": 1, "v": "x"}, nil, nil, "run", nil, nil, nil, nil)
	if err == nil {
		t.Fatalf("rendered %q, want a refusal", got)
	}
	for _, want := range []string{"{{input.n}} stands inside arithmetic", "{{input.v}} stands inside a backquote substitution"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not say %q", err, want)
		}
	}
	if want := "echo ok > ran.txt\necho $(( {{input.n}} + 1 )) `echo {{input.v}}` 'x'"; got != want {
		t.Errorf("refused render returned %q, want %q", got, want)
	}

	dir := t.TempDir()
	e := newTestClawExecutor(NewRegistry(), &ir.Workflow{}, WithToolPolicy(tool.OpenPolicy()), WithWorkDir(dir))
	node := &ir.ToolNode{BaseNode: ir.BaseNode{ID: "gate"}, Language: "sh", Script: script, ScriptRefs: mustRefs(script)}
	_, err = e.Execute(context.Background(), node, map[string]any{"n": 1, "v": "x"})
	if err == nil || !strings.Contains(err.Error(), "stands inside arithmetic") {
		t.Fatalf("Execute = %v, want the refusal", err)
	}
	if _, serr := os.Stat(filepath.Join(dir, "ran.txt")); !errors.Is(serr, os.ErrNotExist) {
		t.Fatalf("the refused script ran (ran.txt: %v)", serr)
	}
}

// TestShellScriptDataThroughTheExecutor is the wiring half of the guarantee:
// the executor's script recipe renders a sh and a bash body with this
// reading, so a hostile value reaches the file the script writes byte for
// byte and runs nothing.
func TestShellScriptDataThroughTheExecutor(t *testing.T) {
	t.Parallel()
	value := "'$(touch pwned)'\"`touch pwned`\" $HOME ${PATH}\n\\"
	// The node's declarations reach its words as a command's do: a `json`
	// field is one token of JSON text, where an undeclared list would be
	// one word per element.
	wf := &ir.Workflow{Schemas: map[string]*ir.Schema{"in": {Name: "in", Fields: []*ir.SchemaField{
		{Name: "v", Type: ir.FieldTypeString},
		{Name: "doc", Type: ir.FieldTypeJSON},
	}}}}
	for _, language := range []string{"sh", "bash"} {
		dir := t.TempDir()
		e := newTestClawExecutor(NewRegistry(), wf, WithToolPolicy(tool.OpenPolicy()), WithWorkDir(dir))
		script := "printf '%s' {{input.v}} > word.txt\nprintf '%s' \"[{{input.v}}]\" > double.txt\nprintf '%s' {{input.doc}} > doc.txt\necho done"
		node := &ir.ToolNode{BaseNode: ir.BaseNode{ID: "gate"}, Language: language, Script: script, ScriptRefs: mustRefs(script), SchemaFields: ir.SchemaFields{InputSchema: "in"}}
		if _, err := e.Execute(context.Background(), node, map[string]any{"v": value, "doc": []any{"a", "b"}}); err != nil {
			t.Fatalf("%s: Execute: %v", language, err)
		}
		for file, want := range map[string]string{"word.txt": value, "double.txt": "[" + value + "]", "doc.txt": `["a","b"]`} {
			got, err := os.ReadFile(filepath.Join(dir, file))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != want {
				t.Errorf("%s: %s = %q, want %q", language, file, got, want)
			}
		}
		if _, err := os.Stat(filepath.Join(dir, "pwned")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: a value ran a command (pwned: %v)", language, err)
		}
	}
}

// TestShellScriptDataCannotReadASecretsValue is the class #3291 names: a
// shell script holds the author's secret reference as a variable of the
// interpreter's environment, and data rendered beside it — the environment
// dumped, or bash enumerating the marker's prefix and reading the name it
// finds — arrives as the bytes it is and reads nothing, while the author's
// reference still receives the value.
func TestShellScriptDataCannotReadASecretsValue(t *testing.T) {
	t.Parallel()
	const value = "sv-3e81c0a9d27f6b54"
	body := "printf '%s|%s|%s' {{secrets.pw}} {{input.d}} \"[{{input.d}}]\" > out.txt\n"
	for _, language := range []string{"sh", "bash"} {
		for _, d := range []string{
			"$(env)",
			"`env`",
			"${x:=${!__ITERION_AUTHORED_@}}${!x}",
			"${!__ITERION_AUTHORED_@}",
			"$(touch pwned.txt)",
		} {
			got, pwned := runSecretScript(t, language, body, value, map[string]any{"d": d})
			if want := value + "|" + d + "|[" + d + "]"; got != want {
				t.Errorf("%s, data %q: the script wrote %q, want %q", language, d, got, want)
			}
			if pwned {
				t.Errorf("%s, data %q ran a command", language, d)
			}
		}
	}
}
