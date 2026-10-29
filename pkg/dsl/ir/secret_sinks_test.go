package ir

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/secretsink"
)

// The compile-time sinks of ADR-146's table, one fixture each: the body that
// holds `{{secrets.tok}}` at that sink. The two sinks the compiler cannot see
// — a placeholder a MODEL emits (`agent`) and a request on the wire
// (`egress`) — are the secret guard's (pkg/backend/secretguard), witnessed
// there.
var secretSinkSites = []struct {
	sink string
	body string
}{
	{secretsink.Prompt, `prompt p:
  Use {{secrets.tok}}.

agent a:
  model: "m"
  user: p
  output: out

workflow w:
  worktree: none
  sandbox: none
  entry: a
  a -> done
`},
	{secretsink.Command, `tool kick:
  command: ` + "`echo {{secrets.tok}}`" + `
  output: out

workflow w:
  worktree: none
  sandbox: none
  entry: kick
  kick -> done
`},
	{secretsink.Command, `tool kick:
  language: python3
  script: "print({{secrets.tok}})"
  output: out

workflow w:
  worktree: none
  sandbox: none
  entry: kick
  kick -> done
`},
	{secretsink.Command, `tool kick:
  command: ` + "`echo hi`" + `
  postcondition: ` + "`test -n {{secrets.tok}}`" + `
  output: out

workflow w:
  worktree: none
  sandbox: none
  entry: kick
  kick -> done
`},
	{secretsink.Params, `tool comment:
  action: forgejo.issue.comment
  connection: c
  params:
    body: "{{secrets.tok}}"
  output: out

workflow w:
  worktree: none
  sandbox: none
  entry: comment
  comment -> done
`},
}

// secretSinkSource declares the value secret `tok` with the given sinks line
// (empty = no `sinks:` at all) in front of a site's body.
func secretSinkSource(sinks, body string) string {
	src := "dsl: 2\n\nsecrets:\n  tok:\n    as: value\n"
	if sinks != "" {
		src += "    sinks: [" + sinks + "]\n"
	}
	return src + "\nschema out:\n  ok: bool\n\n" + body
}

func diagMessages(r *CompileResult, code DiagCode) []string {
	var out []string
	for _, d := range r.Diagnostics {
		if d.Code == code {
			out = append(out, d.Message)
		}
	}
	return out
}

// C323: a value secret's reference at a sink its `sinks:` does not grant is
// refused on the REFERENCE, naming the sink and the grant that would allow
// it. Mutation toward the allowed alternatives, per site: the same reference
// with the sink granted compiles clean, and so does a secret that names no
// `sinks:` at all — the default is every sink (axis 2 = ALL), and the refusal
// is the grant's doing, not the site's.
func TestC323AReferenceAtAnUngrantedSinkIsRefusedOnTheReference(t *testing.T) {
	for _, site := range secretSinkSites {
		name := site.sink + "/" + strings.Fields(site.body)[0] + "-" + strings.Fields(site.body)[1]
		t.Run(name, func(t *testing.T) {
			// `agent` is a grant a value secret may carry and that no
			// compile-time site feeds: the one grant that leaves every site
			// of the table ungranted.
			refused := compileText(t, secretSinkSource(secretsink.Agent, site.body))
			msgs := diagMessages(refused, DiagSecretSinkUngranted)
			if len(msgs) == 0 {
				t.Fatalf("C323 did not fire on {{secrets.tok}} at the %s sink of a `sinks: [agent]` secret:\n%v", site.sink, refused.Diagnostics)
			}
			for _, want := range []string{"reaches the " + site.sink + " sink", `secret "tok" does not grant (sinks: [agent])`, "add `" + site.sink + "`"} {
				if !strings.Contains(msgs[0], want) {
					t.Errorf("C323 message does not carry %q:\n%s", want, msgs[0])
				}
			}
			if !hasDiagAtSeverity(refused, string(DiagSecretSinkUngranted), SeverityError) {
				t.Errorf("C323 is not an error: a run would materialise the value where its declaration said it may not go")
			}

			// `prompt` alone resolves nothing (C322): the prompt site is granted
			// beside `agent`, a resolving sink no compile-time site feeds.
			grantedList := site.sink
			if site.sink == secretsink.Prompt {
				grantedList = secretsink.Prompt + ", " + secretsink.Agent
			}
			granted := compileText(t, secretSinkSource(grantedList, site.body))
			if anyDiag(granted, string(DiagSecretSinkUngranted)) {
				t.Errorf("C323 fires with the %s sink GRANTED — the check reads the site, not the grant:\n%v", site.sink, granted.Diagnostics)
			}
			if anyDiag(granted, string(DiagSecretSinkInvalid)) {
				t.Errorf("C322 fires on the well-formed grant [%s]:\n%v", grantedList, granted.Diagnostics)
			}

			unrestricted := compileText(t, secretSinkSource("", site.body))
			if anyDiag(unrestricted, string(DiagSecretSinkUngranted)) || anyDiag(unrestricted, string(DiagSecretSinkInvalid)) {
				t.Errorf("a secret with no `sinks:` is refused at the %s sink — the default is every sink (ADR-146, axis 2):\n%v", site.sink, unrestricted.Diagnostics)
			}
		})
	}
}

