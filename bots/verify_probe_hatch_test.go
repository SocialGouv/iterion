package bots

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"unicode"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// A repo with genuinely nothing to build gets an EMPTY GATE: a verify.sh that
// echoes that fact and exits 0. It is the prescribed escape hatch — an explicit
// empty gate leaves a reviewable artefact where a missing script leaves nothing
// — and it is also a green that certifies nothing about the tree. The reuse
// pre-check used to decide reuse on SHAPE alone (iteration>0, non-empty,
// `sh -n`), so the hatch became the run's deterministic verdict for every later
// pass. The verdict of the pass that WRITES it is unchanged and measured here
// (TestTheWritingPassStillGreensTheEmptyGate): `passed: true, skipped: false`
// over a committed CMake application the gate never builds, with
// `skipped: false` leaving no reader downstream able to tell.
//
// hatchMarker is the line the author writes to say "this gate is empty on
// purpose". The pre-check answers fresh=false for a script carrying it, so
// verify_build re-decides against the CURRENT tree on every pass.
const hatchMarker = "# iterion: no-build-system"

// decidesTheReuse discriminates the reuse pre-check from every other tool node:
// it syntax-checks the agent-written script instead of running it (verify_run
// runs it, `['sh', script]`), and it answers with a `fresh` verdict that routes
// the graph — `verify_probe -> verify_run when fresh`, `-> verify_build when not
// fresh`. The name is NOT the discriminator: a roster is how the sibling guard
// drifted and omitted app-dev for a whole release.
const (
	decidesTheReuse  = "['sh', '-n', script]"
	printsTheVerdict = "'fresh'"
)

// reusePrecheck is one discovered pre-check: the bundle it belongs to, the node
// name, its REAL compiled command and the bot's own var defaults.
type reusePrecheck struct {
	rel     string
	node    string
	command string
	vars    map[string]string
	wf      *ir.Workflow
}

// discoverReusePrechecks compiles every bundle and returns each tool node that
// decides whether to reuse an already-authored verify.sh.
func discoverReusePrechecks(t *testing.T) []reusePrecheck {
	t.Helper()
	mains, err := filepath.Glob("*/main.bot")
	if err != nil {
		t.Fatal(err)
	}
	var found []reusePrecheck
	declares := map[string]bool{}
	matched := map[string]bool{}
	for _, rel := range mains {
		pr := parseBotUnit(rel)
		if pr.File == nil {
			continue
		}
		cr := ir.Compile(pr.File)
		if cr.Workflow == nil {
			continue
		}
		// A SECOND, independent signal that this bundle reuses a verify.sh: it
		// declares the pre-check's own result schema. Independent of how the
		// command spells its subprocess, which is the way the discriminator
		// above can go stale without anyone noticing.
		if cr.Workflow.Schemas["verify_probe_state"] != nil {
			declares[rel] = true
		}
		defaults := map[string]string{}
		for name, v := range cr.Workflow.Vars {
			if v != nil && v.HasDefault {
				defaults[name] = fmt.Sprintf("%v", v.Default)
			}
		}
		for node, raw := range cr.Workflow.Nodes {
			tn, ok := raw.(*ir.ToolNode)
			if !ok {
				continue
			}
			if !strings.Contains(tn.Command, decidesTheReuse) || !strings.Contains(tn.Command, printsTheVerdict) {
				continue
			}
			found = append(found, reusePrecheck{rel: rel, node: node, command: tn.Command, vars: defaults, wf: cr.Workflow})
			matched[rel] = true
		}
	}
	// Node maps iterate in random order; the subtest order must not be a coin flip.
	sort.Slice(found, func(i, j int) bool {
		if found[i].rel != found[j].rel {
			return found[i].rel < found[j].rel
		}
		return found[i].node < found[j].node
	})
	// The cross-check runs FIRST because it names the culprit: a bundle that
	// declares the verdict schema and matched no node of the command
	// discriminator either spells its subprocess differently or ships the
	// latch unguarded.
	var unmatched []string
	for rel := range declares {
		if !matched[rel] {
			unmatched = append(unmatched, rel)
		}
	}
	if len(unmatched) > 0 {
		sort.Strings(unmatched)
		t.Fatalf("%v declare a verify_probe_state schema and no node of theirs matched %q — either the "+
			"discriminator is stale or these carriers decide reuse unguarded", unmatched, decidesTheReuse)
	}
	// And a floor, because the other failure mode of a discriminator is to
	// match nothing at all and leave a vacuous guard looking green.
	if len(found) < 5 {
		t.Fatalf("discovered %d reuse pre-checks, want >= 5 — the discriminator %q is stale and this "+
			"guard is near-vacuous", len(found), decidesTheReuse)
	}
	return found
}

type probeVerdict struct {
	Fresh  bool   `json:"fresh"`
	Reason string `json:"reason"`
}

// fillCommand binds a discovered carrier's REAL compiled command to the
// fixture: the paths FIRST, then every other placeholder from the bot's own var
// defaults. workspace_dir and scratch_dir carry engine defaults
// (${PROJECT_DIR}, ${PROJECT_SCRATCH_DIR}); letting the generic loop reach them
// substitutes a template the shell then expands to nothing, and the node looks
// in the wrong directory — an answer for a reason that has nothing to do with
// what is being tested.
func fillCommand(t *testing.T, c reusePrecheck, command, ws, scratch string) string {
	t.Helper()
	cmd := strings.ReplaceAll(command, "{{vars.workspace_dir}}", ws)
	cmd = strings.ReplaceAll(cmd, "{{vars.scratch_dir}}", scratch)
	// Pass 2: pass 1 of every run re-authors the script, so the reuse path only
	// exists for iteration>0 and a fixture left at 0 would answer fresh=false
	// for a reason that has nothing to do with the hatch.
	cmd = strings.ReplaceAll(cmd, "{{input.iteration}}", "1")
	for name, val := range c.vars {
		if name == "workspace_dir" || name == "scratch_dir" {
			continue
		}
		cmd = strings.ReplaceAll(cmd, "{{vars."+name+"}}", val)
	}
	if rest := strings.Index(cmd, "{{"); rest >= 0 {
		t.Fatalf("a template ref survived substitution near %q — the node would decide in a state "+
			"production never reaches", cmd[rest:min(rest+60, len(cmd))])
	}
	return cmd
}

// runProbe executes a pre-check's REAL compiled command against the fixture and
// returns its verdict.
func runProbe(t *testing.T, c reusePrecheck, ws, scratch string) probeVerdict {
	t.Helper()
	out := runNodeCommand(t, c, c.command, c.node, ws, scratch)
	var res probeVerdict
	if uerr := json.Unmarshal(out, &res); uerr != nil {
		t.Fatalf("%s output is not the verify_probe_state JSON: %v (out %q)", c.node, uerr, out)
	}
	return res
}

