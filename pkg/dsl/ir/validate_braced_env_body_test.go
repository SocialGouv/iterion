package ir

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// C188 — the engine's braced env expansion defeats the body's own
// assignment (#1522, ADR-130)
// ---------------------------------------------------------------------------
//
// bracedEnvSrc wraps a tool body in a minimal workflow. The %s is the whole
// `tool gate:` block under test.
func bracedEnvSrc(tool string) string {
	return tool + "\nworkflow w:\n  entry: gate\n  gate -> done\n"
}

// bracedEnvDiags returns every C188 diagnostic of a compile, in order.
func bracedEnvDiags(r *CompileResult) []*Diagnostic {
	var out []*Diagnostic
	for i := range r.Diagnostics {
		if r.Diagnostics[i].Code == DiagBracedEnvDefeatsAssignment {
			out = append(out, &r.Diagnostics[i])
		}
	}
	return out
}

// TestBracedEnvDefeatsAssignment_FiresOnBothArms: a shell body that assigns
// a name and reads it back in a form the engine expands — the plain
// `${NAME}` (latent: the engine wins the day its process carries the name)
// and the `${NAME:-default}` (certain: the engine resolves any `:-` form
// unconditionally). Every writer arm the check judges is a row: `NAME=`,
// `export NAME=`, `read NAME`, `for NAME in`, and the postcondition body
// (the third body the walk covers).
//
// Mutation that reddens it: drop the `:-` branch from the reader set (the
// RUN_ID row goes silent), or drop the export/read/for writer arms from
// scanShellBody (their rows go silent).
func TestBracedEnvDefeatsAssignment_FiresOnBothArms(t *testing.T) {
	for name, tc := range map[string]struct {
		tool    string
		wantVal string // the read name the finding must carry
	}{
		"plain read after a command substitution": {
			tool:    "tool gate:\n  command: |\n    COUNT=$(printf '%s' hi | jq 'length')\n    echo \"${COUNT}\"\n",
			wantVal: "COUNT",
		},
		"default-arm read after an assignment": {
			tool:    "tool gate:\n  command: |\n    RUN_ID=\"x\"\n    if [ -z \"${RUN_ID:-}\" ]; then echo empty; fi\n",
			wantVal: "RUN_ID",
		},
		"export arm": {
			tool:    "tool gate:\n  command: |\n    export COUNT=1\n    echo \"${COUNT}\"\n",
			wantVal: "COUNT",
		},
		"read arm": {
			tool:    "tool gate:\n  command: |\n    read -r COUNT\n    echo \"${COUNT}\"\n",
			wantVal: "COUNT",
		},
		"for arm": {
			tool:    "tool gate:\n  command: |\n    for COUNT in a b; do\n      echo \"${COUNT}\"\n    done\n",
			wantVal: "COUNT",
		},
		"postcondition body": {
			tool:    "tool gate:\n  command: \"true\"\n  goal: \"g\"\n  postcondition: |\n    COUNT=1\n    test -n \"${COUNT}\"\n",
			wantVal: "COUNT",
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := compileText(t, bracedEnvSrc(tc.tool))
			got := bracedEnvDiags(r)
			if len(got) != 1 {
				t.Fatalf("want exactly one C188 finding, got %d\ndiagnostics: %v", len(got), r.Diagnostics)
			}
			if got[0].Severity != SeverityWarning {
				t.Errorf("C188 is severity %v, want warning", got[0].Severity)
			}
			if got[0].NodeID != "gate" {
				t.Errorf("C188 attributed to %q, want the tool node", got[0].NodeID)
			}
			for _, want := range []string{"${" + tc.wantVal, "$" + tc.wantVal} {
				if !strings.Contains(got[0].Message, want) {
					t.Errorf("message %q does not name the read or the bare fix %q", got[0].Message, want)
				}
			}
		})
	}
}

// TestBracedEnvDefeatsAssignment_SilentWhereTheEngineDoesNotExpand: the
// reader predicate is the engine's own — `${NAME#x}`, `${NAME:+y}`,
// `${NAME%z}`, `${#NAME}` and `${p.name}` are never expanded by
// expandBracedEnv, so a body assigning the name they mention draws nothing
// for THEM (the `${ID#native:}` prefix strip of adr-cartograph is the
// shipped shape). The `${ID_SHORT}` read on the same line still fires: the
// assignment and the read are both real.
//
// Mutation that reddens it: skip the identifier filter on the probe's names
// (take the leading identifier of every braced segment) — the dotted and
// parameter-expansion rows fire, and this test names the lie.
func TestBracedEnvDefeatsAssignment_SilentWhereTheEngineDoesNotExpand(t *testing.T) {
	for name, tc := range map[string]struct {
		tool     string
		wantFire string // "" = no finding at all; otherwise the only name that may fire
	}{
		"the prefix strip stays silent on its name": {
			tool:     "tool gate:\n  command: |\n    ID={{vars.issue_id}}\n    ID_SHORT=${ID#native:}\n    echo \"${ID_SHORT}\"\n",
			wantFire: "ID_SHORT",
		},
		"the alt-value operator": {
			tool:     "tool gate:\n  command: |\n    X=1\n    echo \"${X:+y}\"\n",
			wantFire: "",
		},
		"the suffix strip": {
			tool:     "tool gate:\n  command: |\n    X=1\n    echo \"${X%y}\"\n",
			wantFire: "",
		},
		"the length form": {
			tool:     "tool gate:\n  command: |\n    X=1\n    echo \"${#X}\"\n",
			wantFire: "",
		},
		"a dotted form is another language's": {
			tool:     "tool gate:\n  command: |\n    p=1\n    echo \"${p.name}\"\n",
			wantFire: "",
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := compileText(t, "vars:\n  issue_id: string = \"\"\n\n"+bracedEnvSrc(tc.tool))
			got := bracedEnvDiags(r)
			if tc.wantFire == "" {
				if len(got) != 0 {
					t.Fatalf("C188 on a form the engine never expands: %s", got[0].Message)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("want exactly one C188 finding for %s, got %d\ndiagnostics: %v", tc.wantFire, len(got), r.Diagnostics)
			}
			if !strings.Contains(got[0].Message, "${"+tc.wantFire) {
				t.Fatalf("finding %q does not name ${%s}", got[0].Message, tc.wantFire)
			}
			if strings.Contains(got[0].Message, "${ID}") || strings.Contains(got[0].Message, "$ID ") {
				t.Fatalf("finding names the ${ID#native:} read, the documented miss: %s", got[0].Message)
			}
		})
	}
}

// TestBracedEnvDefeatsAssignment_KeepsTheDialClean: the adr-cartograph
// dispatcher-transition line, verbatim minus its two converted reads — the
// dial read `${ITERION_DISPATCHER_PORT:-4891}` (093-bot-vars-runtime-settings)
// and the prefix strip `${ID#native:}` stay exactly as written, and the body
// never assigns either name. A check that reddened the dial would train
// authors to ignore every diagnostic.
//
// Mutation that reddens it: count a read whose name the body never assigns
// (drop the assignment half of the intersection) — the dial fires.
func TestBracedEnvDefeatsAssignment_KeepsTheDialClean(t *testing.T) {
	tool := "tool gate:\n" +
		"  command: |\n" +
		"    if [ -n \"{{vars.issue_id}}\" ]; then ID={{vars.issue_id}}; ID_SHORT=${ID#native:}; PORT=${ITERION_DISPATCHER_PORT:-4891}; curl -sS -X POST -H \"Content-Type: application/json\" -d '{\"to\":\"review\"}' \"http://localhost:$PORT/api/v1/native/issues/$ID_SHORT/transition\" 2>&1 | head -c 1000 || true; else echo '{\"skipped\":\"no issue_id\"}'; fi\n"
	r := compileText(t, "vars:\n  issue_id: string = \"\"\n\n"+bracedEnvSrc(tool))
	if got := bracedEnvDiags(r); len(got) != 0 {
		t.Fatalf("C188 reddened the documented dial or the prefix strip: %s", got[0].Message)
	}
}

