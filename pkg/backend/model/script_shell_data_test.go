package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/secretsink"
)

// hostileShellValues are data a model or a webhook could hand a shell
// script: every expansion the shell has, the quotes, the escapes, the bytes
// a JSON literal would cook, the text that ends a heredoc or a comment, and
// the reads of the interpreter's environment the secret path cares about
// (#3291).
var hostileShellValues = []string{
	"", "plain", "two  words",
	"$(touch pwned)", "`touch pwned`", "$HOME", "${HOME}", "${!ITERION_@}",
	"${x:=${!__ITERION_AUTHORED_@}}${!x}", "$__ITERION_SECRET_x__", "$(env)", "${ITERION_CANARY}",
	"'", `"`, `it's "quoted"`, `'\''`, `'$(touch pwned)'"$(touch pwned)"`,
	`\`, `a\\b`, `\n`, `trailing\`,
	"line1\nline2", "\n", "a\n\n", "\nEOF\n$(touch pwned)\nEOF\n", "EOF", "EOF)", "x)\"; touch pwned; \"",
	"#;touch pwned", "x\n#\ntouch pwned", "\ntouch pwned #",
	"*", "?", "[a]", "*?[", "{a,b}", "~/x", "-n", "%s%n", "\t\r",
	"é ☃ 𝄞", "<>&", "\u2028\u2029", "$((1+1))", "$'\\x41'", "!!", "a[$(touch pwned)]",
	"@{[system(\"touch pwned\")]}", "#{system('touch pwned')}",
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
// dash, bash and `bash --posix`; a bash script to bash.
func shellsFor(t *testing.T, language string) [][]string {
	t.Helper()
	all := [][]string{{"bash"}}
	if language == "sh" {
		all = [][]string{{"dash"}, {"bash"}, {"bash", "--posix"}}
	}
	var found [][]string
	for _, argv := range all {
		if _, err := exec.LookPath(argv[0]); err == nil {
			found = append(found, argv)
		}
	}
	if !slices.ContainsFunc(found, func(a []string) bool { return a[0] == "bash" }) {
		t.Fatalf("no bash among %v", all)
	}
	return found
}

// renderAndRun renders script in language with input through the production
// renderer, hands the data as the executor does (scriptData.deliver: the
// environment, a prologue for what it cannot hold), runs it under argv in a
// scratch directory, and returns its stdout, whether a `pwned` file appeared
// and the rendered text.
func renderAndRun(t *testing.T, argv []string, language, script string, input map[string]any) (string, bool, string) {
	t.Helper()
	rendered, data, err := renderScriptIn(script, language, mustRefs(script), input, nil, nil, "run", nil, nil, nil)
	if err != nil {
		t.Fatalf("render %q: %v", script, err)
	}
	env, prologue, err := data.deliver()
	if err != nil {
		t.Fatalf("deliver %q: %v", script, err)
	}
	dir, home := t.TempDir(), t.TempDir()
	path := filepath.Join(dir, "script.sh")
	if err := os.WriteFile(path, []byte(prologue+rendered), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(argv[0], append(argv[1:], path)...)
	cmd.Dir = dir
	cmd.Env = shellCanaryEnv(home)
	for _, name := range slices.Sorted(maps.Keys(env)) {
		cmd.Env = append(cmd.Env, name+"="+env[name])
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	_ = cmd.Run() // a script may fail (a value that does not parse as a number); what it ran is what is judged
	pwned := false
	for _, d := range []string{dir, home} {
		if _, err := os.Stat(filepath.Join(d, "pwned")); err == nil {
			pwned = true
		}
	}
	return stdout.String(), pwned, rendered
}

func jsonText(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// dataRead matches the read of a data variable the renderer writes: the
// context-free `${N+"$N"}`, or a bare `${N}` (a heredoc's JSON variable, a
// read glued to a backslash).
var dataRead = regexp.MustCompile(`\$\{(__ITERION_DATA_[0-9a-f]{16}_[0-9]+(_JSON)?__)(\+"\$__ITERION_DATA_[0-9a-f]{16}_[0-9]+__")?\}`)

// readShape writes each read of s as `${R}` (the context-free form), `${J}`
// (a heredoc's JSON variable) or `${V}` (a bare read).
func readShape(s string) string {
	return dataRead.ReplaceAllStringFunc(s, func(m string) string {
		switch {
		case strings.Contains(m, "+"):
			return "${R}"
		case strings.HasSuffix(m, "_JSON__}"):
			return "${J}"
		}
		return "${V}"
	})
}

// shellPositions are the positions a shell script holds a reference at, each
// with what the script prints for value v: the value's bytes where the shell
// expands the read, its JSON literal in an unquoted heredoc body.
var shellPositions = []struct {
	name     string
	script   string
	bashOnly bool
	want     func(t *testing.T, v string) string
}{
	{"argument", "printf '%s' {{input.v}}", false, func(_ *testing.T, v string) string { return v }},
	{"assignment", "X={{input.v}}\nprintf '%s' \"$X\"", false, func(_ *testing.T, v string) string { return v }},
	{"inside a substitution", "X=$(printf '%s|' {{input.v}})\nprintf '%s' \"${X%|}\"", false, func(_ *testing.T, v string) string { return v }},
	{"inside backquotes", "X=`printf '%s|' {{input.v}}`\nprintf '%s' \"${X%|}\"", false, func(_ *testing.T, v string) string { return v }},
	{"a whole double-quoted string", "printf '%s' \"{{input.v}}\"", false, func(_ *testing.T, v string) string { return v }},
	{"inside a double-quoted string", "printf '%s' \"<{{input.v}}> and {{input.v}}\"", false, func(_ *testing.T, v string) string { return "<" + v + "> and " + v }},
	{"quoted inside a substitution inside quotes", "X=\"$(printf '%s|' \"{{input.v}}\")\"\nprintf '%s' \"${X%|}\"", false, func(_ *testing.T, v string) string { return v }},
	{"a default word", "printf '%s' ${UNSET:-{{input.v}}} \"[${UNSET:-{{input.v}}}]\"", false, func(_ *testing.T, v string) string { return v + "[" + v + "]" }},
	{"a here-string", "cat <<<{{input.v}}", true, func(_ *testing.T, v string) string { return v + "\n" }},
	{"single quotes run by sh -c", "sh -c 'printf \"%s\" {{input.v}}'", false, func(_ *testing.T, v string) string { return v }},
	{"single quotes run by eval", "eval 'printf \"%s\" {{input.v}}'", false, func(_ *testing.T, v string) string { return v }},
	{"single quotes run by a trap", "trap 'printf \"%s\" {{input.v}}' EXIT", false, func(_ *testing.T, v string) string { return v }},
	{"a quoted heredoc run by sh", "sh <<'EOF'\nprintf '%s' {{input.v}}\nEOF", false, func(_ *testing.T, v string) string { return v }},
	{"an unquoted heredoc", "cat <<EOF\n{\"v\": {{input.v}}, \"home\": \"$HOME\"}\nEOF", false, func(t *testing.T, v string) string { return `{"v": ` + jsonText(t, v) + `, "home": "HOMEDIR"}` + "\n" }},
	{"a stripped heredoc", "cat <<-EOF\n\t{{input.v}}\n\tEOF", false, func(t *testing.T, v string) string { return jsonText(t, v) + "\n" }},
	{"a heredoc in a substitution", "X=$(cat <<EOF\n{{input.v}}\nEOF\n)\nprintf '%s' \"$X\"", false, func(t *testing.T, v string) string { return jsonText(t, v) }},
	{"a comment", "printf 'x' # {{input.v}}\nprintf 'y'", false, func(_ *testing.T, _ string) string { return "xy" }},
}

// TestShellScriptDataIsInert is the guarantee of #3291: a data reference at
// every position a shell script holds one arrives as the bytes the position
// promises — the value, or its JSON literal in a heredoc body — and nothing
// in it runs or reads the interpreter's environment, under dash, bash and
// `bash --posix` (devbox's `sh`).
func TestShellScriptDataIsInert(t *testing.T) {
	t.Parallel()
	for _, language := range []string{"sh", "bash"} {
		for _, pos := range shellPositions {
			for _, argv := range shellsFor(t, language) {
				if pos.bashOnly && (argv[0] != "bash" || len(argv) > 1) {
					continue
				}
				t.Run(language+"/"+strings.Join(argv, " ")+"/"+pos.name, func(t *testing.T) {
					t.Parallel()
					for _, v := range hostileShellValues {
						got, pwned, rendered := renderAndRun(t, argv, language, pos.script, map[string]any{"v": v})
						if pwned {
							t.Fatalf("value %q ran a command:\n%s", v, rendered)
						}
						if strings.Contains(got, "leak-") {
							t.Fatalf("value %q read the environment: %q\n%s", v, got, rendered)
						}
						want := pos.want(t, v)
						if strings.Contains(want, "HOMEDIR") {
							got = regexp.MustCompile(`"home": "[^"]*"`).ReplaceAllString(got, `"home": "HOMEDIR"`)
						}
						if got != want {
							t.Fatalf("value %q arrived as %q, want %q\nrendered:\n%s", v, got, want, rendered)
						}
						if strings.Contains(rendered, "touch") || (len(v) > 3 && !strings.Contains(pos.script, v) && strings.Contains(rendered, v)) {
							t.Fatalf("value %q is in the script's text:\n%s", v, rendered)
						}
					}
				})
			}
		}
	}
}