// runNodeCommand runs one bound command under `sh -c`, the way the engine runs
// a tool node, and returns its stdout.
func runNodeCommand(t *testing.T, c reusePrecheck, command, node, ws, scratch string) []byte {
	t.Helper()
	out, err := exec.Command("sh", "-c", fillCommand(t, c, command, ws, scratch)).Output()
	if err != nil {
		stderr := ""
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		t.Fatalf("%s command failed to execute: %v (out %q, stderr %q)", node, err, out, stderr)
	}
	return out
}

// TestVerifyProbeNeverReusesAnEmptyGate stands every DISCOVERED pre-check up
// against the hostile condition the ticket names — a hatch authored on pass 1,
// a build system landing afterwards — and demands the verdict from the product
// itself. A check that the marker string appears somewhere would not do: the
// question is what the running command answers.
//
// The cooperative half is asserted in the same loop, because a pre-check that
// answered fresh=false for everything would pass the hostile half while
// throwing away what it exists for (verify_build is an LLM agent, ~$0.69/13k
// tok EVERY pass).
func TestVerifyProbeNeverReusesAnEmptyGate(t *testing.T) {
	for _, bin := range []string{"python3", "sh"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH", bin)
		}
	}
	cases := []struct {
		name      string
		script    string
		wantFresh bool
		why       string
	}{
		{
			name:      "a_marked_hatch_is_re_decided",
			script:    "#!/bin/sh\n" + hatchMarker + "\necho 'this repo has no build or test system yet'\nexit 0\n",
			wantFresh: false,
			why: "the author marked this gate empty on purpose; reusing it makes an empty verdict the " +
				"run's truth for every later pass, and the tree it was written for is gone",
		},
		{
			// The marker arm and the allowlist would otherwise cover each other:
			// the hatch above is empty BY CONTENT too, so removing the marker arm
			// leaves it refused and that arm looks tested while nothing tests it.
			// This scenario isolates the marker — a script that carries it AND can
			// go red.
			name:      "a_marked_script_is_re_decided_even_when_it_can_fail",
			script:    "#!/bin/sh\n" + hatchMarker + "\nset -e\nmake lint\n",
			wantFresh: false,
			why: "the marker is the author's declaration that this tree has nothing to build, and the " +
				"tree is what moved; it must decide on its own, without the line allowlist",
		},
		{
			// The marker is written by an LLM from prose, so it is written
			// approximately: a capital, a missing space, an underscore for a
			// hyphen. An exact substring made each of those a silent reuse of the
			// pure hatch — #1707's latch, with the prescription followed.
			name:      "a_capitalised_marker_is_still_the_marker",
			script:    "#!/bin/sh\n# Iterion: no-build-system\necho \"No build system detected; nothing to verify.\"\nexit 0\n",
			wantFresh: false,
			why: "the marker is matched with the case folded, because a capital is exactly the deviation " +
				"prose produces — and the allowlist cannot cover for it here, the message carries a `;`",
		},
		{
			// A near-miss on a script that CAN fail: the allowlist cannot answer
			// for this one, so it attributes the tolerant match to the marker arm
			// alone.
			name:      "a_marker_without_the_space_is_still_the_marker",
			script:    "#!/bin/sh\n# iterion:no-build-system\nset -e\nmake lint\n",
			wantFresh: false,
			why: "a missing space is not a different declaration; nothing but the marker arm can refuse " +
				"this script, so the fold is measured where it decides alone",
		},
		{
			name:      "a_marker_written_with_underscores_is_still_the_marker",
			script:    "#!/bin/sh\n# ITERION: NO_BUILD_SYSTEM\necho \"No build system detected; nothing to verify.\"\nexit 0\n",
			wantFresh: false,
			why: "every non-alphanumeric character is dropped before the match, so an underscore for a " +
				"hyphen declares the same thing",
		},
		{
			// The other side of the fold: a LINE is the unit, so a script that
			// merely spells the keys on two lines is not marked. The halves are
			// ADJACENT in the folded copy — nothing but the line break separates
			// them — which is what makes this row the one that reddens when the
			// unit stops being the line.
			name:      "a_real_gate_that_spells_the_marker_halves_apart_is_still_reused",
			script:    "#!/bin/sh\nset -e\necho 'the iterion:\nno-build-system note does not apply'\nmake build\n",
			wantFresh: true,
			why: "the fold drops punctuation, not line breaks: a gate that builds must not be refused " +
				"because one of its messages spells the marker across two lines",
		},
		{
			// The paraphrase the prompt itself invites: verify-build.md says
			// `no build/test system` two lines above the marker it prescribes.
			// An exact tail made it an UNMARKED gate, and the allowlist cannot
			// cover for it — the prescribed message carries a `;`.
			name:      "a_marker_paraphrasing_the_skills_own_wording_is_still_the_marker",
			script:    "#!/bin/sh\n# iterion: no build/test system\necho \"No build system detected; nothing to verify.\"\nexit 0\n",
			wantFresh: false,
			why: "the keys are `iterion` and `no build`, so a word inserted into the declaration does " +
				"not undo it — the skill's own phrasing declares the same thing",
		},
		{
			name:      "a_misspelled_marker_is_still_the_marker",
			script:    "#!/bin/sh\n# iterion: no-build-sytem\necho \"No build system detected; nothing to verify.\"\nexit 0\n",
			wantFresh: false,
			why: "a letter dropped from the tail is a typo, not a different declaration; nothing but " +
				"the marker arm can refuse this script, so the keys are measured where they decide alone",
		},
		{
			name:      "a_marker_whose_halves_are_swapped_is_still_the_marker",
			script:    "#!/bin/sh\n# no-build-system (iterion)\necho \"No build system detected; nothing to verify.\"\nexit 0\n",
			wantFresh: false,
			why: "both keys on one line declare the empty gate whatever their order, which is how prose " +
				"writes it when the product name comes last",
		},
		{
			// THE LIMIT of the tolerant match, held by measurement so it cannot
			// drift in silence: the keys are matched as letters, and `iteration`
			// does not carry `iterion`. The message's `;` makes the line a real
			// gate for the allowlist, so this script IS reused — #1707's latch,
			// reachable only by misspelling the product name itself.
			name:      "a_marker_with_a_different_product_name_keeps_the_reuse",
			script:    "#!/bin/sh\n# iteration: no-build-system\necho \"No build system detected; nothing to verify.\"\nexit 0\n",
			wantFresh: true,
			why: "the fold covers case and punctuation, not spelling: `iteration` is not `iterion`, so " +
				"this is an unmarked gate whose punctuated message the allowlist reads as a real one",
		},
		{
			// The keys are two words, so the author who writes them as two
			// words declares the same thing. Named by the match's own rule:
			// a key must begin where a word begins, and both of these do.
			name:      "a_marker_written_as_two_bare_words_is_still_the_marker",
			script:    "#!/bin/sh\n# iterion nobuild\nset -e\nmake lint\n",
			wantFresh: false,
			why: "`iterion` and `no build` are what the match looks for, folded: an author who writes " +
				"them as two bare words has declared the empty gate as surely as the prescribed line",
		},
		{
			// Where the key STOPS is not a boundary: `no-buildsystem` folds to
			// one word for its tail, and the declaration is the same. The key
			// must begin where a word begins; requiring it to END on one too
			// would read this line as an unmarked gate.
			name:      "a_marker_whose_tail_is_glued_is_still_the_marker",
			script:    "#!/bin/sh\n# iterion: no-buildsystem\nset -e\nmake lint\n",
			wantFresh: false,
			why: "prose glues words as readily as it splits them; a key anchored at both ends would " +
				"lose the marker here, and nothing but the marker arm can refuse this script",
		},
		{
			// THE COST of keying on `iterion`: matched anywhere in the folded
			// line, the product's name sits inside the English word
			// `criterion`, and a gate that builds pays an LLM pass every
			// iteration for the comment above it. A key is matched where a
			// WORD begins, so `criterion` names no product.
			name:      "a_real_gate_whose_comment_says_criterion_is_still_reused",
			script:    "#!/bin/sh\nset -e\n# the acceptance criterion: no build warnings\nmake build\n",
			wantFresh: true,
			why: "`criterion` ends with the product's name and names no product: a key is matched " +
				"where a word begins, so this gate builds and is reused",
		},
		{
			// The boundary of the OTHER key, in the widening direction: a line
			// that names the product and a build without denying one. A
			// recogniser keyed on `iterion` + `build` instead of `iterion` +
			// `no build` passes every other row and refuses this gate.
			name:      "a_real_gate_naming_the_product_and_a_build_is_still_reused",
			script:    "#!/bin/sh\nset -e\n# iterion build helper\nmake build\n",
			wantFresh: true,
			why: "naming the product next to a build is not declaring there is none — a key wide " +
				"enough to call this a marker refuses a working gate on every iteration",
		},
		{
			// The second marker-only scenario, and the one that says why the
			// prescription leads with the marker: the message a skill asks for is
			// English prose, prose carries punctuation, and a `;` makes the line a
			// real gate for the allowlist. The marker carries the decision anyway.
			name:      "a_marked_hatch_whose_message_is_punctuated_is_re_decided",
			script:    "#!/bin/sh\n" + hatchMarker + "\necho \"No build system detected; nothing to verify.\"\nexit 0\n",
			wantFresh: false,
			why: "the allowlist reads whole lines, so the `;` in this message makes it a real gate — the " +
				"marker is what refuses the hatch the prompt and the skill prescribe",
		},
		{
			// The allowlist's own scenario: no marker, so the marker arm cannot
			// answer for it. Deleting the allowlist arm reddens here and nowhere
			// else.
			name:      "an_unmarked_echo_and_exit_gate_is_re_decided",
			script:    "#!/bin/sh\nset -e\necho 'nothing to build in this repo'\nexit 0\n",
			wantFresh: false,
			why: "shell options, a plain echo and a zero exit are the whole body — nothing here can fail, " +
				"so its green says nothing about the tree and must not become a standing verdict",
		},
		{
			name:      "an_unmarked_comment_only_script_is_re_decided",
			script:    "#!/bin/sh\n# no build system in this repo yet\n# nothing to verify\n",
			wantFresh: false,
			why: "a script of comments runs nothing at all — the plainest empty gate there is, and a " +
				"reader that takes a `#` line for a command head calls it a real one",
		},
		{
			name:      "shell_options_and_a_printf_are_still_an_empty_gate",
			script:    "#!/bin/sh\nset -eu\nset -o pipefail\nprintf 'no build system here'\nexit 0\n",
			wantFresh: false,
			why: "`set -<flags>`, `set -o <opt>` and `printf` are heads the allowlist names, each whole, " +
				"and none of them can RUN a command — `set -o pipefail` is not POSIX and reds under " +
				"dash, and `printf '%d' abc` reds anywhere, so this script is refused for the cheap " +
				"reason, not for an exact one",
		},
		{
			// One byte, and the reader used to lose its whole backstop: a BOM
			// survives `encoding='utf-8'` as a character, U+FEFF is not
			// whitespace, so `\ufeff#!/bin/sh` was a command head the allowlist
			// does not name and the hatch read as a real gate. Any Windows editor
			// and any `utf-8-sig` writer emits one.
			name:      "an_unmarked_hatch_behind_a_byte_order_mark_is_re_decided",
			script:    "\ufeff#!/bin/sh\necho 'nothing to build'\nexit 0\n",
			wantFresh: false,
			why: "the script is read with the BOM stripped, so the shebang is a comment like any other " +
				"and the body is the no-ops it looks like",
		},
		{
			// A lone CR is not a command separator for sh: the rest of the
			// physical line is one more argument to the echo, and `make` never
			// runs (measured: `sh verify.sh` exits 0 and builds nothing, where
			// the same script with a newline runs make). Python's universal
			// newlines translate that CR into a line break, which would show
			// the pre-check a `make build` line the shell does not have and
			// reuse an always-green gate — the ticket's latch, one byte wide.
			name:      "a_lone_carriage_return_does_not_manufacture_a_line",
			script:    "#!/bin/sh\nset -e\necho \"nothing to build\"\rmake build\nexit 0\n",
			wantFresh: false,
			why: "the script is read with its own line breaks, so the reader judges the lines sh " +
				"judges: this one echoes and exits, and nothing in it can go red",
		},
		{
			// The control for the read above: a CRLF file still splits on its
			// line breaks, and its no-op body is still recognised.
			name:      "a_crlf_hatch_is_still_an_empty_gate",
			script:    "#!/bin/sh\r\necho 'nothing to build'\r\nexit 0\r\n",
			wantFresh: false,
			why: "a CRLF script is a script whose lines end in a carriage return — the reader " +
				"strips it like any other trailing blank and reads the no-ops it has",
		},
		{
			name:      "a_bare_truth_after_an_indented_comment_is_still_an_empty_gate",
			script:    "#!/bin/sh\n   # nothing to build\ntrue\n:\nexit\n",
			wantFresh: false,
			why: "a comment keeps being a comment when it is indented, and `true`, `:` and a bare `exit` " +
				"are the no-ops the allowlist names",
		},
		{
			// `echo x # y` runs nothing whether sh takes the tail for a comment or
			// for more arguments, and the allowlist says so by reading the line
			// whole: the head is echo and no gate character follows.
			name:      "an_unmarked_echo_with_a_trailing_comment_is_still_an_empty_gate",
			script:    "#!/bin/sh\necho 'nothing to build' # really nothing\nexit 0\n",
			wantFresh: false,
			why:       "an echo with a comment after it is still an echo — the line runs nothing that can fail",
		},
		{
			name:      "a_real_gate_is_still_reused",
			script:    "#!/bin/sh\nset -e\nmake build\nmake test\n",
			wantFresh: true,
			why:       "a gate that can go red is what the pre-check exists to reuse; refusing it pays an LLM pass for nothing",
		},
		{
			name:      "a_real_gate_that_also_echoes_is_still_reused",
			script:    "#!/bin/sh\nset -e\necho 'building'\nmake build\necho 'testing'\nmake test\n",
			wantFresh: true,
			why:       "progress echoes around a real build must not read as an empty gate",
		},
		{
			// The echoes above sit on their own lines. This is the shape that
			// needs the gate characters: a non-failing head FIRST and the real
			// command after it, on one line.
			name:      "a_real_gate_chained_after_an_echo_is_still_reused",
			script:    "#!/bin/sh\nset -e\necho 'building'; make build && make test\n",
			wantFresh: true,
			why: "the statement after a `;` or an `&&` is what can go red — a reader that judged only the " +
				"line's first head would call this real gate empty and pay an LLM pass for nothing",
		},
		{
			name:      "a_hash_inside_a_quoted_message_keeps_the_reuse",
			script:    "#!/bin/sh\nset -e\necho \"=== build # 1 ===\"; make build\n",
			wantFresh: true,
			why: "the `;` is what decides this line, and it decides it the safe way: a reader that cut the " +
				"line at the quoted `#` would discard `make build` and refuse a real gate",
		},
		{
			name:      "a_command_substitution_counts_as_a_real_gate",
			script:    "#!/bin/sh\necho \"$(make -s smoke)\"\nexit 0\n",
			wantFresh: true,
			why: "the pre-check does not read what a substitution runs, so it must not call that " +
				"script empty — unrecognised shapes keep the reuse",
		},
		{
			name:      "a_backtick_substitution_counts_as_a_real_gate",
			script:    "#!/bin/sh\nset -e\necho `make -s smoke`\n",
			wantFresh: true,
			why: "a backtick runs a command the reader cannot read, exactly like $(…): an " +
				"unrecognised shape keeps the reuse",
		},
		{
			// An `&` backgrounds the echo and leaves `make build` to run: the gate
			// is real, and the line allowlist keeps it by refusing to read past
			// the `&` at all.
			name:      "a_backgrounded_echo_before_a_real_gate_keeps_the_reuse",
			script:    "#!/bin/sh\necho 'starting' & make build\n",
			wantFresh: true,
			why: "`make build` runs here, so refusing this script pays an LLM pass every iteration for a " +
				"gate that works — the `&` makes the line a real gate and that is the right answer",
		},
		{
			name:      "a_test_builtin_keeps_the_reuse",
			script:    "#!/bin/sh\n[ -d . ]\necho 'nothing to build'\nexit 0\n",
			wantFresh: true,
			why:       "a test builtin can go red, so it is not on the allowlist and the script reads as a real gate",
		},
		{
			// Heredoc DATA, which no reader of this script is taught to skip:
			// the allowlist keeps the reuse here because the heredoc opens on a
			// `<` and `make build` is a head it does not know — two independent
			// reasons, where a reader tracking quotes across newlines swallowed
			// the gate in the apostrophe of `don't` and refused a script that
			// really builds.
			name:      "a_real_gate_after_heredoc_data_is_still_reused",
			script:    "#!/bin/sh\nset -e\necho 'x' <<EOF\necho don't\nEOF\nmake build\n# don't bother\n",
			wantFresh: true,
			why: "`make build` runs here — a reader that lost it in the heredoc's apostrophe refused a " +
				"real gate and paid an LLM pass every iteration for it",
		},
		{
			// #1707's own reproduction, unmarked: the one closed form the
			// allowlist reads past a `;`. The message is a quoted run with no
			// substitution in it, the rest of the line is `exit 0`, and the
			// line runs nothing (measured under sh: exit 0, no command).
			name:      "the_tickets_unmarked_one_liner_is_re_decided",
			script:    "#!/bin/sh\necho \"this repo has no build or test system yet\"; exit 0\n",
			wantFresh: false,
			why: "the ticket's own script: a message and a zero exit on one line can no more fail " +
				"than the same two lines apart, so its green must not become the run's verdict",
		},
		{
			name:      "the_one_liner_single_quoted_is_re_decided",
			script:    "#!/bin/sh\necho 'nothing to build'; exit 0\n",
			wantFresh: false,
			why:       "a single-quoted message is a message: nothing in it can be expanded into a command",
		},
		{
			name:      "the_one_liner_with_a_bare_message_is_re_decided",
			script:    "#!/bin/sh\necho nothing to build; exit 0\n",
			wantFresh: false,
			why: "an unquoted message carrying no gate character is a message too — the words are " +
				"arguments to echo, whatever the shell globs among them",
		},
		{
			// The `;` INSIDE the message is what the whole-line rule cannot
			// read anywhere else; inside the closed form the quotes bound it,
			// and this is the exact message 13 verify skills prescribe.
			name:      "the_one_liner_whose_message_is_punctuated_is_re_decided",
			script:    "#!/bin/sh\necho \"No build system detected; nothing to verify.\"; exit 0\n",
			wantFresh: false,
			why: "the quoted run is matched whole, so a `;` inside the prescribed message does not " +
				"make the line a gate — the only `;` that counts is the one before the exit",
		},
		{
			name:      "the_one_liner_with_a_bare_exit_is_re_decided",
			script:    "#!/bin/sh\necho 'nothing to build'; exit\n",
			wantFresh: false,
			why:       "`exit` with no argument exits on the echo's status, which is zero: the same empty gate",
		},
		{
			// A real command after the `;` is the false refusal this closed
			// form must not produce: `make build` runs here (measured), and
			// refusing it pays an LLM pass every iteration.
			name:      "a_real_gate_after_the_semicolon_keeps_the_reuse",
			script:    "#!/bin/sh\necho 'x'; make build; exit 0\n",
			wantFresh: true,
			why: "the closed form ends at the exit: anything else after the `;` is a command that " +
				"can go red, and a reader that stopped at the echo would refuse a gate that builds",
		},
		{
			// The other half of the same boundary: the exit is there, but a
			// substitution in the message runs a command (measured: make runs).
			name:      "a_substitution_in_the_one_liners_message_keeps_the_reuse",
			script:    "#!/bin/sh\necho \"$(make -s smoke)\"; exit 0\n",
			wantFresh: true,
			why: "a message built by a substitution is a command the reader cannot read — the closed " +
				"form takes no message carrying one, and the doubt keeps the reuse",
		},
		{
			name:      "a_backtick_in_the_one_liners_message_keeps_the_reuse",
			script:    "#!/bin/sh\necho `make -s smoke`; exit 0\n",
			wantFresh: true,
			why:       "a backtick runs a command exactly like $(…), so the closed form refuses to read this line",
		},
		// The allowlist's LIMIT, not a virtue: these shapes are what the
		// five node comments and docs/improvement-ratchet.md name as kept — an
		// UNMARKED empty gate written in one of them is still reused, and still
		// latches. They are executed so the written limit cannot drift from the
		// code: widen the allowlist and they redden, which is the signal to
		// rewrite the pages that promise them, not to delete the case.
		{
			name:      "an_unmarked_hatch_behind_a_cd_keeps_the_reuse",
			script:    "#!/bin/sh\ncd .\necho 'nothing to build'\nexit 0\n",
			wantFresh: true,
			why: "documented as kept: `cd` is not a head the allowlist names, and a `cd` can fail — the " +
				"allowlist does not judge which one will",
		},
		{
			name:      "an_unmarked_punctuated_message_keeps_the_reuse",
			script:    "#!/bin/sh\nset -e\necho \"No build system detected; nothing to verify.\"\nexit 0\n",
			wantFresh: true,
			why: "documented as kept: outside the one closed form the allowlist reads no quotes, so a " +
				"`;` in a message is a gate character — this is why the prescription leads with the marker",
		},
		{
			// The whole-line rule, said exactly: `exit 00` exits zero and is
			// not the `exit 0` the list names. `exit $?` and `exit +0` are the
			// same class, measured fresh=true at all five.
			name:      "an_unmarked_hatch_whose_exit_is_spelled_00_keeps_the_reuse",
			script:    "#!/bin/sh\necho 'nothing to build'\nexit 00\n",
			wantFresh: true,
			why: "documented as kept: the line must be, WHOLE, one of the listed forms, and `exit 00` " +
				"is not `exit 0` — a reader that matched the head alone would say otherwise",
		},
		{
			name:      "an_unmarked_message_spanning_two_lines_keeps_the_reuse",
			script:    "#!/bin/sh\nset -e\necho \"No build system detected.\nNothing to verify.\"\nexit 0\n",
			wantFresh: true,
			why: "documented as kept: a line of a quoted message is read as a line, and `Nothing` is not " +
				"a head the allowlist knows",
		},
		{
			name:      "an_unmarked_hatch_with_a_redirect_keeps_the_reuse",
			script:    "#!/bin/sh\necho 'nothing to build' > /dev/null\nexit 0\n",
			wantFresh: true,
			why:       "documented as kept: a redirect is a gate character, so this no-op script reads as a real gate",
		},
		{
			name:      "an_unmarked_hatch_in_a_heredoc_keeps_the_reuse",
			script:    "#!/bin/sh\ncat <<'EOF'\nnothing to build in this repo\nEOF\nexit 0\n",
			wantFresh: true,
			why:       "documented as kept: a heredoc opens on a `<`, and its body is read as lines like any other",
		},
		{
			name:      "an_unmarked_hatch_in_a_pipeline_keeps_the_reuse",
			script:    "#!/bin/sh\necho 'nothing to build' | cat\nexit 0\n",
			wantFresh: true,
			why:       "documented as kept: a pipe is a gate character, whatever stands on either side of it",
		},
		{
			name:      "an_unmarked_hatch_whose_exit_carries_a_comment_keeps_the_reuse",
			script:    "#!/bin/sh\necho 'nothing to build'\nexit 0 # nothing to verify\n",
			wantFresh: true,
			why: "documented as kept: no in-line comment is cut, so `exit 0 # …` is not the `exit 0` the " +
				"allowlist names",
		},
		{
			name:      "an_unmarked_hatch_reading_a_file_keeps_the_reuse",
			script:    "#!/bin/sh\necho 'nothing to build' < /dev/null\nexit 0\n",
			wantFresh: true,
			why: "documented as kept: an input redirect is a gate character too — the one shape whose " +
				"verdict turns on `<` alone, so dropping it from the list reddens here",
		},
		{
			name:      "an_unmarked_hatch_behind_an_ampersand_keeps_the_reuse",
			script:    "#!/bin/sh\necho 'nothing to build' &\nexit 0\n",
			wantFresh: true,
			why:       "documented as kept: an `&` is a gate character, so a backgrounded no-op reads as a real gate",
		},
		{
			name:      "an_unmarked_hatch_behind_an_early_exit_keeps_the_reuse",
			script:    "#!/bin/sh\nexit 0\nmake build\n",
			wantFresh: true,
			why: "documented as kept: this script runs NOTHING, and it is reused — the allowlist judges " +
				"lines, and `make build` is a head it does not name, dead or not",
		},
		{
			name:      "an_unmarked_message_mentioning_a_substitution_keeps_the_reuse",
			script:    "#!/bin/sh\nset -e\necho 'nothing to build, try $(nothing)'\nexit 0\n",
			wantFresh: true,
			why: "documented as kept: the allowlist does not tell a substitution from a message quoting " +
				"one, and reuse is the safe half of that doubt",
		},
	}
	for _, c := range discoverReusePrechecks(t) {
		c := c
		for _, tc := range cases {
			tc := tc
			t.Run(c.rel+"/"+c.node+"/"+tc.name, func(t *testing.T) {
				ws, scratch := t.TempDir(), t.TempDir()
				// A build manifest, unchanged between the two passes below:
				// app-dev's pre-check fingerprints the workspace's root-level
				// manifests, and a tree whose fingerprint moves answers
				// fresh=false for that reason instead of for the script's.
				if err := os.WriteFile(filepath.Join(ws, "Makefile"), []byte("build:\n\ttrue\ntest:\n\ttrue\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(scratch, "verify.sh"), []byte(tc.script), 0o755); err != nil {
					t.Fatal(err)
				}
				// Pass 2 is judged, pass 1 only puts the pre-check in its reuse
				// path: app-dev stamps the manifest fingerprint as it answers,
				// so its FIRST answer is always fresh=false (nothing stamped
				// yet) and the latch can only be observed on the second.
				first := runProbe(t, c, ws, scratch)
				got := runProbe(t, c, ws, scratch)
				if got.Fresh != tc.wantFresh {
					t.Fatalf("fresh = %v, want %v — %s\nreason: %q", got.Fresh, tc.wantFresh, tc.why, got.Reason)
				}
				if got.Reason == "" {
					t.Error("the verdict carries no reason — the run's node record is where an " +
						"operator reads why the pre-check re-decided, and it is the only reader there is")
				}
				if !tc.wantFresh {
					// BOTH answers, because the first one is where an empty
					// gate read AFTER a tree-shaped staleness check would show:
					// app-dev's fingerprint is unstamped on that pass, so a
					// pre-check that let the fingerprint speak first would name
					// the manifests here and leave the author re-deciding the
					// wrong thing.
					for _, reason := range []string{first.Reason, got.Reason} {
						if !strings.Contains(reason, "empty gate") {
							t.Errorf("a refusal must NAME what it refused — the run's node record is the "+
								"only place this decision is explained, got %q", reason)
						}
					}
				}
			})
		}
	}
}