// TestBracedEnvDefeatsAssignment_CommentsAndQuotesAreNotAssignments: the
// writer half is judged by scanShellBody, so a `COUNT=1` inside a comment,
// a single-quoted word or a heredoc body never arms the check — only a
// command-position assignment does.
//
// Mutation that reddens it: judge assignments by a raw regexp over the body
// instead of the scanner's words — all three rows fire.
func TestBracedEnvDefeatsAssignment_CommentsAndQuotesAreNotAssignments(t *testing.T) {
	for name, tool := range map[string]string{
		"in a comment":      "tool gate:\n  command: |\n    # COUNT=1\n    echo \"${COUNT}\"\n",
		"in single quotes":  "tool gate:\n  command: |\n    'COUNT=1'\n    echo \"${COUNT}\"\n",
		"in a heredoc body": "tool gate:\n  command: |\n    cat <<EOF\n    COUNT=1\n    EOF\n    echo \"${COUNT}\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			r := compileText(t, bracedEnvSrc(tool))
			if got := bracedEnvDiags(r); len(got) != 0 {
				t.Fatalf("C188 armed on a %s: %s", name, got[0].Message)
			}
		})
	}
}

// TestBracedEnvDefeatsAssignment_NonShellScriptStaysSilent: a js/py script
// body renders through its own language — `${NAME}` there is the language's
// template syntax, the bare-$NAME remedy is not valid code, so the shell
// check stays silent on it.
//
// Mutation that reddens it: drop the shell-bound gate from the walk — both
// rows fire.
func TestBracedEnvDefeatsAssignment_NonShellScriptStaysSilent(t *testing.T) {
	for name, lang := range map[string]string{"js": "js", "py": "py"} {
		t.Run(name, func(t *testing.T) {
			tool := "tool gate:\n  script: |\n    COUNT=1\n    echo \"${COUNT}\"\n  language: " + lang + "\n"
			r := compileText(t, bracedEnvSrc(tool))
			if got := bracedEnvDiags(r); len(got) != 0 {
				t.Fatalf("C188 fired on a language %s body: %s", lang, got[0].Message)
			}
		})
	}
}

// TestBracedEnvDefeatsAssignment_OneFindingPerBodyAndName: three reads of
// the same name in one body are one finding, at the first read's line — a
// finding per occurrence would bury the fix under noise.
//
// Mutation that reddens it: record the LAST read's line instead of the
// first (drop the first-wins guard on the live map) — the finding count
// holds but the line moves off the first read.
func TestBracedEnvDefeatsAssignment_OneFindingPerBodyAndName(t *testing.T) {
	tool := "tool gate:\n  command: |\n    COUNT=1\n    echo \"${COUNT}\"\n    echo \"${COUNT} and ${COUNT} again\"\n"
	r := compileText(t, bracedEnvSrc(tool))
	got := bracedEnvDiags(r)
	if len(got) != 1 {
		t.Fatalf("want one finding for three reads of COUNT, got %d: %v", len(got), got)
	}
	if !strings.Contains(got[0].Message, "body line 2") {
		t.Fatalf("finding %q does not sit on the first read's line (2)", got[0].Message)
	}
}

// TestBracedEnvDefeatsAssignment_ReaderSetIsShellLive: the read set is the
// INTERSECTION of where the shell looks and whom the engine consults — a
// braced form inside a comment, a single-quoted span or a heredoc body is
// substituted by the engine but read by nothing, and stays silent. On the
// nested default `${A:-${B:-c}}` the engine consults the OUTER name only
// (its segment ends at the first `}`): the body never answers `B` through
// it, bash does — so `B` stays silent even while assigned, and a body that
// assigns `A` is named on `A` alone.
//
// A herestring (`<<<`) is the other side of the heredoc arm: it has no body,
// so nothing after it is data and its own word is read by the shell.
//
// Mutations that redden it: swap the intersection's engine half for a
// whole-body probe through expandWithDefault — the comment, quoted and
// heredoc rows fire on COUNT, and the nested row fires on B with a false
// message; or let a `<<<` fall through to the heredoc arm (its second `<`
// then opens a heredoc on the next word) — the two herestring rows go
// silent, writer half included.
func TestBracedEnvDefeatsAssignment_ReaderSetIsShellLive(t *testing.T) {
	for name, tc := range map[string]struct {
		tool     string
		wantFire string // "" = silent; otherwise the only name that may fire
	}{
		"a read inside a comment is read by nothing": {
			tool:     "tool gate:\n  command: |\n    COUNT=1\n    # echo ${COUNT}\n    echo done\n",
			wantFire: "",
		},
		"a read inside single quotes never reaches the shell": {
			tool:     "tool gate:\n  command: |\n    COUNT=1\n    echo '${COUNT}'\n",
			wantFire: "",
		},
		"a read inside a heredoc body is data": {
			tool:     "tool gate:\n  command: |\n    COUNT=1\n    cat <<EOF\n    ${COUNT}\n    EOF\n",
			wantFire: "",
		},
		"the nested default consults the outer name only": {
			tool:     "tool gate:\n  command: |\n    B=1\n    echo \"${A:-${B:-c}}\"\n",
			wantFire: "",
		},
		"an assigned outer name fires on itself": {
			tool:     "tool gate:\n  command: |\n    A=1\n    B=1\n    echo \"${A:-${B:-c}}\"\n",
			wantFire: "A",
		},
		"a herestring has no body, so a later read stays live": {
			tool:     "tool gate:\n  command: |\n    cat <<< hi\n    X=1\n    echo \"${X}\"\n",
			wantFire: "X",
		},
		"a herestring's own word is live for the shell": {
			tool:     "tool gate:\n  command: |\n    X=1\n    cat <<< \"${X}\"\n",
			wantFire: "X",
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := compileText(t, bracedEnvSrc(tc.tool))
			got := bracedEnvDiags(r)
			if tc.wantFire == "" {
				if len(got) != 0 {
					t.Fatalf("C188 fired where neither the shell reads nor the engine consults a live name: %s", got[0].Message)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("want one finding naming %s, got %d: %v", tc.wantFire, len(got), got)
			}
			if !strings.Contains(got[0].Message, "${"+tc.wantFire) {
				t.Fatalf("finding %q does not name ${%s} — the nested inner name must stay silent", got[0].Message, tc.wantFire)
			}
			if strings.Contains(got[0].Message, "${B}") {
				t.Fatalf("finding names ${B}, which bash answers, not the engine: %s", got[0].Message)
			}
		})
	}
}

