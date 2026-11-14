package ir

import (
	"strings"
	"testing"
)

// quotedSecretSource declares a value secret, a file secret and a
// language-agnostic tool node whose body is the case's.
func quotedSecretSource(body string) string {
	return `dsl: 2

secrets:
  api_token:
    optional: true
  creds:
    as: file
    optional: true

schema out:
  ok: bool

` + body + `
workflow w:
  worktree: none
  sandbox: none
  entry: post
  post -> done
`
}

// C342: a shell script receives a value secret through a variable, so a
// default-form reference where the shell expands nothing — single quotes, a
// quoted heredoc — reads the variable's name, not the value. Each row is one
// position the check has to tell apart.
func TestC342ASecretWhereAShellScriptExpandsNothingWarns(t *testing.T) {
	t.Parallel()
	script := func(language, body string) string {
		return "tool post:\n  language: " + language + "\n  script: |\n    " + strings.ReplaceAll(body, "\n", "\n    ") + "\n  output: out\n"
	}
	for name, c := range map[string]struct {
		body string
		warn bool
	}{
		"single quotes":                    {script("sh", "printf '%s' '{{secrets.api_token}}'"), true},
		"a JSON document in single quotes": {script("bash", `printf '%s' '{"t": {{secrets.api_token}}}'`), true},
		"a quoted heredoc":                 {script("sh", "cat <<'EOF'\n{\"t\": {{secrets.api_token}}}\nEOF"), true},
		"an expression":                    {script("sh", "printf '%s' {{secrets.api_token}}"), false},
		"double quotes":                    {script("sh", `printf '%s' "{{secrets.api_token}}"`), false},
		"closed around the reference":      {script("sh", `printf '%s' '{"t": "'{{secrets.api_token}}'"}'`), false},
		"an unquoted heredoc":              {script("sh", "cat <<EOF\n{\"t\": {{secrets.api_token}}}\nEOF"), false},
		"the raw form in single quotes":    {script("sh", "printf '%s' '{{!secrets.api_token}}'"), false},
		"a file secret in single quotes":   {script("sh", "cat '{{secrets.creds}}'"), false},
		"a python string":                  {script("py", "print('{{secrets.api_token}}')"), false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			res := compileText(t, quotedSecretSource(c.body))
			var warns []Diagnostic
			for _, d := range res.Diagnostics {
				if d.Severity == SeverityError {
					t.Errorf("the fixture does not compile clean: %s", d.Error())
				}
				if d.Code == DiagSecretInShellLiteral {
					warns = append(warns, d)
				}
			}
			want := 0
			if c.warn {
				want = 1
			}
			if len(warns) != want {
				t.Fatalf("C342 fired %d time(s), want %d:\n%v", len(warns), want, res.Diagnostics)
			}
			if want == 0 {
				return
			}
			d := warns[0]
			if d.Severity != SeverityWarning || d.NodeID != "post" || d.Line == 0 || strings.TrimSpace(d.Hint) == "" {
				t.Errorf("C342 = %+v, want a positioned warning with a fix line", d)
			}
			if !strings.Contains(d.Message, "{{!secrets.api_token}}") {
				t.Errorf("the C342 message does not name the raw form:\n%s", d.Message)
			}
		})
	}
}