// sharedDecisionRegions are the two stretches of the pre-check the five
// carriers ship byte for byte: the marker constant with the empty-gate reader,
// and the branch chain that consults them. What sits between them differs by
// design — app-dev has no iteration input and fingerprints the workspace's
// build manifests — so the identity is pinned per region rather than over the
// whole command. The fixtures above are behavioural and cover the shapes they
// pin; a region that drifts at one carrier keeps, in that one bundle, a hole
// the other four closed.
var sharedDecisionRegions = []struct {
	what     string
	from     string
	to       string
	minLines int
}{
	{
		what:     "the marker constant and the empty-gate reader",
		from:     "hatch_marker = ",
		to:       "\nfresh = ",
		minLines: 20,
	},
	{
		what:     "the read of the script and the branch chain that consults them",
		from:     "    src = ''",
		to:       "\n    else:",
		minLines: 10,
	},
}

// sharedRegion cuts one shared region out of a discovered pre-check's REAL
// compiled command.
func sharedRegion(t *testing.T, c reusePrecheck, i int) string {
	t.Helper()
	r := sharedDecisionRegions[i]
	start := strings.Index(c.command, r.from)
	if start < 0 {
		t.Fatalf("%s of %s carries no %q — either this pre-check decides an empty gate some other "+
			"way, or it no longer decides it at all", c.node, c.rel, r.from)
	}
	body := c.command[start:]
	end := strings.Index(body, r.to)
	if end < 0 {
		t.Fatalf("%s of %s: nothing follows %q, so %s cannot be cut — this guard would compare a "+
			"truncation", c.node, c.rel, r.from, r.what)
	}
	cut := body[:end]
	// A floor, because a cut that returns nothing compares nothing.
	if lines := strings.Count(cut, "\n") + 1; lines < r.minLines {
		t.Fatalf("%s cut out of %s is %d lines, want >= %d — the cut is stale and this comparison "+
			"is near-vacuous", r.what, c.rel, lines, r.minLines)
	}
	return cut
}