// TestBracedEnvDefeatsAssignment_MechanismIsTheEnginesRule: the clause an
// author reads is the engine's rule for THAT arm, and no more. A `:-` form
// is resolved whatever the environment holds; a plain `${NAME}` only when
// the engine's own process carries the name (bracedEnvWouldExpand), so its
// defeat is latent and the message says "whenever" rather than asserting it.
// No message names a shell, because the three bodies do not share one: a
// `command:` runs under `bash -c`, a `script:` under its language's
// interpreter (`sh` for sh/empty).
//
// Mutation that reddens it: state the plain arm unconditionally (drop the
// "whenever that process carries the name" clause), or name `bash -c` in
// either clause.
func TestBracedEnvDefeatsAssignment_MechanismIsTheEnginesRule(t *testing.T) {
	for name, tc := range map[string]struct {
		tool   string
		want   string
		absent string
	}{
		"the plain arm's defeat is conditional": {
			tool:   "tool gate:\n  command: |\n    COUNT=$(printf '%s' hi)\n    echo \"${COUNT}\"\n",
			want:   "whenever that process carries the name and its env policy exposes it",
			absent: "unconditionally",
		},
		"the default arm's defeat is certain": {
			tool: "tool gate:\n  command: |\n    RUN_ID=\"x\"\n    echo \"${RUN_ID:-}\"\n",
			want: "unconditionally",
		},
		"a language sh script names no shell either": {
			tool: "tool gate:\n  script: |\n    TOTAL=3\n    echo \"${TOTAL}\"\n  language: sh\n",
			want: "whenever that process carries the name and its env policy exposes it",
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := compileText(t, bracedEnvSrc(tc.tool))
			got := bracedEnvDiags(r)
			if len(got) != 1 {
				t.Fatalf("want exactly one C188 finding, got %d: %v", len(got), got)
			}
			msg := got[0].Message
			if !strings.Contains(msg, tc.want) {
				t.Errorf("message %q does not carry the engine's rule for this arm (%q)", msg, tc.want)
			}
			if tc.absent != "" && strings.Contains(msg, tc.absent) {
				t.Errorf("message %q claims %q on an arm the engine resolves only when its process carries the name", msg, tc.absent)
			}
			for _, shell := range []string{"bash", "sh -c", " sh "} {
				if strings.Contains(msg, shell) {
					t.Errorf("message %q names %q — a script body runs under its own interpreter, so the mechanism clause names no shell", msg, shell)
				}
			}
		})
	}
}

// TestBracedEnvDefeatsAssignment_DeclarationFlagsStillAssign: the two
// declaration utilities that write the global variable, `export` and
// `readonly`, assign their `NAME=value` operands, and a flag is never the
// name and never closes that window — `export -n COUNT=1` assigns COUNT as
// plainly as `export COUNT=1` (bash: COUNT is 1 afterwards, with or without
// the flag, and through every operand). Past a flag a bare word closes the
// window: `export -f helper COUNT=1` exports a function and refuses COUNT
// ("not a function"), so nothing there is assigned. `declare`, `local` and
// `typeset` record nothing at all (the documented misses below).
//
// Mutation that reddens it: close the window on any dash-word (the flagged
// rows go silent), keep it open on a bare word past a flag (the `export -f
// helper COUNT=1` row fires on an assignment bash refuses), or drop
// `readonly` from the utilities that open it (its row goes silent).
func TestBracedEnvDefeatsAssignment_DeclarationFlagsStillAssign(t *testing.T) {
	for name, tc := range map[string]struct {
		tool      string
		wantFires bool
	}{
		"export -n":                      {tool: "tool gate:\n  command: |\n    export -n COUNT=1\n    echo \"${COUNT}\"\n", wantFires: true},
		"the end-of-flags marker":        {tool: "tool gate:\n  command: |\n    export -- COUNT=1\n    echo \"${COUNT}\"\n", wantFires: true},
		"a flag, then two operands":      {tool: "tool gate:\n  command: |\n    export -n A=1 COUNT=2\n    echo \"${COUNT}\"\n", wantFires: true},
		"readonly":                       {tool: "tool gate:\n  command: |\n    readonly COUNT=1\n    echo \"${COUNT}\"\n", wantFires: true},
		"export -f names a function":     {tool: "tool gate:\n  command: |\n    export -f helper\n    echo \"${helper}\"\n", wantFires: false},
		"a bare word past a flag closes": {tool: "tool gate:\n  command: |\n    export -f helper COUNT=1\n    echo \"${COUNT}\"\n", wantFires: false},
	} {
		t.Run(name, func(t *testing.T) {
			r := compileText(t, bracedEnvSrc(tc.tool))
			got := bracedEnvDiags(r)
			if tc.wantFires {
				if len(got) != 1 {
					t.Fatalf("a flagged declaration assigns its NAME= operand: want one C188 finding, got %d: %v", len(got), got)
				}
				return
			}
			if len(got) != 0 {
				t.Fatalf("C188 on a body that assigns nothing — a bare word past a flag is not a name the command sets: %s", got[0].Message)
			}
		})
	}
}

// TestBracedEnvDefeatsAssignment_SilentWhereTheReadFeedsTheAssignment: the
// check names a read the body assigns BEFORE, in a command of its own, and
// only that. A command reads its own text before it assigns — bash expands
// a command's words and redirections before its prefix assignments take
// effect, and a declaration utility's operands before it assigns them
// (`X=1 echo "[${X}]"` and `export A=1 B="${A}"` both read the ambient
// value) — so a braced read inside the assigning command answers the
// ambient value by design (an assignment-only command's later assignment
// and its own redirection are the exceptions: DocumentedMissesStaySilent):
// a right-hand side
// (`PATH="${PATH}:/opt/bin"` appends to the inherited
// PATH, and the engine substituting it changes nothing the body does — the
// tool process inherits os.Environ(), so apart from a name the run-level
// layer overrides both resolve the same value; `RUN_ID="${RUN_ID:-none}"` is
// the self-default idiom, and the bare rewrite there yields the empty string
// instead of `none`), a `for`'s word list (`for ARCH in ${ARCH:-amd64 arm64}`
// is the ADR-093 dial shape, and the `:-` remedy spelled as a statement
// without quotes would set nothing and run `arm64` as a command), a `read`'s redirection target
// (`read -r CFG < "${CFG}"` reads the file the ambient CFG names), a second
// operand of one `export`, and a prefix assignment's own command. A read
// above the assignment is the same case in time. A warning on any of them
// would be false, and its remedy destructive or useless.
//
// The position is the assigning COMMAND's terminator, not its line: both
// catalogue conversions assign and read on one line, separated by a `;`,
// and both must keep firing — as must a loop whose BODY reads the variable
// the `for` binds.
//
// Mutations that redden it: drop the position half (accept a read wherever
// the name is assigned anywhere in the body) — every silent row fires, each
// claiming the assignment they feed is dead; or record the assigning word's
// END again instead of its command's terminator — the `for`, `read`,
// command-substitution and prefix rows fire.
func TestBracedEnvDefeatsAssignment_SilentWhereTheReadFeedsTheAssignment(t *testing.T) {
	for name, tc := range map[string]struct {
		body     string
		wantFire string // "" = silent; otherwise the name the single finding carries
		wantLine string // the "body line N" the message must carry
	}{
		"the self-default idiom": {
			body: "RUN_ID=\"${RUN_ID:-none}\"\necho \"[$RUN_ID]\"\n",
		},
		"an exported append to an inherited name": {
			body: "export PATH=\"${PATH}:/opt/bin\"\necho ok\n",
		},
		"a plain append to an inherited name": {
			body: "PATH=\"${PATH}:/opt/bin\"\necho ok\n",
		},
		"a read above the assignment": {
			body: "PREV=\"${LAST_ID}\"\nLAST_ID=new\necho \"$PREV\"\n",
		},
		"a for list over the loop variable's own dial": {
			body: "for ARCH in ${ARCH:-amd64 arm64}; do\n  echo \"$ARCH\"\ndone\n",
		},
		"a for list over the loop variable, plain": {
			body: "for P in ${P}; do :; done\n",
		},
		"a read redirection from the name read assigns": {
			body: "read -r CFG < \"${CFG}\"\n",
		},
		"a for list reading the variable past a command substitution": {
			body: "for X in $(ls) \"${X}\"; do :; done\n",
		},
		"a right-hand side reading the name through a command substitution": {
			body: "PATH=$(echo \"${PATH}\" | tr : ,)\necho ok\n",
		},
		"a second export operand reading the first": {
			body: "export A=1 B=\"${A}\"\necho ok\n",
		},
		"a prefix assignment read by its own command": {
			body: "X=1 echo \"${X}\"\n",
		},
		"a for loop BODY reading the variable it binds fires": {
			body:     "for X in a b; do\n  echo \"${X}\"\ndone\n",
			wantFire: "X",
			wantLine: "body line 2",
		},
		"a while-read loop BODY reading the name it reads fires": {
			body:     "while read -r LINE; do\n  echo \"${LINE}\"\ndone\n",
			wantFire: "LINE",
			wantLine: "body line 2",
		},
		"a counter seeded from the ambient value": {
			body: "COUNT=\"${COUNT:-0}\"\nCOUNT=$((COUNT+1))\necho \"$COUNT\"\n",
		},
		"the genuine defeat still fires": {
			body:     "COUNT=$(printf '%s' hi | wc -c)\necho \"${COUNT}\"\n",
			wantFire: "COUNT",
			wantLine: "body line 2",
		},
		"an accumulator reading its own previous value fires": {
			body:     "OUT=\"\"\nOUT=\"${OUT} more\"\necho \"$OUT\"\n",
			wantFire: "OUT",
			wantLine: "body line 2",
		},
		"assigned and read on ONE line fires": {
			body:     "PORT=4891; curl \"http://localhost:${PORT}/api\"\n",
			wantFire: "PORT",
			wantLine: "body line 1",
		},
		"the catalogue's prefix-strip line fires on the stripped name": {
			body:     "ID=x; ID_SHORT=${ID#native:}; echo \"/issues/${ID_SHORT}/transition\"\n",
			wantFire: "ID_SHORT",
			wantLine: "body line 1",
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := compileText(t, bracedEnvSrc("tool gate:\n  command: |\n"+indentToolBody(tc.body)))
			got := bracedEnvDiags(r)
			if tc.wantFire == "" {
				if len(got) != 0 {
					t.Fatalf("C188 on a read the body's own assignment feeds, or precedes: %s", got[0].Message)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("want one finding naming %s, got %d: %v", tc.wantFire, len(got), got)
			}
			if !strings.Contains(got[0].Message, "${"+tc.wantFire) {
				t.Fatalf("finding %q does not name ${%s}", got[0].Message, tc.wantFire)
			}
			if !strings.Contains(got[0].Message, tc.wantLine) {
				t.Fatalf("finding %q does not sit on %q", got[0].Message, tc.wantLine)
			}
		})
	}
}