// A file secret's reference renders its mounted PATH wherever it stands —
// the value reaches the run through the mount, which the `file` grant on the
// declaration covers — so the per-reference check never fires on it.
func TestC323LeavesAFileSecretsPathReferenceAlone(t *testing.T) {
	src := `dsl: 2

secrets:
  kube:
    as: file
    sinks: [file]

schema out:
  ok: bool

prompt p:
  The config is at {{secrets.kube}} ({{secrets.kube.path}}).

agent a:
  model: "m"
  user: p
  output: out

tool kick:
  command: ` + "`cat {{secrets.kube.path}}`" + `
  output: out

workflow w:
  worktree: none
  sandbox: none
  entry: a
  a -> kick
  kick -> done
`
	got := compileText(t, src)
	for _, code := range []DiagCode{DiagSecretSinkUngranted, DiagSecretSinkInvalid} {
		if anyDiag(got, string(code)) {
			t.Errorf("%s fires on a file secret's path reference:\n%v", code, got.Diagnostics)
		}
	}
}

// C322: the grants themselves — the vocabulary, each name once, and the
// declaration's own shape (a file secret grants `file`, a value secret never
// does).
func TestC322RefusesAGrantOutsideTheVocabularyOrAgainstTheDeclaration(t *testing.T) {
	cases := []struct {
		name  string
		block string
		want  string
	}{
		{"unknown sink", "  tok:\n    as: value\n    sinks: [prompt, stdout]\n", `sinks entry "stdout" is not a materialization sink (known: ` + secretsink.List() + ")"},
		{"a sink named twice", "  tok:\n    as: value\n    sinks: [prompt, prompt]\n", `sinks names "prompt" twice`},
		{"a file secret whose grants lack file", "  tok:\n    as: file\n    sinks: [prompt]\n", "add `file` to sinks: [prompt]"},
		{"a value secret granting file", "  tok:\n    as: value\n    sinks: [file, command]\n", "nothing mounts"},
		{"prompt alone resolves nothing", "  tok:\n    as: value\n    sinks: [prompt]\n", "names `prompt` alone"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := "dsl: 2\n\nsecrets:\n" + tc.block + `
schema out:
  ok: bool

tool kick:
  command: ` + "`echo hi`" + `
  output: out

workflow w:
  worktree: none
  sandbox: none
  entry: kick
  kick -> done
`
			got := compileText(t, src)
			msgs := diagMessages(got, DiagSecretSinkInvalid)
			if len(msgs) == 0 {
				t.Fatalf("C322 did not fire:\n%v", got.Diagnostics)
			}
			found := false
			for _, m := range msgs {
				if strings.Contains(m, tc.want) {
					found = true
				}
			}
			if !found {
				t.Errorf("no C322 message carries %q:\n%s", tc.want, strings.Join(msgs, "\n"))
			}
		})
	}
	// The well-formed declarations of both shapes compile clean.
	for _, block := range []string{
		"  tok:\n    as: value\n    sinks: [prompt, agent, command, params, egress]\n",
		"  tok:\n    as: file\n    sinks: [file, prompt]\n",
	} {
		got := compileText(t, "dsl: 2\n\nsecrets:\n"+block+`
schema out:
  ok: bool

tool kick:
  command: `+"`echo hi`"+`
  output: out

workflow w:
  worktree: none
  sandbox: none
  entry: kick
  kick -> done
`)
		if anyDiag(got, string(DiagSecretSinkInvalid)) {
			t.Errorf("C322 fires on a well-formed declaration:\n%s\n%v", block, got.Diagnostics)
		}
	}
}

// `sinks: []` reads as no list: every writer and the JSON transport drop an
// empty list, so the empty form must mean exactly what its absence means —
// every sink — or a document would change meaning on its way through them.
func TestAnEmptySinksListReadsAsNoList(t *testing.T) {
	got := compileText(t, secretSinkSource("", "tool kick:\n  command: `echo {{secrets.tok}}`\n  output: out\n\nworkflow w:\n  worktree: none\n  sandbox: none\n  entry: kick\n  kick -> done\n"))
	explicit := compileText(t, "dsl: 2\n\nsecrets:\n  tok:\n    as: value\n    sinks: []\n\nschema out:\n  ok: bool\n\ntool kick:\n  command: `echo {{secrets.tok}}`\n  output: out\n\nworkflow w:\n  worktree: none\n  sandbox: none\n  entry: kick\n  kick -> done\n")
	for _, r := range []*CompileResult{got, explicit} {
		for _, code := range []DiagCode{DiagSecretSinkInvalid, DiagSecretSinkUngranted} {
			if anyDiag(r, string(code)) {
				t.Errorf("%s fires on a secret whose sinks: is absent or empty:\n%v", code, r.Diagnostics)
			}
		}
	}
	if s := explicit.Workflow.Secrets["tok"]; s == nil || s.Sinks != nil {
		t.Errorf("sinks: [] reached the IR as %#v, want nil (no list)", explicit.Workflow.Secrets["tok"])
	}
}