// TestTheSharedDecisionIsByteIdenticalAtEveryCarrier pins the identity the
// change relies on: one reader and one branch chain, written five times.
// Discovered, never a roster, so a sixth carrier joins the comparison by
// existing.
func TestTheSharedDecisionIsByteIdenticalAtEveryCarrier(t *testing.T) {
	carriers := discoverReusePrechecks(t)
	for i, r := range sharedDecisionRegions {
		first := sharedRegion(t, carriers[0], i)
		for _, c := range carriers[1:] {
			got := sharedRegion(t, c, i)
			if got == first {
				continue
			}
			want, mine := strings.Split(first, "\n"), strings.Split(got, "\n")
			for line := 0; line < len(want) || line < len(mine); line++ {
				la, lb := "<end of region>", "<end of region>"
				if line < len(want) {
					la = want[line]
				}
				if line < len(mine) {
					lb = mine[line]
				}
				if la == lb {
					continue
				}
				t.Errorf("%s diverges between %s and %s at line %d:\n  %s: %q\n  %s: %q\n"+
					"the carriers ship it byte for byte — a divergence leaves one bundle deciding a "+
					"reuse the others refuse, and only the pinned shapes would say so", r.what,
					carriers[0].rel, c.rel, line+1, carriers[0].rel, la, c.rel, lb)
				break
			}
		}
	}
}