// TestBracedEnvDefeatsAssignment_SilentWhereNoAssignmentPersists: a braced
// read the body never feeds draws nothing, because no assignment the body
// makes reaches it in bash — the engine answering it defeats nothing, and
// "this body's assignment is dead" would be false (each silent row was run
// under bash as written and as the engine rewrites it: the same output).
// The writer half is NARROWED to that end, never extended: every row below
// is a shape the scanner once recorded as an assignment, and every fix
// records less. Beside each silence, a genuine defeat of the nearest shape
// keeps firing.
//
// Mutations that redden it, one per silence, each on its own rows: drop
// dropPrefix from the verb (the prefix rows fire), drop the quote branch's
// dropPrefix (the prefix-before-a-quoted-verb row fires) or its verb (the
// target-less quoted-verb row fires), record a subshell's entries again
// (the subshell, substitution, array and extglob rows fire), record a bare
// declaration operand (the bare rows fire), drop the verb gate of the
// command-position arm (the operand and for-list rows fire), stop restoring
// the verb a nesting saved (the after-a-substitution rows fire), stop
// refusing an unclosed `$(`/backquote right-hand side (the three
// quoted-substitution rows and the pipe-glued backquote row fire), let a
// pipeline-element `read` open its window (the two pipe rows fire), end
// that window at a newline (the continued-pipeline row fires), open the
// window for `local`/`declare`/`typeset` again (the function rows fire),
// discharge a lone `&`'s assignments instead of dropping them (the
// backgrounded rows fire), stop reading a command modifier as the verb (the
// env, command and bare-exec rows fire), stop reading an escape that opens
// the command word as the verb (the escaped-word row fires), keep recording
// a declaration's operands past an `f` flag (the `-f` rows fire), record a
// `for` variable without waiting for its `in` (the two `for X do` rows
// fire), stop counting a backquote as nesting (the backquote row fires).
// Six guards keep controls firing: count a case pattern's `(` as nesting
// (the `(pat)` control goes silent), drop the verb's boundary test (the
// `X=$(…)/sub` control goes silent), let the second byte of `||` start a
// pipeline element (the `||` control goes silent), take every `&` for a
// lone one (the `&&` control goes silent), read a line continuation as a
// verb (the continuation control goes silent), or never close a backquote
// (the after-the-backquote control goes silent).
func TestBracedEnvDefeatsAssignment_SilentWhereNoAssignmentPersists(t *testing.T) {
	for name, tc := range map[string]struct {
		body     string
		wantFire string // "" = silent; otherwise the name the single finding carries
		wantLine string // the "body line N" the message must carry
	}{
		// A prefix assignment sets the name in its command's environment
		// alone: `HOME=/tmp/x true; echo "${HOME}"` prints the inherited HOME.
		"a prefix assignment of an earlier command": {
			body: "HOME=/tmp/x true\necho \"${HOME}\"\n",
		},
		// `env NAME=v cmd` puts NAME in cmd's environment, never the shell's.
		"an env modifier's operand": {
			body: "env GIT_DIR=/repo/.git true\necho \"using ${GIT_DIR}\"\n",
		},
		// IFS=, applies to the read alone: after it IFS is back to its value.
		"a prefix on a read, its own name read later": {
			body: "IFS=, read -r A B <<< \"x,y\"\necho \"sep=${IFS}\"\n",
		},
		"the read's own name still fires": {
			body:     "IFS=: read -r A B <<< \"p:q\"\necho \"${A}\"\n",
			wantFire: "A",
			wantLine: "body line 2",
		},
		// A quoted command word is the verb: X=1 was its prefix (every word
		// quoted, so no later unquoted word drops it in the quote's stead).
		"a prefix before a quoted command word": {
			body: "X=1 \"$CMD\" \"$ARG\"\necho \"${X}\"\n",
		},
		// …and `PREFIX=/opt` after it is make's operand, never an assignment
		// (bash: the inherited PREFIX), with a target after it or none.
		"a quoted command word's assignment-shaped operand": {
			body: "\"$MAKE\" PREFIX=/opt install\necho \"${PREFIX}\"\n",
		},
		"a quoted command word's operand, no target": {
			body: "\"$MAKE\" PREFIX=/opt\necho \"${PREFIX}\"\n",
		},
		"a quoted verb ends at its separator": {
			body:     "\"$CMD\" --flag; X=1; echo \"${X}\"\n",
			wantFire: "X",
			wantLine: "body line 1",
		},
		// Past a verb a NAME=value word is an operand, however many follow.
		"a verb's assignment-shaped operands": {
			body: "make build VERSION=1.2 COMMIT=abc\necho \"${COMMIT}\"\n",
		},
		"an assignment-only command of two words fires": {
			body:     "X=1 Y=2; echo \"${X}\"\n",
			wantFire: "X",
			wantLine: "body line 1",
		},
		// A nesting does not hide the verb of the command around it: the
		// operand after `$(date)` is echo's, the second operand is local's.
		"an operand after a command substitution": {
			body: "echo $(date) A=1\necho \"${A}\"\n",
		},
		"a local's second operand after a command substitution": {
			body: "f() { local X=$(date) Y=2; }\nf\necho \"${Y:-none}\"\n",
		},
		"a for list's word after a command substitution": {
			body: "for x in $(ls) FOO=1; do :; done\necho \"${FOO}\"\n",
		},
		"two substitution assignments of one command fire": {
			body:     "X=$(printf a) Y=$(printf b)\necho \"${Y}\"\n",
			wantFire: "Y",
			wantLine: "body line 2",
		},
		"an assignment after an array literal fires": {
			body:     "X=(a b) Y=1\necho \"${Y}\"\n",
			wantFire: "Y",
			wantLine: "body line 2",
		},
		// A word glued to a substitution's `)` continues the assignment.
		"a word glued to the substitution's close is no verb": {
			body:     "X=$(printf /root)/sub; echo \"${X}\"\n",
			wantFire: "X",
			wantLine: "body line 1",
		},
		// A subshell's assignment dies with it: bash prints the inherited
		// GOFLAGS, or `none` when there is none.
		"a subshell's export": {
			body: "( export GOFLAGS=-mod=mod; true )\necho \"flags: ${GOFLAGS:-none}\"\n",
		},
		"an assignment inside a command substitution, read outside it": {
			body: "OUT=$(X=1; echo hi)\necho \"[$OUT] ${X}\"\n",
		},
		// An array literal's elements are values: RVA_HOME is untouched.
		"an array literal's elements": {
			body: "ENVS=(RVA_HOME=/tmp PATH=/usr/bin)\necho \"${RVA_HOME} ${#ENVS[@]}\"\n",
		},
		// Extglob alternatives are a pattern: FOO is untouched.
		"extglob alternatives": {
			body: "shopt -s extglob\n[[ x == @(FOO=*|BAR=*) ]] || true\necho \"${FOO}\"\n",
		},
		// The POSIX `(pat)` form nests nothing: a later assignment persists.
		"the (pat) case form opens no subshell": {
			body:     "case \"a\" in (a) echo a ;; esac\nX=1\necho \"${X}\"\n",
			wantFire: "X",
			wantLine: "body line 3",
		},
		"an if-assignment persists": {
			body:     "if X=$(printf hi); then echo \"${X}\"; fi\n",
			wantFire: "X",
			wantLine: "body line 1",
		},
		// A bare declaration operand assigns nothing: the inherited value
		// is read, marked for export or read-only.
		"a bare export": {
			body: "export GH_TOKEN\necho \"Authorization: token ${GH_TOKEN}\"\n",
		},
		"a bare readonly": {
			body: "readonly RVA_RO\necho \"${RVA_RO}\"\n",
		},
		"a bare operand beside an assigned one": {
			body: "export A=1 B\necho \"${B}\"\n",
		},
		// A `for` list's words are values: FOO is untouched.
		"a for list's assignment-shaped words": {
			body: "for kv in FOO=1 BAR=2; do :; done\necho \"${FOO}\"\n",
		},
		// A quoted `$( … )` right-hand side reads its own name before the
		// assignment, inner quotes and separators notwithstanding.
		"a quoted substitution reading its own name past an inner pipe": {
			body: "ROW=\"$(printf '%s|%s' \"a|b\" \"${ROW}\")\"\necho \"$ROW\"\n",
		},
		"a quoted substitution reading its own name past a table header": {
			body: "TABLE=\"$(printf '%s\\n' \"| head |\" \"${TABLE}\")\"\necho \"$TABLE\"\n",
		},
		"a quoted substitution reading its own name past an unbalanced paren": {
			body: "NOTE=\"$(printf '%s %s' \"1) draft:\" \"${NOTE}\")\"\necho \"$NOTE\"\n",
		},
		// Held twice: by the backquote refusal, and by the quote after
		// `printf` read as a verb. The pipe-glued row holds by the first alone.
		"a backquoted right-hand side reading its own name": {
			body: "X=`printf 'a\\nb\\n' | grep -v \"${X}\"`\necho \"$X\"\n",
		},
		"a backquoted right-hand side reading its own name past a glued pipe": {
			body: "X=`hostname|grep -v \"${X}\"`\necho \"$X\"\n",
		},
		"a closed quoted substitution fires": {
			body:     "X=\"$(printf hi)\"; echo \"${X}\"\n",
			wantFire: "X",
			wantLine: "body line 1",
		},
		// A `read` that is a pipeline element runs in a subshell (bash
		// without lastpipe, dash always): the later read is the ambient X.
		"a read that is a pipeline element": {
			body: "printf a | read -r X\necho \"[${X}]\"\n",
		},
		"a pipeline continued on the next line": {
			body: "printf a |\nread -r X\necho \"[${X}]\"\n",
		},
		"a while-read loop behind a pipe fires in its own body": {
			body:     "printf 'l1\\n' | while read -r L; do echo \"${L}\"; done\n",
			wantFire: "L",
			wantLine: "body line 1",
		},
		"an || is no pipe": {
			body:     "false || read -r X <<< hi\necho \"[${X}]\"\n",
			wantFire: "X",
			wantLine: "body line 2",
		},
		// `local`, `declare` and `typeset` make the name local to the
		// function: g reads the global X, which f never touched.
		"a local in one function, read in another": {
			body: "f() { local X=1; }\ng() { echo \"${X:-none}\"; }\nf; g\n",
		},
		"a declare and a typeset in a function, read outside": {
			body: "f() { declare X=1; typeset Y=2; }\nf\necho \"${X:-none} ${Y:-none}\"\n",
		},
		"an export in a function writes the global": {
			body:     "f() { export X=1; }\nf\necho \"${X}\"\n",
			wantFire: "X",
			wantLine: "body line 3",
		},
		// A lone `&` runs its command in the background, in a subshell:
		// after `wait` bash prints the inherited X (so does dash, which
		// refuses the `<<<` of the read row).
		"a backgrounded assignment-only command": {
			body: "X=1 &\nwait\necho \"[${X}]\"\n",
		},
		"a backgrounded export": {
			body: "export X=1 &\nwait\necho \"[${X}]\"\n",
		},
		"a backgrounded read": {
			body: "read -r X <<< hi &\nwait\necho \"[${X}]\"\n",
		},
		"a backgrounded and-list ending in its assignment": {
			body: "true && X=1 &\nwait\necho \"[${X}]\"\n",
		},
		"a backgrounded (pat)-case substitution": {
			body: "X=$(case \"$v\" in (a) echo one;; esac) &\nwait\necho \"[${X}]\"\n",
		},
		"an && is no lone &": {
			body:     "X=1 && echo \"[${X}]\"\n",
			wantFire: "X",
			wantLine: "body line 1",
		},
		"a lone & ends its own command only": {
			body:     "sleep 0 & X=1\nwait\necho \"[${X}]\"\n",
			wantFire: "X",
			wantLine: "body line 3",
		},
		// A command modifier is the verb: a prefix before it is the
		// environment it runs with, a NAME=value after it its operand —
		// bash and dash print the inherited value. Before a bare `exec`
		// bash keeps no prefix either (sh does: ShScriptMissesStaySilent).
		"a prefix before a trailing env in a pipeline": {
			body: "GIT_DIR=/x/.git env | grep -c '^GIT_DIR=' >/dev/null\necho \"${GIT_DIR}\"\n",
		},
		"an env operand in a pipeline": {
			body: "env X=1 | grep -c '^X=1$' >/dev/null\necho \"[${X}]\"\n",
		},
		"a command operand": {
			body: "command X=1\necho \"[${X}]\"\n",
		},
		"a prefix before a bare exec": {
			body: "X=1 exec\necho \"[${X}]\"\n",
		},
		"command export still assigns": {
			body:     "command export X=1\necho \"[${X}]\"\n",
			wantFire: "X",
			wantLine: "body line 2",
		},
		// An escaped command word is the verb: HOME=/tmp/fakehome was the
		// prefix of `\true`, and the inherited HOME is printed.
		"a prefix before an escaped command word": {
			body: "HOME=/tmp/fakehome \\true\necho \"${HOME}\"\n",
		},
		"a line continuation after a prefix is no verb": {
			body:     "X=1 \\\nY=2\necho \"[${X}]\"\n",
			wantFire: "X",
			wantLine: "body line 3",
		},
		// Past a flag carrying `f` the operands name functions: bash
		// answers "X=1: not a function", dash refuses the flag; X is untouched.
		"an export -f operand": {
			body: "export -f X=1 2>/dev/null\necho \"[${X}]\"\n",
		},
		"a readonly -f operand": {
			body: "readonly -f X=1 2>/dev/null\necho \"[${X}]\"\n",
		},
		// `for NAME do` iterates the positional parameters, and a tool body
		// runs with none: the loop runs zero times and X keeps its value.
		"a for loop without in": {
			body: "for X do :; done\necho \"[${X}]\"\n",
		},
		"a for loop without in, after a separator": {
			body: "for X; do :; done\necho \"[${X}]\"\n",
		},
		"a for loop over a list binds its variable": {
			body:     "for X in a b; do :; done\necho \"[${X}]\"\n",
			wantFire: "X",
			wantLine: "body line 2",
		},
		// A backquoted substitution runs in a subshell, like `$( … )`:
		// X=1 dies in it, and bash and dash print `[<inherited>] hi`.
		"an assignment inside a backquoted substitution, read outside it": {
			body: "OUT=`true; X=1; echo hi`\necho \"[${X}] $OUT\"\n",
		},
		"an assignment after a closed backquote substitution fires": {
			body:     "echo `printf a` >/dev/null; X=1\necho \"[${X}]\"\n",
			wantFire: "X",
			wantLine: "body line 2",
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := compileText(t, bracedEnvSrc("tool gate:\n  command: |\n"+indentToolBody(tc.body)))
			got := bracedEnvDiags(r)
			if tc.wantFire == "" {
				if len(got) != 0 {
					t.Fatalf("C188 on a read no assignment of the body reaches in bash: %s", got[0].Message)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("want one finding naming %s — a genuine defeat beside a silenced shape — got %d: %v", tc.wantFire, len(got), got)
			}
			if !strings.Contains(got[0].Message, "${"+tc.wantFire) {
				t.Fatalf("finding %q does not name ${%s}", got[0].Message, tc.wantFire)
			}
			if !strings.Contains(got[0].Message, tc.wantLine) {
				t.Fatalf("finding %q does not sit on %q", got[0].Message, tc.wantLine)
			}
		})
	}
}

