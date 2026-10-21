package model

import (
	"os"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/platformcfg"
)

// The reader predicate of the C188 lint (ADR-130) is the engine's own
// segmentation: a braced form the engine expands is one the lint names, a
// form it passes through verbatim (`${X#y}`, `${X:+y}`, `${X%y}`, `${#X}`,
// `${p.name}`) is one the lint emits nothing for, and on a nested default
// (`${A:-${B:-c}}`) the engine consults the OUTER name only, its segment
// ending at the first `}`, handing the inner form to the shell untouched.
// This pin holds ir.BracedEnvReads to the engine on the whole disagreement
// class, twice per row: the helper's read set, and the engine's actual
// answer under a controlled environment. A form the engine would answer
// that the helper does not report reddens here, on the engine's side of
// the arrow, so the lint cannot drift from what expandBracedEnv consults.
//
// Two things keep the table from being a closed world. The bash arms the
// engine does NOT support — `${X-y}`, `${X:=y}`, `${X:?e}`, `${X/a/b}`,
// `${!X}`, `${X[0]}` — each hold a row of their own, so growing one in the
// engine is a row that changes. And every row carries one guarantee past
// its own expectations: whenever the engine REWROTE the form, the helper
// must have named a name — so a new arm cannot be absorbed by bumping a
// row's rendered text while the lint goes blind to the form.
//
// The engine is quote-blind: the rows on a single-quoted span, a shell
// comment and a heredoc body answer there too. The C188 lint's silence at
// those positions is its shell-live reader half's decision, not a claim
// that the engine leaves them alone — a decision taken by intersecting
// OFFSETS, so every row also pins that each read points at the `${` it
// describes.
//
// Mutation that reddens it: teach looksLikeEnvRef to accept `.` — the
// dotted row then renders its sentinel where it must stay verbatim,
// naming the form the engine now eats and the read the helper still
// fails to report. Or grow the engine an arm the helper does not model
// (split the body at the first `-`, not `:-`): the `${X-y}` row renders
// its sentinel and the guarantee names the blind form.
func TestBracedEnvReadsMatchWhatTheEngineConsults(t *testing.T) {
	tests := []struct {
		name     string
		form     string
		set      map[string]string
		unset    []string
		want     []ir.BracedEnvRead
		rendered string
	}{
		{
			name:     "a plain read, answered",
			form:     "${COUNT}",
			set:      map[string]string{"COUNT": "FROM_ENGINE"},
			want:     []ir.BracedEnvRead{{Name: "COUNT"}},
			rendered: "FROM_ENGINE",
		},
		{
			name:     "a plain read, unanswered stays verbatim",
			form:     "${COUNT}",
			unset:    []string{"COUNT"},
			want:     []ir.BracedEnvRead{{Name: "COUNT"}},
			rendered: "${COUNT}",
		},
		{
			name:     "a plain read answers a set-but-empty name with empty",
			form:     "${COUNT}",
			set:      map[string]string{"COUNT": ""},
			want:     []ir.BracedEnvRead{{Name: "COUNT"}},
			rendered: "",
		},
		{
			name:     "the default arm resolves unconditionally",
			form:     "x${COUNT:-0}y",
			unset:    []string{"COUNT"},
			want:     []ir.BracedEnvRead{{Name: "COUNT", HasDefault: true}},
			rendered: "x0y",
		},
		{
			name:     "the default arm prefers the set name",
			form:     "x${COUNT:-0}y",
			set:      map[string]string{"COUNT": "FROM_ENGINE"},
			want:     []ir.BracedEnvRead{{Name: "COUNT", HasDefault: true}},
			rendered: "xFROM_ENGINEy",
		},
		{
			name:     "the default arm answers a set-but-empty name with empty",
			form:     "x${COUNT:-0}y",
			set:      map[string]string{"COUNT": ""},
			want:     []ir.BracedEnvRead{{Name: "COUNT", HasDefault: true}},
			rendered: "xy",
		},
		{
			name:     "a dotted form is never consulted",
			form:     "${p.name}",
			set:      map[string]string{"p": "EATEN", "p.name": "EATEN"},
			want:     nil,
			rendered: "${p.name}",
		},
		{
			name:     "the prefix strip is never consulted",
			form:     "${X#y}",
			set:      map[string]string{"X": "EATEN"},
			want:     nil,
			rendered: "${X#y}",
		},
		{
			name:     "the alt-value arm is never consulted",
			form:     "${X:+y}",
			set:      map[string]string{"X": "EATEN"},
			want:     nil,
			rendered: "${X:+y}",
		},
		{
			name:     "the suffix strip is never consulted",
			form:     "${X%y}",
			set:      map[string]string{"X": "EATEN"},
			want:     nil,
			rendered: "${X%y}",
		},
		{
			name:     "the length form is never consulted",
			form:     "${#X}",
			set:      map[string]string{"X": "EATEN"},
			want:     nil,
			rendered: "${#X}",
		},
		{
			name:     "an empty default with the name absent",
			form:     "pre ${X:-} post",
			unset:    []string{"X"},
			want:     []ir.BracedEnvRead{{Name: "X", HasDefault: true}},
			rendered: "pre  post",
		},
		{
			name:     "the nested default consults the outer name only",
			form:     "${A:-${B:-c}}",
			set:      map[string]string{"A": "OUTER", "B": "INNER"},
			want:     []ir.BracedEnvRead{{Name: "A", HasDefault: true}},
			rendered: "OUTER}",
		},
		{
			name:     "the nested default, outer set to a plain value",
			form:     "${A:-${B:-c}}",
			set:      map[string]string{"A": "x"},
			unset:    []string{"B"},
			want:     []ir.BracedEnvRead{{Name: "A", HasDefault: true}},
			rendered: "x}",
		},
		{
			name:     "the nested default, outer set-but-empty",
			form:     "${A:-${B:-c}}",
			set:      map[string]string{"A": ""},
			unset:    []string{"B"},
			want:     []ir.BracedEnvRead{{Name: "A", HasDefault: true}},
			rendered: "}",
		},
		{
			name:     "the nested default, outer absent keeps the inner form",
			form:     "${A:-${B:-c}}",
			set:      map[string]string{"B": "INNER"},
			unset:    []string{"A"},
			want:     []ir.BracedEnvRead{{Name: "A", HasDefault: true}},
			rendered: "${B:-c}",
		},
		{
			name:     "an unterminated form is never a segment",
			form:     "x ${COUNT more",
			set:      map[string]string{"COUNT": "EATEN"},
			want:     nil,
			rendered: "x ${COUNT more",
		},
		// The bash arms the engine does not support: one row each, so
		// growing one is a row that changes rather than a silent
		// widening of what the lint must see.
		{
			name:     "the single-dash default is never consulted",
			form:     "${X-y}",
			set:      map[string]string{"X": "EATEN"},
			want:     nil,
			rendered: "${X-y}",
		},
		{
			name:     "assign-default is never consulted",
			form:     "${X:=y}",
			set:      map[string]string{"X": "EATEN"},
			want:     nil,
			rendered: "${X:=y}",
		},
		{
			name:     "the error arm is never consulted",
			form:     "${X:?e}",
			set:      map[string]string{"X": "EATEN"},
			want:     nil,
			rendered: "${X:?e}",
		},
		{
			name:     "substitution is never consulted",
			form:     "${X/a/b}",
			set:      map[string]string{"X": "EATEN"},
			want:     nil,
			rendered: "${X/a/b}",
		},
		{
			name:     "indirection is never consulted",
			form:     "${!X}",
			set:      map[string]string{"X": "EATEN"},
			want:     nil,
			rendered: "${!X}",
		},
		{
			name:     "an array subscript is never consulted",
			form:     "${X[0]}",
			set:      map[string]string{"X": "EATEN"},
			want:     nil,
			rendered: "${X[0]}",
		},
		{
			name:     "a substring is never consulted",
			form:     "${X:1:2}",
			set:      map[string]string{"X": "EATEN"},
			want:     nil,
			rendered: "${X:1:2}",
		},
		// The engine is quote-blind: it answers a read the shell would
		// leave alone (a single-quoted span), one nothing reads (a
		// comment) and one an unquoted heredoc delimiter leaves live.
		{
			name:     "a single-quoted read is answered all the same",
			form:     "COUNT=1\necho 'value ${COUNT}'\n",
			set:      map[string]string{"COUNT": "FROM_ENGINE"},
			want:     []ir.BracedEnvRead{{Name: "COUNT"}},
			rendered: "COUNT=1\necho 'value FROM_ENGINE'\n",
		},
		{
			name:     "a read inside a shell comment is answered all the same",
			form:     "COUNT=1\n# echo ${COUNT}\n",
			set:      map[string]string{"COUNT": "FROM_ENGINE"},
			want:     []ir.BracedEnvRead{{Name: "COUNT"}},
			rendered: "COUNT=1\n# echo FROM_ENGINE\n",
		},
		{
			name:     "a read inside a heredoc body is answered all the same",
			form:     "COUNT=1\ncat <<EOF\nvalue ${COUNT}\nEOF\n",
			set:      map[string]string{"COUNT": "FROM_ENGINE"},
			want:     []ir.BracedEnvRead{{Name: "COUNT"}},
			rendered: "COUNT=1\ncat <<EOF\nvalue FROM_ENGINE\nEOF\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for n, v := range tc.set {
				t.Setenv(n, v)
			}
			for _, n := range tc.unset {
				t.Setenv(n, "")
				if err := os.Unsetenv(n); err != nil {
					t.Fatalf("unsetenv %s: %v", n, err)
				}
			}
			got := ir.BracedEnvReads(tc.form)
			if len(got) != len(tc.want) {
				t.Fatalf("ir.BracedEnvReads(%q) = %v, want %v — the helper's read set and the engine's consulted set diverge",
					tc.form, got, tc.want)
			}
			for i := range got {
				// Name and arm are what the engine answers a form with, so
				// they are what a row expects. The offset is not an engine
				// claim — expandBracedEnv reports no position — so it is
				// pinned by its own invariant below rather than by a
				// literal per row.
				if got[i].Name != tc.want[i].Name || got[i].HasDefault != tc.want[i].HasDefault {
					t.Fatalf("ir.BracedEnvReads(%q) = %v, want %v — the helper's read set and the engine's consulted set diverge",
						tc.form, got, tc.want)
				}
				// At must point AT the form this read describes: a consumer
				// intersects it with the offsets a shell reads, and an
				// offset off by a byte intersects nothing.
				if got[i].At < 0 || got[i].At+2 > len(tc.form) || tc.form[got[i].At:got[i].At+2] != "${" {
					t.Fatalf("ir.BracedEnvReads(%q)[%d].At = %d, which is not the offset of a `${` — the read's position cannot be intersected with the shell's",
						tc.form, i, got[i].At)
				}
			}
			render := expandBracedEnv(tc.form)
			if render != tc.rendered {
				t.Fatalf("expandBracedEnv(%q) under the row's environment renders %q, want %q — the engine answers a form the lint's reader does not report, or refuses one it does",
					tc.form, render, tc.rendered)
			}
			// The guarantee every row carries, whatever it expects: an
			// engine that rewrote the body consulted a name, and the
			// helper must have reported it. This is what a new engine arm
			// reddens even when its row's rendered text is updated with it.
			if render != tc.form && len(got) == 0 {
				t.Fatalf("expandBracedEnv rewrote %q into %q while ir.BracedEnvReads reports no read — the engine consults a braced form the C188 lint cannot see",
					tc.form, render)
			}
		})
	}
}