// TestVerifyProbeHatchMarkerIsTaughtWhereTheScriptIsWritten holds the
// prescription to the enforcement. A marker nobody is told to write enforces
// nothing on the cooperative path, and the empty-gate backstop next to it only
// recognises the shapes it recognises. The author reads two things: the PROMPTS
// of the node that re-authors verify.sh — the node the pre-check routes to when
// it refuses reuse, discovered from the edge rather than named — and the
// verify-build skill the bundle ships. A `##` comment in the .bot teaches
// nobody, which is why this reads the compiled prompt bodies.
func TestVerifyProbeHatchMarkerIsTaughtWhereTheScriptIsWritten(t *testing.T) {
	for _, c := range discoverReusePrechecks(t) {
		c := c
		t.Run(c.rel, func(t *testing.T) {
			var author *ir.AgentNode
			var authorName string
			for _, e := range c.wf.Edges {
				if e.From != c.node || e.Condition != "fresh" || !e.Negated {
					continue
				}
				node, ok := c.wf.Nodes[e.To].(*ir.AgentNode)
				if !ok {
					t.Fatalf("%s routes a refused reuse to %s, which is %T — the re-author path must "+
						"reach the agent that writes verify.sh", c.node, e.To, c.wf.Nodes[e.To])
				}
				author, authorName = node, e.To
			}
			if author == nil {
				t.Fatalf("no `%s -> … when not fresh` edge in %s — a refused reuse re-decides nothing",
					c.node, c.rel)
			}
			var prompts strings.Builder
			for _, name := range []string{author.SystemPrompt, author.UserPrompt} {
				if p := c.wf.Prompts[name]; p != nil {
					prompts.WriteString(p.Body)
				}
			}
			if prompts.Len() == 0 {
				t.Fatalf("%s carries no system or user prompt body — nothing instructs it", authorName)
			}
			if !strings.Contains(prompts.String(), hatchMarker) {
				t.Errorf("%s of %s prescribes the empty gate without telling the author to write %q — "+
					"the marker the pre-check reads is one nobody writes", authorName, c.rel, hatchMarker)
			}
			skill := filepath.Join(filepath.Dir(c.rel), "skills", "verify-build.md")
			body, err := os.ReadFile(skill)
			if err != nil {
				t.Fatalf("read %s: %v", skill, err)
			}
			if !strings.Contains(string(body), hatchMarker) {
				t.Errorf("%s prescribes the empty gate without %q — the skill is the contract verify.sh "+
					"is written against", skill, hatchMarker)
			}
		})
	}
}