// TestBracedEnvDefeatsAssignment_DocumentedMissesStaySilent: REAL defeats
// the lint leaves unreported by design. In each row bash answers the read
// from the body's own assignment and the engine answers it from its
// environment, so the body is broken — but reporting the shape would take a
// deeper model of bash than the scanner keeps, or would fire on the correct
// bodies that share its spelling (precision over recall). They are listed in
// docs/workflow_authoring_pitfalls.md; a change that makes one fire must
// show it fires on no correct body of the same shape.
//
// Mutations that redden it: record a subshell's entries again (the
// substitution row fires), stop refusing an unclosed `$(` right-hand side
// (the separator row fires), open the window for `local`, `declare` and
// `typeset` again (the function and declare rows fire), drop the verb gate
// (the second `declare -x` operand, the two brace-line rows and the
// export-after-a-substitution row fire), stop restoring the verb a nesting
// saved (the one-line function and the export-after-a-substitution rows
// fire), drop dropPrefix from the verb (the redirection and empty-substitution
// rows fire), drop the quote branch's dropPrefix (the quote-continued row
// fires), stop reading an escape that opens a word as the verb (the
// escape-continued row fires), discharge a lone `&`'s assignments instead of
// dropping them (the `${…&…}` row fires), record a `for` variable without
// waiting for its `in` (the two `for` rows fire). The row of a redirection
// reading its own name is held twice — by the verb, and by the position
// (its read precedes the command's terminator) — and fires only when both go.
func TestBracedEnvDefeatsAssignment_DocumentedMissesStaySilent(t *testing.T) {
	for name, body := range map[string]string{
		// bash: hi — a subshell's assignment is never recorded, its own
		// read inside the substitution included.
		"a name assigned and read inside one command substitution": "OUT=$(read -r X <<< hi; echo \"${X}\")\necho \"$OUT\"\n",
		// bash: a b, a|b — the inner blank or separator flushes the word
		// with its `$(` open. The blank row is held twice (the refusal, and
		// the inner word after the blank read as a verb); the separator row
		// by the refusal alone.
		"a quoted substitution whose inner word holds a blank":     "X=\"$(printf '%s' \"a b\")\"\necho \"${X}\"\n",
		"a quoted substitution whose inner word holds a separator": "X=\"$(printf '%s' \"a|b\")\"\necho \"${X}\"\n",
		// bash: 1 and /tmp/out/report.json — an assignment-only command
		// assigns left to right, each assignment seeing the previous one.
		"a later assignment of the same command": "A=1 B=\"${A}\"\necho \"$B\"\n",
		"a one-line dir and file pair":           "DIR=/tmp/out FILE=\"${DIR}/report.json\"\necho \"$FILE\"\n",
		// bash: 1, [unset] — local, declare and typeset record nothing.
		"a function's own read of its local":                 "f() { local X=1; echo \"${X}\"; }\nf\n",
		"a function's local left unset, read with a default": "f() { local X; echo \"[${X:-unset}]\"; }\nf\n",
		"a flagged local":                       "f() { local -r COUNT=1; echo \"${COUNT}\"; }\nf\n",
		"a top-level declare":                   "declare X=1\necho \"${X}\"\n",
		"a top-level declare -x":                "declare -x COUNT=1\necho \"${COUNT}\"\n",
		"a declare with two operands":           "declare -x A=1 COUNT=2\necho \"${COUNT}\"\n",
		"the end-of-flags marker on declare":    "declare -- COUNT=1\necho \"${COUNT}\"\n",
		"typeset -g, global even in a function": "f() { typeset -g COUNT=1; }\nf\necho \"${COUNT}\"\n",
		// bash: 1 — `function` and `f()` are read as a verb, so the first
		// statement on the brace line falls behind the verb gate.
		"the first statement on a function keyword's brace line": "function f { X=1; }\nf\necho \"${X}\"\n",
		"the first statement of a one-line function":             "f() { X=1; }\nf\necho \"${X}\"\n",
		// bash: 2 — the `)` of a substitution ends export's operand window,
		// and the verb gate then refuses the operand after it.
		"an export operand after a command substitution": "export A=$(printf a) B=2\necho \"${B}\"\n",
		// bash: hi — the redirection word reads as the command's verb, so
		// the assignment it follows is dropped as that verb's prefix.
		"an assignment-only command carrying a redirection": "X=$(printf hi) 2>/dev/null\necho \"${X}\"\n",
		// bash: f.new — an assignment-only command assigns before its own
		// redirections under bash (dash expands the redirection first, and
		// creates f.<inherited>); the redirection word reads as the verb.
		"an assignment-only command's redirection reading its own name": "OUT=new >\"f.${OUT}\"\nls\n",
		// bash: 1 — a substitution expanding to no word leaves the command
		// assignment-only, and its `$` reads as the verb.
		"a prefix before a substitution that expands to no word": "X=1 $(true)\necho \"[${X}]\"\n",
		// bash: /root/sub — a quote or an escape glued to a substitution's
		// `)` continues the right-hand side, and reads as the verb.
		"a right-hand side continued by a quote past a substitution":   "X=$(printf /root)\"/sub\"\necho \"[${X}]\"\n",
		"a right-hand side continued by an escape past a substitution": "X=$(printf /root)\\/sub\necho \"[${X}]\"\n",
		// bash: aandb — the `&` inside `${…}` reads as a lone `&`, whose
		// command's assignments are dropped (dash: Bad substitution).
		"an unquoted & inside a parameter expansion": "X=${Y//&/and}\necho \"[${X}]\"\n",
		// bash: b — `for NAME do` iterates the positional parameters `set --`
		// gave the body, and an `in` on the next line still binds the list;
		// a `for` variable is recorded only when `in` is its next word.
		"a for loop without in over set -- parameters": "set -- a b\nfor X do :; done\necho \"[${X}]\"\n",
		"a for loop whose in is on the next line":      "for X\nin a b; do :; done\necho \"[${X}]\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			r := compileText(t, bracedEnvSrc("tool gate:\n  command: |\n"+indentToolBody(body)))
			if got := bracedEnvDiags(r); len(got) != 0 {
				t.Fatalf("C188 fired on a documented miss — a real defeat left unreported by design; firing here needs the evidence that the same shape fires on no correct body: %s", got[0].Message)
			}
		})
	}
}