// A sub-field the secret has not (C095) is refused for that alone: C323 is
// a judgement on a reference the secret can render, and a `.path` on a value
// secret is not one — the remedy of C323 would lead the author to C095.
func TestC323DoesNotHideC095OnAMalformedReference(t *testing.T) {
	got := compileText(t, secretSinkSource(secretsink.Agent, "tool kick:\n  command: `cat {{secrets.tok.path}}`\n  output: out\n\nworkflow w:\n  worktree: none\n  sandbox: none\n  entry: kick\n  kick -> done\n"))
	if !anyDiag(got, string(DiagSecretSubfield)) {
		t.Fatalf("C095 did not fire on .path of a value secret:\n%v", got.Diagnostics)
	}
	if anyDiag(got, string(DiagSecretSinkUngranted)) {
		t.Errorf("C323 fires beside C095 on a reference the secret cannot render:\n%v", got.Diagnostics)
	}
}

// An explicit `egress` grant on a secret with no `hosts:` is substituted
// toward any host: said as a warning (C324), never a refusal — a secret
// with no `sinks:` has always gone toward any host.
func TestC324WarnsOfAnEgressGrantWithNoHosts(t *testing.T) {
	body := "tool kick:\n  command: `echo hi`\n  output: out\n\nworkflow w:\n  worktree: none\n  sandbox: none\n  entry: kick\n  kick -> done\n"
	open := compileText(t, secretSinkSource("prompt, egress", body))
	if !hasDiagAtSeverity(open, string(DiagSecretEgressUnscoped), SeverityWarning) {
		t.Fatalf("C324 did not warn on egress without hosts:\n%v", open.Diagnostics)
	}
	if hasDiagAtSeverity(open, string(DiagSecretEgressUnscoped), SeverityError) {
		t.Fatal("C324 is an error: it refuses a declaration that has always been legal")
	}
	scoped := compileText(t, "dsl: 2\n\nsecrets:\n  tok:\n    as: value\n    hosts: [\"api.example.com\"]\n    sinks: [prompt, egress]\n\nschema out:\n  ok: bool\n\n"+body)
	if anyDiag(scoped, string(DiagSecretEgressUnscoped)) {
		t.Errorf("C324 fires on an egress grant that names its hosts:\n%v", scoped.Diagnostics)
	}
	unrestricted := compileText(t, secretSinkSource("", body))
	if anyDiag(unrestricted, string(DiagSecretEgressUnscoped)) {
		t.Errorf("C324 fires on a secret with no sinks: at all:\n%v", unrestricted.Diagnostics)
	}
}

// Two names that derive one placeholder (`tok`, `_tok`) would render the
// same token for two declarations — one value registered, one grant list
// governing the other's references: refused on the declaration (C090).
func TestC090RefusesTwoSecretsThatShareAPlaceholder(t *testing.T) {
	got := compileText(t, "dsl: 2\n\nsecrets:\n  tok:\n    as: value\n  _tok:\n    as: value\n\nschema out:\n  ok: bool\n\ntool kick:\n  command: `echo hi`\n  output: out\n\nworkflow w:\n  worktree: none\n  sandbox: none\n  entry: kick\n  kick -> done\n")
	msgs := diagMessages(got, DiagDuplicateSecret)
	if len(msgs) == 0 || !strings.Contains(msgs[0], "share the placeholder __ITERION_SECRET_tok__") {
		t.Fatalf("C090 did not refuse the placeholder collision:\n%v", got.Diagnostics)
	}
	if secretsink.PlaceholderFor("_tok") != "__ITERION_SECRET_tok__" {
		t.Fatalf("PlaceholderFor(_tok) = %q", secretsink.PlaceholderFor("_tok"))
	}
}

// The IR carries the grants as declared, and Grants reads them with the
// ALL default: the one source the guard and the runtime read from.
func TestASecretsGrantsReachTheIRWithTheAllDefault(t *testing.T) {
	got := compileText(t, secretSinkSource("prompt, egress", secretSinkSites[0].body))
	s := got.Workflow.Secrets["tok"]
	if s == nil || strings.Join(s.Sinks, ",") != "prompt,egress" {
		t.Fatalf("sinks did not reach the IR: %+v", s)
	}
	if !s.Grants(secretsink.Prompt) || s.Grants(secretsink.Command) {
		t.Errorf("Grants reads the list wrong: prompt=%v command=%v", s.Grants(secretsink.Prompt), s.Grants(secretsink.Command))
	}
	all := compileText(t, secretSinkSource("", secretSinkSites[0].body)).Workflow.Secrets["tok"]
	for _, sink := range secretsink.All {
		if !all.Grants(sink) {
			t.Errorf("a secret with no sinks: does not grant %s — the default is every sink", sink)
		}
	}
}