// theReadersLimit is the sentence a pre-check carrier owes the maintainer who
// reads the node next to its comment: the line allowlist fails SAFE toward
// reuse, so an UNMARKED empty gate written in a shape it refuses to judge is
// still reused and still latches. Every shape it names is executed against
// every discovered pre-check by the limit cases of
// TestVerifyProbeNeverReusesAnEmptyGate, so the sentence cannot drift from
// what the code answers.
const theReadersLimit = "the reuse is what is refused, and the allowlist reads whole lines -- no " +
	"in-line comment cut, no statement split, quotes read only inside the one closed form it knows, " +
	"`echo <message>; exit 0` -- so a message carrying another gate character, a message spanning two " +
	"lines, an exit 0 with a comment after it, and any line that is not, WHOLE, one of the listed " +
	"forms are real gates, and an UNMARKED hatch written that way is still reused -- even one that " +
	"runs nothing at all."

// theMarkersScope is the other half the carrier owes its reader: how wide the
// marker's match is, and where it stops. The keys are matched where a word
// begins, which is what keeps the English word `criterion` from naming the
// product, and a widening of either key is a decision about which real gates
// pay a verify_build pass — pinned here because the fixtures measure the
// behaviour and nothing else held the sentence that describes it.
const theMarkersScope = "recognised on any line carrying both `iterion` and `no build`, case folded " +
	"and punctuation dropped, each key matched where a word begins so the English word `criterion` " +
	"does not name the product, because an LLM writes that line from prose and prose deviates; a " +
	"different product name (`iteration:`) is not it"

// flattenedProse reads a bot source as the prose a maintainer reads — comment
// markers off, line breaks collapsed — so a pinned sentence is matched by its
// words and not by where the 80th column happened to fall.
func flattenedProse(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var words []string
	for _, line := range strings.Split(string(body), "\n") {
		words = append(words, strings.Fields(strings.TrimPrefix(strings.TrimSpace(line), "##"))...)
	}
	return strings.Join(words, " ")
}