// TestBracedEnvDefeatsAssignment_ShScriptMissesStaySilent: sh keeps a
// prefix assignment before a special builtin — dash and `bash --posix`
// leave X set after `X=1 :` or `X=1 exec` — where bash keeps none, and the
// scanner reads every body the way bash does. In a `script:` run by sh the
// rows below are therefore real defeats C188 leaves unreported (listed in
// docs/workflow_authoring_pitfalls.md); in a `command:`, which bash runs,
// the bare-exec text is a silent row of SilentWhereNoAssignmentPersists.
//
// Mutations that redden it: stop reading a command modifier as the verb
// (the bare-exec row fires), drop dropPrefix from the verb (the `:` row
// fires).
func TestBracedEnvDefeatsAssignment_ShScriptMissesStaySilent(t *testing.T) {
	for name, body := range map[string]string{
		// dash: [1]
		"a prefix before a bare exec": "X=1 exec\necho \"[${X}]\"\n",
		// dash: [1]
		"a prefix before the colon builtin": "X=1 :\necho \"[${X}]\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			r := compileText(t, bracedEnvSrc("tool gate:\n  script: |\n"+indentToolBody(body)+"  language: sh\n"))
			if got := bracedEnvDiags(r); len(got) != 0 {
				t.Fatalf("C188 fired on a documented sh miss — a real defeat left unreported by design: %s", got[0].Message)
			}
		})
	}
}