// TestShellScriptDesyncsRunNothing re-runs every reading the adversarial
// review of the first fix (rva-sh91-1) found the engine's lexer and the
// shells to disagree on: whatever the lexer reads there, the script's text
// holds only reads of variables, so no value runs a command or reads the
// environment — the worst a misread costs is a split word or a value that
// does not arrive.
func TestShellScriptDesyncsRunNothing(t *testing.T) {
	t.Parallel()
	payloads := []any{"$(touch pwned)", "x)\"; touch pwned; \"", "\"; touch pwned; \"", "${ITERION_CANARY}", "${__ITERION_AUTHORED_m}",
		"\ntouch pwned #", "`touch pwned`", "@{[system(\"touch pwned\")]}", []any{"x", "sh", "-c", "touch pwned"}, []any{"x", "/proc/self/environ"}}
	rows := []struct{ id, language, script string }{
		{"F1 heredoc in $( ) ended by a delimiter-led line", "bash", "cfg=$(cat <<END\nENDPOINT={{input.v}}\nEND\n)\necho \"cfg=[$cfg]\""},
		{"F1 EOF) idiom, later data", "sh", "payload=$(cat <<'EOF'\n{\"t\": 1}\nEOF)\necho {{input.v}}\ncat <<'EOF'\nx\nEOF"},
		{"F1b-A newline inside $( ) with a heredoc pending", "sh", "cat <<EOF; x=$(printf %s\n{{input.v}}\n)\nbody\nEOF\necho \"x=$x\""},
		{"F1b-B heredoc in $( ) closed on its operator line", "sh", "x=$(cat <<EOF) ; echo \"x=[$x]\"\n{{input.v}}\nEOF\necho end"},
		{"F1b-C EOF) in a process substitution", "bash", "cat <(cat <<'EOF'\nhi\nEOF)\necho {{input.v}}\nEOF\n)"},
		{"F1b-D bash 5.3 ${ cmd; }", "bash", "x=\"${ echo }'; echo {{input.v}}; '; }\"\necho \"x=[$x]\""},
		{"F2 case split by a line continuation", "sh", "x=\"$(ca\\\nse a in a) echo '{{input.v}}';; esac)\"\necho \"$x\""},
		{"F3 a list at an assignment", "sh", "X={{input.v}}\necho \"$X\""},
		{"F3 a list in a here-string", "bash", "cat <<<{{input.v}}"},
		{"F5 # after a process substitution", "bash", "cat <(true)#{{input.v}} 2>/dev/null; echo done"},
		{"F5 # after an extglob group", "bash", "[[ a == @(a|b)#{{input.v}} ]]; echo done"},
		{"F5b ((…)) then #", "sh", "((true))#{{input.v}}\necho end"},
		{"F6 alias of case", "sh", "alias c=case\nx=\"$(c a in a) echo '{{input.v}}';; esac)\"\necho \"$x\""},
		{"F7 trap body in single quotes", "sh", "trap 'echo {{input.v}}' EXIT\necho body"},
		{"F7 sh -c", "sh", "sh -c 'echo {{input.v}}'"},
		{"F7 eval in single quotes", "sh", "eval 'echo {{input.v}}'"},
		{"F7 perl -e", "sh", "perl -e 'print {{input.v}}' 2>/dev/null; echo"},
		{"F7 ruby -e", "sh", "ruby -e 'puts {{input.v}}' 2>/dev/null; echo"},
		{"F8 << split by a line continuation", "sh", "cat <\\\n<EOF\n{{input.v}}\nEOF\necho end"},
		{"F8 $( split by a line continuation", "sh", "x=\"$\\\n(echo '{{input.v}}')\"\necho \"$x\""},
		{"F8 ${ split by a line continuation", "sh", "x=\"$\\\n{X:-'{{input.v}}'}\"\necho \"$x\""},
		{"glued to a backslash", "sh", "echo \\{{input.v}} \"\\{{input.v}}\" end"},
		{"glued to a dollar", "sh", "echo ${{input.v}} \"${{input.v}}\" end"},
		{"a reference as a heredoc delimiter", "sh", "cat <<{{input.v}}\nx\n{{input.v}}\necho end"},
	}
	for _, r := range rows {
		for _, argv := range shellsFor(t, r.language) {
			t.Run(r.id+"/"+strings.Join(argv, " "), func(t *testing.T) {
				t.Parallel()
				for _, v := range payloads {
					got, pwned, rendered := renderAndRun(t, argv, r.language, r.script, map[string]any{"v": v})
					if pwned {
						t.Fatalf("value %v ran a command:\n%s", v, rendered)
					}
					if strings.Contains(got, "leak-") {
						t.Fatalf("value %v read the environment: %q\n%s", v, got, rendered)
					}
				}
			})
		}
	}
}

