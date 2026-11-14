package model

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// scriptSecretValues are values a tenant may bind to a declared secret —
// whoever sets one is not the bot's author — each a shape a string literal
// of some script language reads as syntax: quotes, a backslash, a shell
// expansion, a command substitution, line breaks, non-ASCII, and the payloads
// that break out of a literal to run code in python, node and sh.
var scriptSecretValues = []string{
	`ab'cd-single`,
	`ab"cd-double`,
	`back\slash\n-and-A`,
	`dollar $HOME and ${PATH}`,
	"backtick `id` here",
	"multi\nline\ttab\r\n",
	"non-ascii é€   <&>",
	`a"+str(open("pwned.txt","w").write("1"))+"b`,
	`a"+require("fs").writeFileSync("pwned.txt","1")+"b`,
	`$(touch pwned.txt) and ` + "`touch pwned.txt`",
}

// scriptSecretBodies write the value the author's `{{secrets.pw}}` renders to
// out.txt, one body per interpreter a `script:` names.
var scriptSecretBodies = map[string]string{
	"sh":   "printf '%s' {{secrets.pw}} > out.txt\n",
	"bash": "printf '%s' {{secrets.pw}} > out.txt\n",
	"py":   "open('out.txt', 'w', encoding='utf-8', newline='').write({{secrets.pw}})\n",
	"js":   "require('fs').writeFileSync('out.txt', {{secrets.pw}})\n",
}

func requireInterpreter(t *testing.T, language string) {
	t.Helper()
	argv, _ := scriptInterpreter(language)
	if len(argv) == 0 {
		t.Fatalf("no interpreter for %q", language)
	}
	if _, err := exec.LookPath(argv[0]); err != nil {
		t.Skipf("%s not on PATH", argv[0])
	}
}

// A secret's value reaches a `script:` body as a string literal of the
// script's own language, byte for byte, whatever it holds: the author's
// `{{secrets.pw}}` renders a JSON string literal, and the value takes the
// whole literal's place in the language's syntax — never shell escaping
// inside a python string, never a quote that closes it.
func TestAScriptReceivesASecretValueByteExactAndRunsNothing(t *testing.T) {
	t.Parallel()
	for language, body := range scriptSecretBodies {
		for i, value := range scriptSecretValues {
			t.Run(language+"/"+strings.Repeat("v", i+1), func(t *testing.T) {
				t.Parallel()
				requireInterpreter(t, language)
				g := secretguard.New([]secretguard.Secret{{Name: "pw", Value: value}}, secretguard.DefaultConfig())
				refs, err := ir.ParseRefs(body)
				if err != nil {
					t.Fatal(err)
				}
				dir := t.TempDir()
				e := newTestClawExecutor(NewRegistry(), &ir.Workflow{}, WithWorkDir(dir), WithSecretGuard(g))
				node := &ir.ToolNode{BaseNode: ir.BaseNode{ID: "s"}, Script: body, ScriptRefs: refs, Language: language}
				if _, err := e.Execute(context.Background(), node, map[string]any{}); err != nil {
					t.Fatalf("value %q: the script failed: %v", value, err)
				}
				got, err := os.ReadFile(filepath.Join(dir, "out.txt"))
				if err != nil {
					t.Fatalf("value %q: the script wrote nothing: %v", value, err)
				}
				if string(got) != value {
					t.Errorf("the script received %q, want %q byte for byte", got, value)
				}
				if _, err := os.Stat(filepath.Join(dir, "pwned.txt")); err == nil {
					t.Errorf("value %q ran as code in the %s script", value, language)
				}
			})
		}
	}
}