// TestBracedEnvDefeatsAssignment_KnownLimitsStillFire: correct bodies C188
// still fires on. In each row the assignment never reaches the read — run
// as written and as the engine rewrites it, under bash and dash for a
// `command:` row and under dash for the sh `script:` row, the body prints
// the same thing — but the scanner records an assignment where it is
// written, not where it runs: it keeps no function scope, no pipeline or
// background scope for a compound command or an and-list whose `|` or `&`
// comes after the assignment, and reads neither an extglob pattern nor a
// quote nested inside a substitution's quoted substitution. The rows are
// the "Known limits" of docs/workflow_authoring_pitfalls.md and pin that
// list: a change that silences one moves its row to
// SilentWhereNoAssignmentPersists, with the evidence that it records no less
// on the genuine shapes beside it, and takes it out of the docs.
//
// Mutation that reddens it: record no assignment at all (dischargeAssigns
// keeps nothing) — every row goes quiet, and names the stale list.
func TestBracedEnvDefeatsAssignment_KnownLimitsStillFire(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		sh   bool   // a `script:` run by sh, instead of a `command:`
		name string // the name the single finding carries
	}{
		"a function's export read before the first call": {
			body: "setup() { export X=1; }\necho \"${X:-unset}\"\nsetup\n", name: "X",
		},
		"a (pat)-case substitution in a function, read before the call": {
			body: "detect() {\n  X=$(case \"$v\" in (a) echo one;; esac)\n}\necho \"[${X:-unset}]\"\ndetect\n", name: "X",
		},
		"a pipeline loop's name read after done": {
			body: "printf 'l1\\n' | while read -r L; do :; done\necho \"[${L}]\"\n", name: "L",
		},
		"a pipeline loop's assignment read after done": {
			body: "printf 'a\\n' | while read -r L; do X=$(case \"$L\" in (a) echo one;; esac); done\necho \"[${X}]\"\n", name: "X",
		},
		"a brace group in a pipeline": {
			body: "{ X=1; } | cat\necho \"[${X}]\"\n", name: "X",
		},
		"an and-list backgrounded after its assignment": {
			body: "X=1 && true &\nwait\necho \"[${X}]\"\n", name: "X",
		},
		"a brace group backgrounded": {
			body: "{ X=1; } &\nwait\necho \"[${X}]\"\n", name: "X",
		},
		"a for loop backgrounded": {
			body: "for X in a b; do :; done &\nwait\necho \"[${X}]\"\n", name: "X",
		},
		"an export of a function's local": {
			body: "f() { local X; export X=1; }\nf\necho \"[${X}]\"\n", name: "X",
		},
		"an extglob pattern inside a substitution": {
			body: "shopt -s extglob\nOUT=$(case \"$v\" in @(a|b)) X=1;; esac; echo hi)\necho \"[${X}] $OUT\"\n", name: "X",
		},
		"a paren in a quoted substitution inside a substitution": {
			body: "OUT=$(MSG=\"$(printf '%s' \")\")\"; X=1; echo \"$MSG\")\necho \"[${X}] $OUT\"\n", name: "X",
		},
		// bash: [1], a real defeat; dash reads `&` then `>/dev/null`.
		"an &> redirection under sh": {
			body: "X=1 &>/dev/null\necho \"[${X}]\"\n", sh: true, name: "X",
		},
	} {
		t.Run(name, func(t *testing.T) {
			tool := "tool gate:\n  command: |\n" + indentToolBody(tc.body)
			if tc.sh {
				tool = "tool gate:\n  script: |\n" + indentToolBody(tc.body) + "  language: sh\n"
			}
			r := compileText(t, bracedEnvSrc(tool))
			got := bracedEnvDiags(r)
			if len(got) != 1 {
				t.Fatalf("a known limit no longer fires (got %d findings): move the row to the silent table and take it out of the docs' Known limits, with the evidence: %v", len(got), got)
			}
			if !strings.Contains(got[0].Message, "${"+tc.name) {
				t.Fatalf("finding %q does not name ${%s}", got[0].Message, tc.name)
			}
		})
	}
}

