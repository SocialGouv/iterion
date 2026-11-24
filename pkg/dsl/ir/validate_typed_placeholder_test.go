package ir

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/secretsink"
	"github.com/SocialGouv/iterion/pkg/store"
)

// typedPlaceholderSource declares a value secret and the server-minted
// credential's var in front of one tool node's body.
func typedPlaceholderSource(body string) string {
	return `dsl: 2

vars:
  forge_publish_token: string = ""

secrets:
  api_token:
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

// C340: a body resolves only the references its author writes (ADR-146,
// amendment of 2026-11-14), so a secret's placeholder typed as text is inert
// where the old materialiser resolved it. Each row is one reading the check
// has to tell apart.
func TestC340ASecretPlaceholderTypedAsTextWarns(t *testing.T) {
	t.Parallel()
	api := secretsink.PlaceholderFor("api_token")
	minted := secretsink.PlaceholderFor(store.ForgePublishTokenVar)
	for _, tc := range []struct {
		name, body, where, wantRef string
	}{{
		name:    "declared secret typed in a command",
		body:    "tool post:\n  command: `curl -H \"Authorization: Bearer " + api + "\" https://example.invalid && printf '{\"ok\":true}'`\n  output: out\n",
		where:   "command",
		wantRef: "{{secrets.api_token}}",
	}, {
		name:    "minted credential typed in a postcondition",
		body:    "tool post:\n  command: `printf '{\"ok\":true}'`\n  postcondition: `test -n '" + minted + "'`\n  output: out\n",
		where:   "postcondition",
		wantRef: "{{vars." + store.ForgePublishTokenVar + "}}",
	}, {
		name:    "declared secret typed in a script, even as an environment read",
		body:    "tool post:\n  language: sh\n  script: \"printf '%s' \\\"$" + api + "\\\"\"\n  output: out\n",
		where:   "script",
		wantRef: "{{secrets.api_token}}",
	}, {
		name: "the reference itself",
		body: "tool post:\n  command: `curl -H \"Authorization: Bearer \"{{secrets.api_token}} https://example.invalid && printf '{\"ok\":true}'`\n  output: out\n",
	}, {
		name: "an environment read in a command",
		body: "tool post:\n  command: `PUB={{vars.forge_publish_token}} printf '%s' \"$" + minted + "\" \"${" + api + "}\" >/dev/null && printf '{\"ok\":true}'`\n  output: out\n",
	}, {
		name: "the minted credential's variable named in a script",
		body: "tool post:\n  language: py\n  script: \"import os, json\\ntok = os.environ.get('" + minted + "', '')\\nprint(json.dumps({'ok': bool(tok)}))\\n\"\n  output: out\n",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res := compileText(t, typedPlaceholderSource(tc.body))
			var warns []Diagnostic
			for _, d := range res.Diagnostics {
				if d.Severity == SeverityError {
					t.Errorf("the fixture does not compile clean: %s", d.Error())
				}
				if d.Code == DiagTypedSecretPlaceholder {
					warns = append(warns, d)
				}
			}
			want := 0
			if tc.wantRef != "" {
				want = 1
			}
			if len(warns) != want {
				t.Fatalf("C340 fired %d time(s), want %d:\n%v", len(warns), want, res.Diagnostics)
			}
			if want == 0 {
				return
			}
			d := warns[0]
			if d.Severity != SeverityWarning {
				t.Errorf("C340 is %q, want a warning: the bytes run as written and the author may mean them", d.Severity)
			}
			for _, s := range []string{tc.where, tc.wantRef} {
				if !strings.Contains(d.Message, s) {
					t.Errorf("the C340 message does not carry %q:\n%s", s, d.Message)
				}
			}
			if d.NodeID != "post" || d.Line == 0 {
				t.Errorf("C340 landed on node %q line %d, want the tool node's property", d.NodeID, d.Line)
			}
			if strings.TrimSpace(d.Hint) == "" {
				t.Error("C340 arrives with no catalogue fix line")
			}
		})
	}
}