// The raw `{{!secrets.pw}}` is the author's own text, and the author's
// quoting decides: escaped for the single-quoted word a shell script puts it
// in, as in a command; verbatim in python and node; and a raw reference the
// author double-quoted reads like the default form.
func TestARawSecretInAScriptIsTheAuthorsOwnText(t *testing.T) {
	t.Parallel()
	const value = `it's "quoted"`
	for language, c := range map[string]struct{ body, want string }{
		"sh":         {"printf '%s' '{{!secrets.pw}}' > out.txt\n", value},
		"py":         {"open('out.txt', 'w').write(r'''{{!secrets.pw}}''')\n", value},
		"js":         {"require('fs').writeFileSync('out.txt', String.raw`{{!secrets.pw}}`)\n", value},
		"py, quoted": {"open('out.txt', 'w').write(\"{{!secrets.pw}}\")\n", value},
	} {
		t.Run(language, func(t *testing.T) {
			t.Parallel()
			lang, _, _ := strings.Cut(language, ",")
			requireInterpreter(t, lang)
			g := secretguard.New([]secretguard.Secret{{Name: "pw", Value: value}}, secretguard.DefaultConfig())
			refs, err := ir.ParseRefs(c.body)
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			e := newTestClawExecutor(NewRegistry(), &ir.Workflow{}, WithWorkDir(dir), WithSecretGuard(g))
			node := &ir.ToolNode{BaseNode: ir.BaseNode{ID: "s"}, Script: c.body, ScriptRefs: refs, Language: lang}
			if _, err := e.Execute(context.Background(), node, map[string]any{}); err != nil {
				t.Fatalf("the script failed: %v", err)
			}
			got, err := os.ReadFile(filepath.Join(dir, "out.txt"))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != c.want {
				t.Errorf("the script received %q, want %q", got, c.want)
			}
		})
	}
}

// An author may wrap the reference in quoting of their own — a shell
// single-quoted word, a JSON document in single quotes, a python or node
// string, an f-string, a template literal, a regular expression. The value
// must not close or interpolate any of them: in a shell script it never
// enters the text (the literal becomes a variable read, the value rides the
// interpreter's environment), and in js and py every character that closes
// or interpolates a string is written as an escape. Each script still runs
// clean, and a JSON document stays one.
func TestASecretInAnAuthorQuotedScriptContextRunsNoCode(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		language, body string
		values         []string
		json           bool
	}{
		"sh, single quotes": {"sh", "printf '%s' '{{secrets.pw}}' > out.txt\n",
			[]string{`x'; touch pwned.txt; echo '`}, false},
		"sh, a JSON document in single quotes": {"sh", "printf '%s' '{\"t\": {{secrets.pw}}}' > out.txt\n",
			[]string{`x'; touch pwned.txt; echo '`, `ab$cd-dollar`}, true},
		"bash, single quotes": {"bash", "printf '%s' '{{secrets.pw}}' > out.txt\n",
			[]string{`x'; touch pwned.txt; echo '`}, false},
		"bash, a JSON document in single quotes": {"bash", "printf '%s' '{\"t\": {{secrets.pw}}}' > out.txt\n",
			[]string{`x'; touch pwned.txt; echo '`, `ab$cd-dollar`}, true},
		"py, single quotes": {"py", "open('out.txt', 'w').write('{{secrets.pw}}')\n",
			[]string{`x' + str(open('pwned.txt','w').write('1')) + '`}, false},
		"py, an f-string": {"py", "open('out.txt', 'w').write(f'{{secrets.pw}}')\n",
			[]string{`{__import__("os").system("touch pwned.txt")}`, `x' + str(open('pwned.txt','w').write('1')) + '`}, false},
		"py, a raw string": {"py", "open('out.txt', 'w').write(r'{{secrets.pw}}')\n",
			[]string{`x' + str(open('pwned.txt','w').write('1')) + '`}, false},
		"js, single quotes": {"js", "require('fs').writeFileSync('out.txt', '{{secrets.pw}}')\n",
			[]string{`x' + require('fs').writeFileSync('pwned.txt','1') + '`}, false},
		"js, a template literal": {"js", "require('fs').writeFileSync('out.txt', `{{secrets.pw}}`)\n",
			[]string{"${require(\"fs\").writeFileSync(\"pwned.txt\",\"1\")}", "x` + require('fs').writeFileSync('pwned.txt','1') + `"}, false},
		"js, a regular expression": {"js", "require('fs').writeFileSync('out.txt', /{{secrets.pw}}/.source)\n",
			[]string{`a/+require("fs").writeFileSync("pwned.txt","1")+/b`}, false},
	} {
		for i, value := range c.values {
			t.Run(name+"/"+strings.Repeat("v", i+1), func(t *testing.T) {
				t.Parallel()
				requireInterpreter(t, c.language)
				g := secretguard.New([]secretguard.Secret{{Name: "pw", Value: value}}, secretguard.DefaultConfig())
				refs, err := ir.ParseRefs(c.body)
				if err != nil {
					t.Fatal(err)
				}
				dir := t.TempDir()
				e := newTestClawExecutor(NewRegistry(), &ir.Workflow{}, WithWorkDir(dir), WithSecretGuard(g))
				node := &ir.ToolNode{BaseNode: ir.BaseNode{ID: "s"}, Script: c.body, ScriptRefs: refs, Language: c.language}
				if _, err := e.Execute(context.Background(), node, map[string]any{}); err != nil {
					t.Errorf("the script no longer parses or runs: %v", err)
				}
				if _, err := os.Stat(filepath.Join(dir, "pwned.txt")); err == nil {
					t.Fatalf("the value ran as code in the author's quoting")
				}
				got, err := os.ReadFile(filepath.Join(dir, "out.txt"))
				if err != nil {
					t.Fatalf("the script wrote nothing: %v", err)
				}
				if c.json && !json.Valid(got) {
					t.Errorf("the JSON document is no longer one: %q", got)
				}
			})
		}
	}
}

