package ir

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/secretsink"
	"github.com/SocialGouv/iterion/pkg/store"
)

// mintedScriptSource declares the server-minted credential and an ordinary
// var — as every bot that holds the grant does — in front of one tool node's
// body.
func mintedScriptSource(body string) string {
	return `dsl: 2

vars:
  forge_publish_token: string = ""
  gate_enabled: bool = false

schema out:
  ok: bool

` + body + `
workflow w:
  worktree: none
  sandbox: none
  entry: publish
  publish -> done
`
}

// C331: a `{{vars.forge_publish_token}}` in a `script:` body is what the RUN
// refuses (refuseMintedScriptRef), and until this check nothing said so
// before the run — `iterion validate` was clean and the preview rendered a
// tidy placeholder, so the author learned it mid-node, after the spend
// upstream. The five rows are the five readings the condition has to tell
// apart — one per term, so a condition that drops any of them reddens here;
// the sink cannot tell the first from the last, since a script body and a
// `command:` share secretsink.Command.
func TestC331AMintedVarInAScriptBodyWarnsBeforeTheRunRefusesIt(t *testing.T) {
	ph := secretsink.PlaceholderFor(store.ForgePublishTokenVar)
	for _, tc := range []struct {
		name     string
		body     string
		wantWarn int
	}{{
		// The defect: the run refuses this body as soon as it holds the
		// grant, before the script's temp file is written.
		name:     "script body, default form",
		body:     "tool publish:\n  language: sh\n  script: \"printf '%s' {{vars.forge_publish_token}}\"\n  output: out\n",
		wantWarn: 1,
	}, {
		// The author's explicit override, which the run honours: raw text,
		// their quoting, the value rendered into the temp file. Warning
		// there would push them toward a door they deliberately refused.
		name: "script body, raw form",
		body: "tool publish:\n  language: sh\n  script: \"printf '%s' '{{!vars.forge_publish_token}}'\"\n  output: out\n",
	}, {
		// The other form the run's refusal skips — a path longer than one,
		// the term C331 mirrors here: a member read of a string var drills
		// to nothing (drillTemplatePath answers no value on a string
		// parent), so the body renders the missing-value literal and the
		// bearer never reaches the temp file. Nothing is refused, so a
		// warning here would name a defect the run does not have.
		name: "script body, sub-field path",
		body: "tool publish:\n  language: sh\n  script: \"printf '%s' {{vars.forge_publish_token.inner}}\"\n  output: out\n",
	}, {
		// An ordinary var in the same position: nothing minted it, so
		// nothing is refused and nothing warns.
		name: "script body, ordinary var",
		body: "tool publish:\n  language: sh\n  script: \"printf '%s' {{vars.gate_enabled}}\"\n  output: out\n",
	}, {
		// The door the catalog uses and the message names: the command path
		// swaps the single-quoted placeholder for a read of the child's
		// environment, so the reference resolves and the bearer never
		// enters the command line.
		name: "command, default form",
		body: "tool publish:\n  command: " + "`PUB_TOKEN={{vars.forge_publish_token}} printf '%s' \"$PUB_TOKEN\"`" + "\n  output: out\n",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			res := compileText(t, mintedScriptSource(tc.body))
			var warns []Diagnostic
			for _, d := range res.Diagnostics {
				if d.Severity == SeverityError {
					t.Errorf("the fixture does not compile clean: %s", d.Error())
				}
				if d.Code == DiagMintedVarInScriptBody {
					warns = append(warns, d)
				}
			}
			if len(warns) != tc.wantWarn {
				t.Fatalf("C331 fired %d time(s), want %d:\n%v", len(warns), tc.wantWarn, res.Diagnostics)
			}
			if tc.wantWarn == 0 {
				return
			}
			d := warns[0]
			if d.Severity != SeverityWarning {
				t.Errorf("C331 is %q, want a warning: a reference judged by NAME, on a run that may hold no grant, may not refuse the bot", d.Severity)
			}
			// The var, the door and the override: what an author has to read
			// to act, in one voice with the run's refusal.
			for _, want := range []string{
				store.ForgePublishTokenVar,
				ph,
				"{{!vars." + store.ForgePublishTokenVar + "}}",
			} {
				if !strings.Contains(d.Message, want) {
					t.Errorf("the C331 message does not carry %q:\n%s", want, d.Message)
				}
			}
			if d.NodeID != "publish" || d.Line == 0 {
				t.Errorf("C331 landed on node %q line %d, want the tool node's own declaration: a reference in a body has no span of its own", d.NodeID, d.Line)
			}
			if strings.TrimSpace(d.Hint) == "" {
				t.Error("C331 arrives with no catalogue fix line")
			}
		})
	}
}