// TestCloudPolicyRefusesThePlainArmNotTheDefaultArm: what C188 says about the
// two arms, against the policy object a cloud server and runner really install
// (cmd/iterion/server.go, cmd/iterion/runner.go). A plain `${NAME}` is
// answered only when the process carries the name AND the process-env policy
// exposes it, so for a credential-shaped name ITERION_CLOUD_ENV_PASSTHROUGH
// does not list (the test empties it) the engine hands the form to the
// shell verbatim and the plain arm's defeat cannot happen there — which is why
// the diagnostic says "whenever that process carries the name and its env
// policy exposes it" rather than asserting the defeat. The `:-` arm never
// consults the policy: it resolves regardless, and a refused name falls to the
// default, so its defeat is certain in every deployment.
//
// Mutation that reddens it: drop the ProcessEnvReadable call from
// lookupToolEnv (the refused row expands), or make the `:-` arm consult the
// policy (the default row stops resolving).
func TestCloudPolicyRefusesThePlainArmNotTheDefaultArm(t *testing.T) {
	t.Setenv("PROBE_C188_TOKEN", "platform-secret")
	t.Setenv("PROBE_C188_COUNT", "9")
	t.Setenv(platformcfg.EnvCloudEnvPassthrough, "")
	if !platformcfg.CredentialShapedName("PROBE_C188_TOKEN") {
		t.Fatal("PROBE_C188_TOKEN is not credential-shaped — the probe exercises nothing")
	}
	// Local operator, no policy: the plain arm's defeat is real.
	if got := expandBracedEnv("${PROBE_C188_TOKEN}"); got != "platform-secret" {
		t.Fatalf("expandBracedEnv(plain) = %q with no policy, want the process value: the probe proves nothing", got)
	}
	ir.SetProcessEnvPolicy(platformcfg.CloudProcessEnvPolicy())
	t.Cleanup(func() { ir.SetProcessEnvPolicy(nil) })
	for _, tc := range []struct {
		name, form, want, why string
	}{
		{
			name: "the plain arm of a credential-shaped name is left verbatim",
			form: "${PROBE_C188_TOKEN}",
			want: "${PROBE_C188_TOKEN}",
			why:  "the plain arm cannot defeat a body's assignment where the policy refuses the name",
		},
		{
			name: "the default arm resolves whatever the policy says",
			form: "${PROBE_C188_TOKEN:-none}",
			want: "none",
			why:  "the `:-` arm never consults the policy, so a refused name falls to the default — a certain defeat",
		},
		{
			name: "a name the policy allows is answered as ever",
			form: "${PROBE_C188_COUNT}",
			want: "9",
			why:  "the policy refuses credential-shaped names and platform secrets only",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := expandBracedEnv(tc.form); got != tc.want {
				t.Fatalf("expandBracedEnv(%q) = %q, want %q — %s", tc.form, got, tc.want, tc.why)
			}
		})
	}
}