// A value no form carries as written is refused, naming the reference and
// never the value: invalid UTF-8 in a js or py string literal (JSON would
// rewrite it into U+FFFD), a NUL byte in a shell script (neither its
// environment nor its text can hold one).
func TestAScriptRefusesAValueItCannotCarryAsWritten(t *testing.T) {
	t.Parallel()
	ph := secretguard.PlaceholderForName("pw")
	for name, c := range map[string]struct{ language, body, value string }{
		"py, invalid UTF-8": {"py", "open('out.txt', 'w').write({{secrets.pw}})\n", "ab\xff\xfecd-xyz"},
		"js, invalid UTF-8": {"js", "require('fs').writeFileSync('out.txt', {{secrets.pw}})\n", "ab\xff\xfecd-xyz"},
		"sh, a NUL byte":    {"sh", "printf '%s' {{secrets.pw}} > out.txt\n", "ab\x00cd-xyz"},
		"bash, a NUL byte":  {"bash", "printf '%s' {{secrets.pw}} > out.txt\n", "ab\x00cd-xyz"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			g := secretguard.New([]secretguard.Secret{{Name: "pw", Value: c.value}}, secretguard.DefaultConfig())
			refs, err := ir.ParseRefs(c.body)
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			e := newTestClawExecutor(NewRegistry(), &ir.Workflow{}, WithWorkDir(dir), WithSecretGuard(g))
			node := &ir.ToolNode{BaseNode: ir.BaseNode{ID: "s"}, Script: c.body, ScriptRefs: refs, Language: c.language}
			_, err = e.Execute(context.Background(), node, map[string]any{})
			if err == nil {
				t.Fatal("the script ran with a value it cannot carry as written")
			}
			if !strings.Contains(err.Error(), ph) {
				t.Errorf("the refusal does not name the reference %s: %v", ph, err)
			}
			if strings.Contains(err.Error(), "cd-xyz") {
				t.Errorf("the refusal quotes the value: %v", err)
			}
			if _, statErr := os.Stat(filepath.Join(dir, "out.txt")); statErr == nil {
				t.Error("the refused script ran")
			}
		})
	}
}