// TestBracedEnvDefeatsAssignment_RemedyFitsTheForm: the remedy a diagnostic
// offers must be sound on the form it fired on. Dropping the braces is the
// whole fix only where they carry nothing else:
//
//   - `"${COUNT}_items"` — a bare `$COUNT_items` names ANOTHER variable
//     (bash reads the longest name), so the body prints nothing where it
//     printed `7_items`; the fix is to close the word (`"$COUNT"_items`).
//   - `"${COUNT:-0}"` — a bare `$COUNT` drops the fallback, so the body
//     prints nothing where it printed `0`; the fix spells the default as a
//     statement the shell runs (`[ -z "$COUNT" ] && COUNT="0"`), the shape
//     golden-master's own rule names.
//
// One finding covers every defeating read of the name, so its remedy has to
// be sound on all of them: a body reading `${COUNT}` on one line and
// `${COUNT}_items` on the next carries the glue clause whichever order they
// are in, and the clause says WHERE it applies rather than asserting it of
// the read the finding sits on.
//
// Mutations that redden it: drop either clause (the glue test, or the `:-`
// arm's) — the row for it keeps the bare-only advice, which breaks the body
// it is given to; or read the glue off the REPORTED read alone instead of
// every defeating read of the name — the two-read rows lose it in one of
// the two orders.
func TestBracedEnvDefeatsAssignment_RemedyFitsTheForm(t *testing.T) {
	for name, tc := range map[string]struct {
		body   string
		want   []string
		unwant []string
	}{
		"an identifier byte follows the brace": {
			body:   "COUNT=7\necho \"${COUNT}_items\"\n",
			want:   []string{"closing the word yourself (\"$COUNT\"…)", "part of the name"},
			unwant: []string{"so the shell answers", "drops the default"},
		},
		"a digit follows the brace": {
			body:   "N=1\necho \"${N}2\"\n",
			want:   []string{"closing the word yourself (\"$N\"…)"},
			unwant: []string{"so the shell answers"},
		},
		"a default the body would lose": {
			body:   "COUNT=$(ls | wc -l)\necho \"${COUNT:-0}\"\n",
			want:   []string{"[ -z \"$COUNT\" ] && COUNT=\"…\"", "drops the default"},
			unwant: []string{"so the shell answers", "closing the word"},
		},
		"both at once": {
			body:   "COUNT=$(ls | wc -l)\necho \"${COUNT:-0}_items\"\n",
			want:   []string{"closing the word yourself (\"$COUNT\"…)", "[ -z \"$COUNT\" ] && COUNT=\"…\""},
			unwant: []string{"so the shell answers"},
		},
		"a non-identifier byte follows, and no default": {
			body:   "COUNT=7\necho \"${COUNT} items\"\n",
			want:   []string{"Read the variable bare ($COUNT) so the shell answers"},
			unwant: []string{"closing the word", "drops the default"},
		},
		"a second read is the glued one": {
			body:   "COUNT=7\necho \"${COUNT}\"\necho \"${COUNT}_items\"\n",
			want:   []string{"closing the word yourself (\"$COUNT\"…)", "wherever an identifier byte follows the brace"},
			unwant: []string{"so the shell answers"},
		},
		"a second read is the glued one, under a default arm": {
			body:   "COUNT=7\necho \"${COUNT:-0}\"\necho \"${COUNT}_items\"\n",
			want:   []string{"closing the word yourself (\"$COUNT\"…)", "[ -z \"$COUNT\" ] && COUNT=\"…\""},
			unwant: []string{"so the shell answers"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := compileText(t, bracedEnvSrc("tool gate:\n  command: |\n"+indentToolBody(tc.body)))
			got := bracedEnvDiags(r)
			if len(got) != 1 {
				t.Fatalf("want one finding, got %d: %v", len(got), got)
			}
			for _, w := range tc.want {
				if !strings.Contains(got[0].Message, w) {
					t.Errorf("message %q does not carry the remedy this form needs (%q)", got[0].Message, w)
				}
			}
			for _, u := range tc.unwant {
				if strings.Contains(got[0].Message, u) {
					t.Errorf("message %q carries %q, advice that is unsound or irrelevant on this form", got[0].Message, u)
				}
			}
		})
	}
}

// TestBracedEnvDefeatsAssignment_ReportsTheStrongestArm: a body reading one
// name in BOTH arms is defeated unconditionally — the `:-` form resolves
// whatever the engine's environment holds — so that is the arm reported,
// whichever order the author wrote the two reads in. Reporting the first
// read's arm instead would announce the certain defeat of #1473
// (`[ -n "${RUN_ID:-}" ] || exit 1` under a plain `log "${RUN_ID}"`) as a
// latent one, and make the certainty depend on line order.
//
// Mutation that reddens it: keep the first qualifying read's arm (drop the
// `r.HasDefault && !cur.hasDefault` promotion) — the plain-first row goes
// latent, and names the order-dependence.
func TestBracedEnvDefeatsAssignment_ReportsTheStrongestArm(t *testing.T) {
	for name, tc := range map[string]struct {
		body     string
		wantLine string
	}{
		"plain read first, default arm below": {
			body:     "RUN_ID=$(date +%s)\nlog \"${RUN_ID}\"\n[ -n \"${RUN_ID:-}\" ] || exit 1\n",
			wantLine: "body line 3",
		},
		"default arm first, plain read below": {
			body:     "RUN_ID=$(date +%s)\n[ -n \"${RUN_ID:-}\" ] || exit 1\nlog \"${RUN_ID}\"\n",
			wantLine: "body line 2",
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := compileText(t, bracedEnvSrc("tool gate:\n  command: |\n"+indentToolBody(tc.body)))
			got := bracedEnvDiags(r)
			if len(got) != 1 {
				t.Fatalf("want one finding for the two reads of RUN_ID, got %d: %v", len(got), got)
			}
			msg := got[0].Message
			if !strings.Contains(msg, "unconditionally") || !strings.Contains(msg, "${RUN_ID:-…}") {
				t.Errorf("message %q reports a latent defeat on a body the engine defeats unconditionally", msg)
			}
			if !strings.Contains(msg, tc.wantLine) {
				t.Errorf("message %q does not sit on the `:-` read (%s)", msg, tc.wantLine)
			}
		})
	}
}

// TestBracedEnvDefeatsAssignment_IntersectsOnThePositionTheEngineConsults:
// the two halves of the read set meet on an OFFSET, not on a name. On
// `${A:-${B:-c}}` the engine's segment ends at the first `}`, so it consults
// A there and never B — the inner `${B:-c}` is text it hands to the shell.
// A body that also reads `${B}` plainly further down therefore carries one
// finding for B, on THAT line and with the plain arm: an intersection by
// name would put B's finding on the nested line and call its defeat
// unconditional, which is bash's business there, not the engine's.
//
// Mutation that reddens it: intersect the halves by name (accept any read
// whose name the engine consults somewhere) — B's finding moves onto the
// nested line and claims the `:-` arm.
func TestBracedEnvDefeatsAssignment_IntersectsOnThePositionTheEngineConsults(t *testing.T) {
	r := compileText(t, bracedEnvSrc("tool gate:\n  command: |\n"+indentToolBody(
		"A=1\nB=1\necho \"${A:-${B:-c}}\"\necho \"${B}\"\n")))
	got := bracedEnvDiags(r)
	if len(got) != 2 {
		t.Fatalf("want one finding per name, got %d: %v", len(got), got)
	}
	byName := map[string]string{}
	for _, d := range got {
		switch {
		case strings.Contains(d.Message, "${A"):
			byName["A"] = d.Message
		case strings.Contains(d.Message, "${B"):
			byName["B"] = d.Message
		}
	}
	if a := byName["A"]; !strings.Contains(a, "body line 3") || !strings.Contains(a, "${A:-…}") {
		t.Errorf("A's finding %q is not the outer default arm on line 3", a)
	}
	b := byName["B"]
	if !strings.Contains(b, "body line 4") {
		t.Errorf("B's finding %q does not sit on the plain read the engine actually consults (line 4)", b)
	}
	if strings.Contains(b, "unconditionally") || strings.Contains(b, "${B:-…}") {
		t.Errorf("B's finding %q claims the nested `:-` form the engine never reaches as a segment", b)
	}
}

// indentToolBody indents a shell body into a `command: |` block.
func indentToolBody(body string) string {
	var b strings.Builder
	for _, l := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		b.WriteString("    " + l + "\n")
	}
	return b.String()
}