// TestEveryReusePrecheckCarrierNamesTheReadersLimit holds the written record to
// the measured one at the DISCOVERED carriers. "The hatch cannot latch" is true
// of a marked gate and of a body the reader recognises, and false of an
// unmarked empty gate in a shape it does not read — measured fresh:true at all
// five. A carrier whose comment stops at the claim sends the maintainer
// deciding whether this hole is closed to a green that certifies nothing, which
// is how the overclaim travelled across this class the first time.
func TestEveryReusePrecheckCarrierNamesTheReadersLimit(t *testing.T) {
	owed := []struct {
		what     string
		sentence string
	}{
		{"the allowlist's limit", theReadersLimit},
		{"the marker's scope", theMarkersScope},
	}
	for _, c := range discoverReusePrechecks(t) {
		c := c
		t.Run(c.rel, func(t *testing.T) {
			// The node a carrier compiles can come from an included unit:
			// feature-dev's pre-check lives in lib/nodes.bot, and so does its
			// comment.
			sources := []string{c.rel}
			libs, err := filepath.Glob(filepath.Join(filepath.Dir(c.rel), "lib", "*.bot"))
			if err != nil {
				t.Fatal(err)
			}
			sources = append(sources, libs...)
			var prose []string
			for _, src := range sources {
				prose = append(prose, flattenedProse(t, src))
			}
			for _, owe := range owed {
				found := false
				for _, p := range prose {
					if strings.Contains(p, owe.sentence) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("no source of %s names %s — %q. Every carrier owes it; one that stops "+
						"at `cannot latch` is how the claim drifts back", c.rel, owe.what, owe.sentence)
				}
			}
		})
	}
}

// prescribesTheEmptyGate is the clause a verify skill uses to tell the author
// what to do in a repo with nothing to build. It discriminates on the ACT being
// prescribed rather than on a file name: a skill that prescribes the hatch and
// omits the marker teaches the author to write a gate a pre-check will reuse.
const prescribesTheEmptyGate = "echoes that fact and exits 0"

// TestEveryVerifySkillPrescribingTheEmptyGateTeachesTheMarker holds the marker
// convention over the whole class of files that prescribe the hatch, not only
// the verify-build.md the five pre-check carriers ship: e2e-coverage and
// test-coverage ship a verify-tests.md that prescribes it too, and a bundle
// that teaches the marker in one file and not the other leaves the author
// writing whichever it read last.
func TestEveryVerifySkillPrescribingTheEmptyGateTeachesTheMarker(t *testing.T) {
	skills, err := filepath.Glob("*/skills/verify-*.md")
	if err != nil {
		t.Fatal(err)
	}
	prescribing := 0
	for _, skill := range skills {
		body, err := os.ReadFile(skill)
		if err != nil {
			t.Fatalf("read %s: %v", skill, err)
		}
		if !strings.Contains(string(body), prescribesTheEmptyGate) {
			continue
		}
		prescribing++
		if !strings.Contains(string(body), hatchMarker) {
			t.Errorf("%s prescribes the empty gate without %q — the marker the pre-check reads is "+
				"one this file never asks for", skill, hatchMarker)
		}
	}
	// A floor, because the other failure mode of a phrase discriminator is to
	// match nothing at all and leave the guard green over an empty set.
	if prescribing < 13 {
		t.Fatalf("%d verify skills prescribe the empty gate, want >= 13 — the clause %q is stale",
			prescribing, prescribesTheEmptyGate)
	}
}

// refutedReuseClaims are claims about the reuse pre-check that measurement
// refutes. It refuses a script carrying hatchMarker, and one whose every
// non-comment line is, whole, a set of shell options, a plain echo or printf, a
// bare truth or a zero exit — nothing else. The limit cases of
// TestVerifyProbeNeverReusesAnEmptyGate run #1707's own one-liner, a punctuated
// message, a two-line message, a redirect, a heredoc, a pipeline, a `cd`, a
// comment trailing the exit and a message quoting a substitution through every
// discovered pre-check and measure
// fresh:true, so a page promising that NO empty gate is ever reused sends its
// reader — and the unattended-merge decision that rests on these gates — to a
// green that certifies nothing. Widening the allowlist widens this list's
// premise, not the list.
// Matched against a case-folded copy of the page: the vocabulary these claims
// are made of is written in CAPS at the carriers ("A RECOGNISED EMPTY GATE is
// never REUSED"), so a scanner over the raw bytes let the claim through in the
// voice the pages actually use. The qualified forms ("a RECOGNISED empty gate",
// "the hatch the prescription asks for") are true and must stay sayable, so the
// claims below carry their article: `an empty gate is never reused` is the
// absolute one, `a recognised empty gate is never reused` is not.
var refutedReuseClaims = []string{
	"never an empty gate",
	"never reuses an empty gate",
	"no empty gate is reused",
	"an empty gate is never reused",
	"the empty gate is never reused",
	"empty gate can no longer latch",
	"any empty gate is",
	"every empty gate",
	"cannot become the verdict",
	"never becomes the run",
}

// TestNoPageClaimsEveryEmptyGateIsRefused scans what a reader actually reads —
// the bundle READMEs, every verify skill, the bot sources and the ratchet page
// — for a claim the pre-check does not honour. The claim travelled by
// copy-paste across this class once already, so the guard is over the class and
// not over the file that carried it.
func TestNoPageClaimsEveryEmptyGateIsRefused(t *testing.T) {
	pages := append(pagesAReaderAndAModelRead(t), filepath.Join("..", "docs", "improvement-ratchet.md"))
	for _, page := range pages {
		body, err := os.ReadFile(page)
		if err != nil {
			t.Fatalf("read %s: %v", page, err)
		}
		low := strings.ToLower(string(body))
		for _, claim := range refutedReuseClaims {
			if strings.Contains(low, claim) {
				t.Errorf("%s claims %q — the pre-check refuses the marked hatch and a body of "+
					"whole-line no-ops, and keeps the reuse for every line it cannot judge; say "+
					"what it refuses, or a reader trusts a green that certifies nothing",
					page, claim)
			}
		}
	}
}