// TestShellScriptDataRendersReads pins what the script's text holds: a read
// of a variable per data reference, written for its quoting — `"${…}"` as a
// word, `${…}` inside double quotes, arithmetic or after a backslash,
// `${…_JSON}` in a heredoc body — and the value the environment carries: the
// raw form's text, ONE value for a list, `null` for a hole.
func TestShellScriptDataRendersReads(t *testing.T) {
	t.Parallel()
	input := map[string]any{"s": "it's", "n": float64(3), "m": map[string]any{"k": "v"}, "l": []any{"a", "b"}}
	script := "echo {{input.s}} \"x={{input.s}}\" $(( {{input.n}} + 1 )) \\{{input.s}} '{{input.s}}'\ncat <<EOF\n{{input.m}}\nEOF\nX={{input.l}} Y={{input.missing}}"
	got, data, err := renderScriptIn(script, "bash", mustRefs(script), input, nil, nil, "run", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	shape := readShape(got)
	if want := "echo ${R} \"x=${R}\" $(( ${R} + 1 )) \\${V} '${R}'\ncat <<EOF\n${J}\nEOF\nX=${R} Y=${R}"; shape != want {
		t.Errorf("rendered\n%s\nshape\n%s\nwant\n%s", got, shape, want)
	}
	var values []string
	for _, name := range data.names {
		values = append(values, data.values[name])
	}
	if want := []string{"it's", "3", `{"k":"v"}`, `["a","b"]`, "null"}; !slices.Equal(values, want) {
		t.Errorf("the environment carries %q, want %q", values, want)
	}
	for _, v := range values {
		if strings.Contains(got, v) && v != "3" {
			t.Errorf("the value %q is in the script's text:\n%s", v, got)
		}
	}
	// Two renderings draw two markers: no name is predictable.
	again, _, _ := renderScriptIn(script, "bash", mustRefs(script), input, nil, nil, "run", nil, nil, nil)
	if dataRead.FindString(again) == dataRead.FindString(got) {
		t.Errorf("two renderings named the same variable %q", dataRead.FindString(got))
	}
}

// TestShellScriptDataDelivery: a value rides the environment while it fits
// the bound, past it the script opens with its single-quoted assignment —
// a 1 MiB value and the overflow of many arrive byte-exact through the
// executor — and a NUL byte, which neither can carry, is refused naming the
// reference.
func TestShellScriptDataDelivery(t *testing.T) {
	t.Parallel()
	small, big := strings.Repeat("s", 10), strings.Repeat("ab'\n$(touch pwned)", 1<<16)
	if len(big) < 1<<20 {
		t.Fatalf("the big value is %d bytes, want over 1 MiB", len(big))
	}
	var d scriptData
	d.set("A", "{{input.a}}", small)
	d.set("B", "{{input.b}}", big)
	for i := range 8 {
		d.set("C"+string(rune('0'+i)), "{{input.c}}", strings.Repeat("c", 60<<10))
	}
	env, prologue, err := d.deliver()
	if err != nil {
		t.Fatal(err)
	}
	if env["A"] != small || strings.Contains(prologue, "A=$(") {
		t.Errorf("a small value did not ride the environment")
	}
	if _, inEnv := env["B"]; inEnv || !strings.HasPrefix(prologue, "B=$(cat <<'__ITERION_DATA_EOF_") {
		t.Errorf("a 1 MiB value rode the environment")
	}
	// What the prologue carries is bounded: past it the script is refused,
	// naming the reference, not the value.
	var huge scriptData
	huge.set("H", "{{input.huge}}", strings.Repeat("h", dataSpillMax+1))
	if _, _, err := huge.deliver(); err == nil || !strings.Contains(err.Error(), "{{input.huge}}") || strings.Contains(err.Error(), "hhhh") {
		t.Errorf("a value past the spill bound delivered (%v), want a refusal naming the reference", err)
	}
	total := 0
	for name, v := range env {
		total += len(name) + len(v) + 2
	}
	if total > dataEnvTotalMax || len(env) == 9 {
		t.Errorf("the environment holds %d bytes over %d values, want at most %d", total, len(env), dataEnvTotalMax)
	}

	for _, language := range []string{"sh", "bash"} {
		dir := t.TempDir()
		e := newTestClawExecutor(NewRegistry(), &ir.Workflow{}, WithToolPolicy(tool.OpenPolicy()), WithWorkDir(dir))
		script := "printf '%s' {{input.big}} > big.txt\nprintf '%s' \"{{input.mid}}\" > mid.txt\nsh -c 'printf \"%s\" {{input.mid}}' > nested.txt\necho done"
		node := &ir.ToolNode{BaseNode: ir.BaseNode{ID: "gate"}, Language: language, Script: script, ScriptRefs: mustRefs(script)}
		mid := strings.Repeat("m", 1000)
		if _, err := e.Execute(context.Background(), node, map[string]any{"big": big, "mid": mid}); err != nil {
			t.Fatalf("%s: Execute: %v", language, err)
		}
		for file, want := range map[string]string{"big.txt": big, "mid.txt": mid, "nested.txt": mid} {
			got, err := os.ReadFile(filepath.Join(dir, file))
			if err != nil || string(got) != want {
				t.Errorf("%s: %s holds %d bytes (%v), want %d", language, file, len(got), err, len(want))
			}
		}
		if _, err := os.Stat(filepath.Join(dir, "pwned")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: a value ran a command", language)
		}
	}

	script := "echo {{input.v}}"
	if _, _, err := renderScriptIn(script, "sh", mustRefs(script), map[string]any{"v": "a\x00b"}, nil, nil, "run", nil, nil, nil); err == nil || !strings.Contains(err.Error(), "{{input.v}} holds a NUL byte") || strings.Contains(err.Error(), "a\x00b") {
		t.Errorf("a NUL byte rendered (%v), want a refusal naming the reference and not the value", err)
	}
}

// TestShellScriptKeepsTheAuthorsOwnText: what is not data keeps the
// rendering every script body gives it — the raw form is the author's text,
// a secret reference its placeholder's JSON literal, the literal open
// delimiter its braces — and a js/py body is not read as a shell at all.
func TestShellScriptKeepsTheAuthorsOwnText(t *testing.T) {
	t.Parallel()
	script := `echo {{!input.cmd}} $(( {{!input.n}} + 1 )) {{secrets.tok}} {{"{{"}} "{{secrets.tok}}"`
	got, data, err := renderScriptIn(script, "bash", mustRefs(script), map[string]any{"cmd": "a | b", "n": 2}, nil, nil, "run", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ph := secretRefText(nil, "tok")
	if want := `echo a | b $(( 2 + 1 )) "` + ph + `" {{ ""` + ph + `""`; got != want || len(data.names) != 0 {
		t.Fatalf("rendered\n%s\nwant\n%s (data %v)", got, want, data.names)
	}
	for _, language := range []string{"js", "py"} {
		body := "x = {{input.v}}; y = $(( {{input.v}} ))"
		got, data, err := renderScriptIn(body, language, mustRefs(body), map[string]any{"v": "$(a)"}, nil, nil, "run", nil, nil, nil)
		if err != nil || got != `x = "$(a)"; y = $(( "$(a)" ))` || len(data.names) != 0 {
			t.Fatalf("%s rendered %q (err %v, data %v), want the JSON literals", language, got, err, data.names)
		}
	}
}

// TestShellScriptDataThroughTheExecutor is the wiring half of the guarantee:
// the executor's script recipe renders a sh and a bash body with this
// reading and hands the interpreter the data's environment, so a hostile
// value — and a list, ONE value — reaches the file the script writes byte for
// byte and runs nothing.
func TestShellScriptDataThroughTheExecutor(t *testing.T) {
	t.Parallel()
	value := "'$(touch pwned)'\"`touch pwned`\" $HOME ${PATH}\n\\"
	for _, language := range []string{"sh", "bash"} {
		dir := t.TempDir()
		e := newTestClawExecutor(NewRegistry(), &ir.Workflow{}, WithToolPolicy(tool.OpenPolicy()), WithWorkDir(dir))
		script := "printf '%s' {{input.v}} > word.txt\nprintf '%s' \"[{{input.v}}]\" > double.txt\nprintf '%s' {{input.l}} > list.txt\ncat <<EOF > json.txt\n{{input.v}}\nEOF\necho done"
		node := &ir.ToolNode{BaseNode: ir.BaseNode{ID: "gate"}, Language: language, Script: script, ScriptRefs: mustRefs(script)}
		if _, err := e.Execute(context.Background(), node, map[string]any{"v": value, "l": []any{"a", "b c"}}); err != nil {
			t.Fatalf("%s: Execute: %v", language, err)
		}
		for file, want := range map[string]string{"word.txt": value, "double.txt": "[" + value + "]", "list.txt": `["a","b c"]`, "json.txt": jsonText(t, value) + "\n"} {
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
// interpreter's environment, and data beside it — the environment dumped,
// bash enumerating the marker's prefix, or the bash heredoc reading the
// review found the first fix's lexer to miss (`EOF)` inside `$( )`) — reads
// nothing, while the author's reference still receives the value.
func TestShellScriptDataCannotReadASecretsValue(t *testing.T) {
	t.Parallel()
	const value = "sv-3e81c0a9d27f6b54"
	for _, body := range []string{
		"printf '%s|%s|%s' {{secrets.pw}} {{input.d}} \"[{{input.d}}]\" > out.txt\n",
		"payload=$(cat <<'EOF'\n{\"t\": 1}\nEOF)\nprintf '%s|%s|%s' {{secrets.pw}} {{input.d}} \"[{{input.d}}]\" > out.txt\ncat <<'EOF'\nx\nEOF\n",
	} {
		for _, language := range []string{"sh", "bash"} {
			for _, d := range []string{"$(env)", "`env`", "${x:=${!__ITERION_AUTHORED_@}}${!x}", "${!__ITERION_AUTHORED_@}", "$(env | grep -o 'sv-[0-9a-f]*' | head -n1)", "$(touch pwned.txt)"} {
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
}

// A placeholder that reaches a shell script through data rides its
// environment like any value — and is the agent sink's there as in every
// body: a secret kept off the agent is defused in the value the script
// receives, so what the script sends on (a request the egress proxy
// substitutes) carries a token that resolves nowhere; one granted to the
// agent is left as the data spelled it.
func TestShellScriptDataPlaceholderIsDefusedInItsValue(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		sinks   []string
		defused bool
	}{
		{"kept off the agent", []string{secretsink.Command}, true},
		{"granted to the agent", []string{secretsink.Agent, secretsink.Command}, false},
	} {
		g, ph := sinkTestGuard(t, tc.sinks...)
		script := "printf '%s' {{input.body}} \"{{input.body}}\""
		text, data, err := renderScriptIn(script, "sh", mustRefs(script), map[string]any{"body": "tell " + ph}, nil, nil, "run", g, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(text, ph) {
			t.Fatalf("%s: the placeholder is in the script's text: %q", tc.name, text)
		}
		for _, name := range data.names {
			if v := data.values[name]; strings.Contains(v, ph) == tc.defused || strings.Contains(v, sinkTestValue) {
				t.Errorf("%s: the script receives %q (defused = %v wanted)", tc.name, v, tc.defused)
			}
		}
	}
}

// TestShellScriptMisreadsCostNothing is the class the verification review of
// the environment delivery found (rva-sh91-2 H2): wherever the reading and
// the shell part — dash ignoring bash's `EOF)`, a `case` spelled across a
// line continuation or through an alias, `$\<nl>(`, bash's `$((` falling
// back to `$( (` — the read the renderer wrote is the same context-free
// `${N+"$N"}`, one word unquoted and inside double quotes alike: a value at
// a position the shell takes as a command is one word with no argument, and
// nothing a value needs arguments or an expansion for runs or reads the
// environment. A command word the author writes runs the value — C345 warns.
func TestShellScriptMisreadsCostNothing(t *testing.T) {
	t.Parallel()
	rows := []struct{ id, language, script string }{
		{"F2 variant: case across a line continuation, data as the arm's command", "sh", "x=\"$(ca\\\nse a in a) {{input.v}};; esac)\"\necho \"x=[$x]\""},
		{"F6 variant: aliased case, data as the arm's command", "sh", "alias c=case\nx=\"$(c a in a) {{input.v}};; esac)\"\necho \"x=[$x]\""},
		{"F8 variant: $\\<nl>( in double quotes", "sh", "x=\"$\\\n(cd /; {{input.v}})\"\necho \"x=[$x]\""},
		{"dash: EOF) does not end a heredoc in $( )", "sh", "x=\"$(cat <<EOF\nEOF)\nEOF\n{{input.v}}\n)\"\necho \"x=[$x]\""},
		{"bash: $(( falls back to $( (", "bash", "x=$((cd /tmp; {{input.v}}) 2>&1)\necho \"x=[$x]\""},
		{"backquote stripping a backslash", "sh", "x=`echo \\\"{{input.v}}\\\"`\necho \"x=[$x]\""},
		{"read right: data as the arm's command", "sh", "x=\"$(case a in a) {{input.v}};; esac)\"\necho \"x=[$x]\""},
	}
	payloads := []any{"touch pwned", "sh -c touch${IFS}pwned", "cp /proc/self/environ leak.txt", "\ntouch pwned #", "touch\tpwned", "$(touch pwned)", "${ITERION_CANARY}"}
	for _, r := range rows {
		for _, argv := range shellsFor(t, r.language) {
			t.Run(r.id+"/"+strings.Join(argv, " "), func(t *testing.T) {
				t.Parallel()
				for _, v := range payloads {
					got, pwned, rendered := renderAndRun(t, argv, r.language, r.script, map[string]any{"v": v})
					if pwned || strings.Contains(got, "leak-") {
						t.Fatalf("value %q ran (pwned %v) or read the environment (%q):\n%s", v, pwned, got, rendered)
					}
				}
			})
		}
	}
	// Through the executor, beside a real secret: the F8 and `$((` rows.
	const value = "sv-3e81c0a9d27f6b54"
	for _, c := range []struct{ language, body string }{
		{"sh", "printf '%s' {{secrets.pw}} > /dev/null\nx=\"$\\\n(cd .; {{input.d}})\"\nprintf '%s' \"$x\" > out.txt\n"},
		{"bash", "printf '%s' {{secrets.pw}} > /dev/null\nx=$((cd .; {{input.d}}) 2>&1)\nprintf '%s' \"$x\" > out.txt\n"},
	} {
		for _, d := range []string{"cp /proc/self/environ pwned.txt", "sh -c env", "touch pwned.txt", "$(env)"} {
			got, pwned := runSecretScript(t, c.language, c.body, value, map[string]any{"d": d})
			if pwned || strings.Contains(got, value) {
				t.Errorf("%s, data %q: ran (pwned %v) or read the secret (out.txt %q)", c.language, d, pwned, got)
			}
		}
	}
}

// TestShellScriptRefusesAnEvaluatedValue: where the reading is sure bash
// evaluates the value — arithmetic, a variable name — the run refuses a value
// that is not an integer (an identifier), naming the reference and never the
// value, and an integer (an identifier) runs.
func TestShellScriptRefusesAnEvaluatedValue(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ script, good string }{
		{"echo $(( {{input.v}} + 1 ))", "-42"},
		{"[[ {{input.v}} -gt 0 ]] && echo pos", "7"},
		{"let n={{input.v}}+1", "1"},
		{"arr[{{input.v}}]=1", "0"},
		{"declare -i n\nn={{input.v}}", "3"},
		{"printf -v {{input.v}} '%s' x", "name_1"},
		{"[[ -v {{input.v}} ]] && echo set", "HOME"},
		{"read {{input.v}} <<< x", "line"},
	} {
		for _, bad := range []string{"a[$(touch pwned)]", "1+1", "", "x y"} {
			if _, _, err := renderScriptIn(c.script, "bash", mustRefs(c.script), map[string]any{"v": bad}, nil, nil, "run", nil, nil, nil); err == nil || !strings.Contains(err.Error(), "{{input.v}} is ") || (bad != "" && strings.Contains(err.Error(), bad)) {
				t.Errorf("%q with %q: rendered (%v), want a refusal naming the reference", c.script, bad, err)
			}
		}
		got, pwned, rendered := renderAndRun(t, []string{"bash"}, "bash", c.script, map[string]any{"v": c.good})
		if pwned || strings.Contains(got, "leak-") {
			t.Errorf("%q with %q misbehaved:\n%s", c.script, c.good, rendered)
		}
	}
}

// TestShellScriptSpillIsLinear: a spilled value is read from a heredoc, not
// spelled as one single-quoted word, which bash parses in time quadratic in
// its apostrophes — a 1 MiB value of quotes arrives byte for byte, quickly.
func TestShellScriptSpillIsLinear(t *testing.T) {
	t.Parallel()
	quotes := strings.Repeat("'", 1<<20)
	start := time.Now()
	got, pwned, _ := renderAndRun(t, []string{"bash"}, "bash", "printf '%s' {{input.v}}", map[string]any{"v": quotes})
	if pwned || got != quotes {
		t.Fatalf("a 1 MiB value of quotes arrived as %d bytes", len(got))
	}
	if took := time.Since(start); took > 20*time.Second {
		t.Errorf("a 1 MiB value of quotes took %v to arrive", took)
	}
}

// The self-repair rung shows a model the failed script, whose data reads name
// variables of its environment, and runs what the model answers as a
// command: that command keeps the reads, so it runs with the script's data —
// a repaired script still receives the value, not an empty string.
func TestASelfRepairedShellScriptKeepsItsData(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	reg := NewRegistry()
	client := &echoingRepairClient{}
	reg.Register("anthropic", func(string) (api.APIClient, error) { return client, nil })
	exec := newTestClawExecutor(reg, &ir.Workflow{}, WithWorkDir(dir))
	body := "printf '%s' {{input.answer}} > data.txt; printf '%s' {{input.big}} > big.txt; exit 1"
	node := &ir.ToolNode{
		BaseNode:      ir.BaseNode{ID: "publish"},
		Language:      "sh",
		Script:        body,
		ScriptRefs:    mustRefs(body),
		Goal:          "publish the answer",
		Postcondition: "test -f repaired.txt",
		Policy:        ir.PolicyRecover,
		Recovery:      &ir.RecoverySpec{MaxRepairAttempts: 1, Model: "anthropic/claude-test"},
	}
	answer, big := "the answer is $(touch pwned) 42", strings.Repeat("b", dataEnvValueMax+1)
	if _, err := exec.Execute(context.Background(), node, map[string]any{"answer": answer, "big": big}); err != nil {
		t.Fatalf("Execute: %v — the repaired command did not run", err)
	}
	if len(client.seen()) != 1 {
		t.Fatalf("the repair model was asked %d times, want once", len(client.seen()))
	}
	for file, want := range map[string]string{"data.txt": answer, "big.txt": big} {
		got, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil || string(got) != want {
			t.Errorf("%s after the repair holds %d bytes (%v), want the script's value (%d bytes)", file, len(got), err, len(want))
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "pwned")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a value ran a command")
	}
}