// runSecretScript runs body as a `script:` in language under a guard holding
// pw = value, with input, and returns what the script wrote to out.txt and
// whether it created pwned.txt.
func runSecretScript(t *testing.T, language, body, value string, input map[string]any) (string, bool) {
	t.Helper()
	requireInterpreter(t, language)
	g := secretguard.New([]secretguard.Secret{{Name: "pw", Value: value}}, secretguard.DefaultConfig())
	refs, err := ir.ParseRefs(body)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	e := newTestClawExecutor(NewRegistry(), &ir.Workflow{}, WithWorkDir(dir), WithSecretGuard(g))
	node := &ir.ToolNode{BaseNode: ir.BaseNode{ID: "s"}, Script: body, ScriptRefs: refs, Language: language}
	if _, err := e.Execute(context.Background(), node, input); err != nil {
		t.Fatalf("the script failed: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "out.txt"))
	if err != nil {
		t.Fatalf("the script wrote nothing: %v", err)
	}
	_, perr := os.Stat(filepath.Join(dir, "pwned.txt"))
	return string(got), perr == nil
}

// A shell script reads the value from a variable the engine names after the
// rendering's marker, which no data can spell: data rendered into the same
// script that names the reference's placeholder as a variable — directly,
// braced, or through bash's indirection — reads nothing, while the author's
// own reference still receives the value.
func TestDataInAShellScriptCannotReadTheSecretsVariable(t *testing.T) {
	t.Parallel()
	const value = "pv-5b2e9c0d7f13a468"
	ph := secretguard.PlaceholderForName("pw")
	body := "printf '%s|%s' {{secrets.pw}} {{input.d}} > out.txt\n"
	for name, c := range map[string]struct{ language, d string }{
		"sh, the placeholder as a variable":   {"sh", "$" + ph},
		"sh, braced":                          {"sh", "${" + ph + "}"},
		"bash, the placeholder as a variable": {"bash", "$" + ph},
		"bash, through indirection":           {"bash", "${x:=__ITERION_SECRET_pw}${y:=${x}__}${!y}"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, _ := runSecretScript(t, c.language, body, value, map[string]any{"d": c.d})
			own, data, _ := strings.Cut(got, "|")
			if own != value {
				t.Errorf("the author's reference received %q, want the value", own)
			}
			if strings.Contains(data, value) {
				t.Errorf("data read the secret's variable: %q", data)
			}
		})
	}
}

// Where the shell expands, a shell script's reference receives the value —
// as an expression, in the author's double quotes, in a JSON document whose
// single quotes the author closes around it, in an unquoted heredoc (which
// keeps the reference's own quotes). Single quotes and a quoted heredoc
// expand nothing; there the script reads the variable's name, and C342 says
// so at authoring time (TestC342…).
func TestAShellScriptReceivesTheValueWhereTheShellExpands(t *testing.T) {
	t.Parallel()
	const value = `tok-AbC123 x;y`
	for _, language := range []string{"sh", "bash"} {
		for name, c := range map[string]struct{ body, want string }{
			"an expression":       {"printf '%s' {{secrets.pw}} > out.txt\n", value},
			"double quotes":       {"printf '%s' \"{{secrets.pw}}\" > out.txt\n", value},
			"a JSON document":     {"printf '%s' '{\"t\": \"'{{secrets.pw}}'\"}' > out.txt\n", `{"t": "` + value + `"}`},
			"an unquoted heredoc": {"cat > out.txt <<EOF\n{\"t\": {{secrets.pw}}}\nEOF\n", `{"t": "` + value + `"}` + "\n"},
		} {
			t.Run(language+"/"+name, func(t *testing.T) {
				t.Parallel()
				got, pwned := runSecretScript(t, language, c.body, value, map[string]any{})
				if got != c.want || pwned {
					t.Errorf("the script received %q (pwned %v), want %q", got, pwned, c.want)
				}
			})
		}
	}
}

// The author's double quotes around the default form — `"{{secrets.pw}}"`,
// an f-string's or a template's — are the one literal the author meant: the
// value fills it, byte for byte, and a crafted value runs as no code.
func TestADoubleQuotedSecretIsOneLiteral(t *testing.T) {
	t.Parallel()
	pyPayload := `+str(open(chr(112)+chr(119)+chr(110)+chr(101)+chr(100)+chr(46)+chr(116)+chr(120)+chr(116),chr(119)).write(chr(49)))+`
	jsPayload := `+require(String.fromCharCode(102,115)).writeFileSync(String.fromCharCode(112,119,110,101,100,46,116,120,116),String.fromCharCode(49))+`
	for name, c := range map[string]struct{ language, body string }{
		"py, double quotes": {"py", "open('out.txt', 'w', encoding='utf-8', newline='').write(\"{{secrets.pw}}\")\n"},
		"py, an f-string":   {"py", "open('out.txt', 'w', encoding='utf-8', newline='').write(f\"{{secrets.pw}}\")\n"},
		"js, double quotes": {"js", "require('fs').writeFileSync('out.txt', \"{{secrets.pw}}\")\n"},
		"sh, double quotes": {"sh", "printf '%s' \"{{secrets.pw}}\" > out.txt\n"},
	} {
		for _, value := range []string{pyPayload, jsPayload, `it's "quoted" {x} ${y} \ok`, "a;touch pwned.txt"} {
			t.Run(name+"/"+value[:5], func(t *testing.T) {
				t.Parallel()
				got, pwned := runSecretScript(t, c.language, c.body, value, map[string]any{})
				if pwned {
					t.Fatalf("value %q ran as code", value)
				}
				if got != value {
					t.Errorf("the script received %q, want %q", got, value)
				}
			})
		}
	}
}