// TestTheWritingPassStillGreensTheEmptyGate measures the half of the hatch a
// refused reuse does NOT remove, because the five node comments, the verify
// skills and docs/improvement-ratchet.md all state it: the reuse is refused,
// and the pass that WRITES the gate still reports passed=true, skipped=false.
// It is the sentence that decides what a reader may conclude from a green —
// the unattended-merge decision rests on these gates — so it is measured
// end to end, through both deterministic nodes of the gate, and not asserted.
func TestTheWritingPassStillGreensTheEmptyGate(t *testing.T) {
	for _, bin := range []string{"python3", "sh", "git"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH", bin)
		}
	}
	const message = "no build or test system in this repository"
	script := "#!/bin/sh\n" + hatchMarker + "\necho '" + message + "'\nexit 0\n"
	for _, c := range discoverReusePrechecks(t) {
		c := c
		t.Run(c.rel, func(t *testing.T) {
			// The node that RUNS the script, discovered from the edge the
			// pre-check takes when it reuses — never named, for the same reason
			// the carriers are not.
			var runner *ir.ToolNode
			var runnerName string
			for _, e := range c.wf.Edges {
				if e.From != c.node || e.Condition != "fresh" || e.Negated {
					continue
				}
				node, ok := c.wf.Nodes[e.To].(*ir.ToolNode)
				if !ok {
					t.Fatalf("%s routes a reuse to %s, which is %T — the gate that executes the "+
						"script must be a deterministic tool node", c.node, e.To, c.wf.Nodes[e.To])
				}
				runner, runnerName = node, e.To
			}
			if runner == nil {
				t.Fatalf("no `%s -> … when fresh` edge in %s — nothing executes the script this "+
					"pre-check decides about", c.node, c.rel)
			}
			ws, scratch := t.TempDir(), t.TempDir()
			// A build system the hatch never invokes, committed the way a
			// campaign pass leaves its work before the gate runs. CMake ships
			// none of the root manifests app-dev's pre-check fingerprints, so
			// this tree reaches the reuse path at every carrier — the tree the
			// ticket names, and the one a manifest list cannot catch.
			if err := os.WriteFile(filepath.Join(ws, "CMakeLists.txt"),
				[]byte("project(latch C)\nadd_executable(latch main.c)\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(ws, "main.c"), []byte("int main(void) { return\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gittest.Run(t, ws, "init", "-q")
			commitFixture(t, ws)
			if err := os.WriteFile(filepath.Join(scratch, "verify.sh"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			// The pre-check refuses the reuse. Twice, because app-dev's first
			// answer is always fresh=false on an unstamped fingerprint, so only
			// the second says the marker decided it.
			runProbe(t, c, ws, scratch)
			if got := runProbe(t, c, ws, scratch); got.Fresh {
				t.Fatalf("%s reuses the marked hatch (reason %q) — the latch this change exists to "+
					"break", c.node, got.Reason)
			}
			var res struct {
				Passed  bool   `json:"passed"`
				Skipped bool   `json:"skipped"`
				LogTail string `json:"log_tail"`
			}
			out := runNodeCommand(t, c, runner.Command, runnerName, ws, scratch)
			if uerr := json.Unmarshal(out, &res); uerr != nil {
				t.Fatalf("%s output is not the verify_result JSON: %v (out %q)", runnerName, uerr, out)
			}
			if !res.Passed || res.Skipped {
				t.Fatalf("%s of %s answered passed=%v skipped=%v for the hatch — the pages say the "+
					"writing pass still greens, and a reader who cannot see this green is a reader "+
					"the refusal would have to protect alone", runnerName, c.rel, res.Passed, res.Skipped)
			}
			// What that green is made of: the hatch's own echo, and nothing
			// that read the tree. `skipped: false` is the part no downstream
			// reader can tell from a real pass.
			if !strings.Contains(res.LogTail, message) {
				t.Errorf("%s greened without running the hatch — its log carries %q, not the "+
					"script's own message", runnerName, res.LogTail)
			}
			if strings.Contains(strings.ToLower(res.LogTail), "cmake") {
				t.Errorf("%s's log mentions the build system (%q): this fixture exists because "+
					"nothing in the gate builds the committed application", runnerName, res.LogTail)
			}
		})
	}
}

// TestAMarkerRefusalQuotesTheLineThatMatched holds the refusal's reason to the
// FACT it measured. The match is tolerant — case, punctuation and word order
// are folded away — so the line that declared the empty gate is usually not the
// spelling the prescription shows. A reason naming the prescription instead
// sends the operator who reads the node record grepping verify.sh for a string
// that is not in it, and this reason is the only reader this decision has (no
// edge and no prompt consumes outputs.verify_probe).
func TestAMarkerRefusalQuotesTheLineThatMatched(t *testing.T) {
	for _, bin := range []string{"python3", "sh"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH", bin)
		}
	}
	const declared = "# ITERION: NO_BUILD_SYSTEM"
	script := "#!/bin/sh\n" + declared + "\necho \"No build system detected; nothing to verify.\"\nexit 0\n"
	prescribed := strings.TrimPrefix(hatchMarker, "# ")
	for _, c := range discoverReusePrechecks(t) {
		c := c
		t.Run(c.rel, func(t *testing.T) {
			ws, scratch := t.TempDir(), t.TempDir()
			if err := os.WriteFile(filepath.Join(ws, "Makefile"), []byte("build:\n\ttrue\ntest:\n\ttrue\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(scratch, "verify.sh"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			// Pass 2 is judged: app-dev's first answer is always fresh=false on
			// an unstamped fingerprint.
			runProbe(t, c, ws, scratch)
			got := runProbe(t, c, ws, scratch)
			if got.Fresh {
				t.Fatalf("%s reuses a marked hatch (reason %q)", c.node, got.Reason)
			}
			if !strings.Contains(got.Reason, declared) {
				t.Errorf("%s refused on %q and the reason does not quote it, so the record names no "+
					"greppable line: %q", c.node, declared, got.Reason)
			}
			if strings.Contains(got.Reason, prescribed) {
				t.Errorf("%s names the prescribed spelling %q, which this verify.sh does NOT carry — "+
					"an operator who greps for it finds nothing: %q", c.node, prescribed, got.Reason)
			}
		})
	}
}

// TestTheMarkerKeysCannotDriftFromTheMarker ties the two keys the match uses to
// the marker the prescription teaches, at every DISCOVERED carrier: a key that
// is not part of the marker would refuse scripts no page tells an author about,
// and a marker reworded without its keys would stop recognising the very line
// 18 prescription sites ask for.
func TestTheMarkerKeysCannotDriftFromTheMarker(t *testing.T) {
	keys := regexp.MustCompile(`marker_keys = \(([^)]*)\)`)
	marker := regexp.MustCompile(`hatch_marker = '([^']*)'`)
	for _, c := range discoverReusePrechecks(t) {
		c := c
		t.Run(c.rel, func(t *testing.T) {
			mm := marker.FindStringSubmatch(c.command)
			if mm == nil {
				t.Fatalf("%s of %s declares no hatch_marker", c.node, c.rel)
			}
			km := keys.FindStringSubmatch(c.command)
			if km == nil {
				t.Fatalf("%s of %s declares no marker_keys", c.node, c.rel)
			}
			flat := flattenAlnum(mm[1])
			var found int
			for _, raw := range strings.Split(km[1], ",") {
				key := strings.Trim(strings.TrimSpace(raw), "'")
				if key == "" {
					continue
				}
				found++
				if !strings.Contains(flat, key) {
					t.Errorf("marker_keys of %s carries %q, which is not part of the marker %q "+
						"(folded %q) — the match would refuse a line no page prescribes",
						c.rel, key, mm[1], flat)
				}
			}
			if found < 2 {
				t.Errorf("%s matches on %d key(s) — one key alone is either the product name or a "+
					"phrase about builds, and both appear in real gates", c.rel, found)
			}
		})
	}
}

// flattenAlnum folds a string the way the pre-check folds a line: lower case,
// every non-alphanumeric character dropped. The class is python's str.isalnum():
// letters, digits and numerics, ASCII or not.
func flattenAlnum(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsNumber(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}
