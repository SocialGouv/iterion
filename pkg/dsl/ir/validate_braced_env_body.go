package ir

import "strings"

// ---------------------------------------------------------------------------
// C188 — the engine's braced env expansion defeats the body's own
// assignment (#1522, ADR-130)
// ---------------------------------------------------------------------------
//
// expandBracedEnv (pkg/backend/model/executor_tool.go) rewrites a tool body
// BEFORE any shell sees it: every `${NAME:-default}` resolves
// unconditionally, and a plain `${NAME}` whenever lookupToolEnv answers the
// name — an `ITERION_*` name from the bot-var overlay first, any name from
// the engine's own environment when its process-env policy exposes it
// (ProcessEnvReadable: a cloud server and runner install
// platformcfg.CloudProcessEnvPolicy, which refuses a credential-shaped name
// or a platform secret unless ITERION_CLOUD_ENV_PASSTHROUGH lists it, so
// there a plain `${TOKEN}` reaches the shell verbatim while `${TOKEN:-none}`
// still resolves — to the default). A shell body that assigns a name and
// then reads it back braced therefore answers the engine's environment, and
// the value the body just set never reaches the read — sec-audit-source's
// run-id guard refused every production deep-scan pass that way until #1473
// repaired that bot.
//
// What C188 aims at: precision over recall. It is meant to fire only where
// the body's assignment, once it runs, persists to the braced read in bash,
// and only on a form the engine answers before any shell runs — a
// `${NAME:-…}` always, a plain `${NAME}` whenever lookupToolEnv answers the
// name, a latent defeat the message says is one. Every shape the scanner
// knows to be otherwise stays silent, whatever that costs in recall: a lint
// that misses is acceptable, a warning on a correct line is not
// (docs/references/diagnostics.md). The scanner does not see every such
// shape: its known limits — bodies whose assignment never reaches the read
// in a way it cannot follow, where the finding is wrong — are listed in
// docs/workflow_authoring_pitfalls.md beside the documented misses.
//
// The read set is an INTERSECTION OF THREE, each half authoritative for one
// question:
//
//   - WHERE the shell looks — the `${` offsets scanShellBody records at
//     unquoted and double-quoted positions.
//   - WHOM the engine consults — BracedEnvReads, the engine's own
//     segmentation replayed, which reports the offset it consults each name
//     FROM (on a nested default the outer name only: its segment ends at the
//     first `}`, so the inner form's offset is never a segment of its own).
//     The two halves meet on an OFFSET, so a form one half reads at a
//     position the other does not is no finding.
//   - WHEN the body sets the name — the terminator of the earliest command
//     that assigns it (shellAssign). scanShellBody leaves out the spellings
//     it recognises of an assignment bash confines elsewhere: a prefix
//     (`NAME=v cmd`, `NAME=v \cmd`, `NAME=v env …`), a command modifier's
//     operand (`env NAME=v cmd`, `command NAME=v`), a subshell, a `$( … )`
//     or backquoted substitution, a backgrounded command (`NAME=v &`), a
//     `read` that is a pipeline element, a function's `local`, `declare` or
//     `typeset`, an `export -f` operand, and a `for NAME do` without a word
//     list. The spellings it does not follow are its known limits, where it
//     still fires. A read before that offset reads the ambient value by
//     design: a command reads its own text before it assigns — bash expands
//     a command's words and redirections before its prefix assignments take
//     effect, and a declaration utility's operands, a `for`'s word list and
//     a `read`'s redirection before they assign — so the right-hand side
//     (`PATH="${PATH}:/opt/bin"`, `RUN_ID="${RUN_ID:-none}"`,
//     `X=$(tr : , <<< "${X}")`), a `for`'s word list
//     (`for ARCH in ${ARCH:-amd64}`), a `read`'s redirection target
//     (`read -r CFG < "${CFG}"`) and a second operand of the same `export`
//     all answer the ambient value — and so does a read that simply precedes
//     the assignment. Every one of them would be a lie to call dead, and the
//     bare-$NAME remedy destroys the self-default shape outright. The offset
//     costs three documented misses: a read a LATER loop iteration would
//     answer from the body (a `${X}` above `X=1` inside a `for`), and two
//     reads inside an assignment-only command, which assigns left to right
//     (`A=1 B="${A}"` reads 1) and, under bash, before its own redirections
//     (`X=new >"f.${X}"` creates f.new; dash expands the redirection first)
//     — the engine defeats each, and C188 stays silent.
//
// A name fires only where all three agree, and a `js`/`py` script body is
// not a shell at all — the bare-$NAME remedy is not valid code there.
//
// A node that declares `env_expansion: none` draws nothing at all
// (ADR-130): the engine answers none of its bodies' braced forms, so
// the assignment reaches the read and the premise of the whole check is
// gone. That is also the remedy the message names beside the bare
// rewrite, for a body whose braces the downstream language needs.
//
// The shell-live half is why a read inside a comment, a single-quoted span or
// a heredoc body draws nothing — not because such a position is safe: the
// engine is quote-blind and substitutes all three (proven against
// expandBracedEnv in pkg/backend/model's pin table), and an UNQUOTED heredoc
// delimiter leaves its body live for the shell too, so the defeat there is
// real. What the scanner cannot tell apart at those positions is a read from
// text meant verbatim, and a warning that fired on both would train authors
// to ignore it; the misses are the documented kind, reported by the dry run
// rather than by this arm.
//
// A warning, like C153/C157/C158: yesterday's bot keeps compiling, and the
// diagnostic names the fix. One finding per (body, name), on the read it
// reports — positioned on the body's property line; the AST keeps no
// position inside a body, so the line inside it travels in the message.
func (c *compiler) validateBracedEnvBodies(w *Workflow) {
	for _, n := range w.Nodes {
		t, ok := n.(*ToolNode)
		if !ok {
			continue
		}
		if !t.ExpandsBracedEnv() {
			// The node declines the expansion (ADR-130), so no braced read
			// of its bodies is answered before their reader runs: the
			// assignment DOES reach the read, and there is nothing to name.
			// The declaration is the node's, so this covers all three
			// bodies. A check that kept firing after its own remedy would
			// teach authors to ignore it. The predicate is the one the
			// chokepoint asks (model.ToolBodyEnv), so the lint and the run
			// cannot disagree about which bodies the engine rewrites.
			continue
		}
		bodies := []struct {
			where string
			text  string
			shell bool // the body runs through a shell (a script: only when its language is sh/bash/empty)
		}{
			{"command", t.Command, true},
			{"script", t.Script, t.Language == "" || t.Language == "sh" || t.Language == "bash"},
			{"postcondition", t.Postcondition, true},
		}
		for _, b := range bodies {
			if b.text == "" || !b.shell {
				continue
			}
			sc := scanShellBody(b.text)
			if len(sc.assigns) == 0 {
				continue
			}
			set := make(map[string]int, len(sc.assigns)) // name → the earliest assignment's end
			for _, a := range sc.assigns {
				if end, dup := set[a.name]; !dup || a.end < end {
					set[a.name] = a.end
				}
			}
			shellLive := make(map[int]bool, len(sc.reads))
			for _, off := range sc.reads {
				shellLive[off] = true
			}
			type defeat struct {
				at         int
				hasDefault bool
				glued      bool
			}
			defeated := map[string]defeat{}
			var order []string
			for _, r := range BracedEnvReads(b.text) {
				end, assigned := set[r.Name]
				if !shellLive[r.At] || !assigned || r.At < end {
					continue
				}
				// The arm REPORTED is the strongest one the body carries
				// for this name, not the first it happens to write: a body
				// reading `${RUN_ID}` on one line and `${RUN_ID:-}` on the
				// next is defeated unconditionally, and saying so depends
				// on the engine's rule, never on the author's ordering.
				//
				// The glue travels the same way, for the same reason: the
				// finding covers every defeating read of the name, so its
				// remedy has to be sound on all of them. One read of
				// `${COUNT}` glued to `_items` makes the bare rewrite alone
				// destructive for the name, whichever read the finding sits
				// on — the clause below says WHERE it applies rather than
				// asserting it of the read reported.
				g := bracedEnvGluedToAName(b.text, r.At)
				switch cur, seen := defeated[r.Name]; {
				case !seen:
					defeated[r.Name] = defeat{r.At, r.HasDefault, g}
					order = append(order, r.Name)
				case r.HasDefault && !cur.hasDefault:
					defeated[r.Name] = defeat{r.At, true, cur.glued || g}
				case g && !cur.glued:
					cur.glued = true
					defeated[r.Name] = cur
				}
			}
			for _, name := range order {
				d := defeated[name]
				// The mechanism clause carries the engine's rule for THIS
				// arm, and no more: a `:-` form is resolved whatever the
				// environment holds, a plain one only when the engine's own
				// process carries the name and its process-env policy exposes
				// it (bracedEnvWouldExpand -> lookupToolEnv ->
				// ProcessEnvReadable), so the plain arm's defeat is latent and
				// stating it outright would be false on every engine whose
				// environment lacks that name, and on a cloud server or
				// runner for a credential-shaped one its
				// ITERION_CLOUD_ENV_PASSTHROUGH does not list. The shell
				// is unnamed because the three bodies do not share one: a
				// `command:` runs under `bash -c`, a `script:` under its
				// language's interpreter (`sh` for sh/empty).
				form := "${" + name + "}"
				mechanism := "the engine resolves ${" + name + "} from its own process environment, before any shell runs the body, whenever that process carries the name and its env policy exposes it — the read then answers the engine's environment and this body's assignment of " + name + " is dead"
				if d.hasDefault {
					form = "${" + name + ":-…}"
					mechanism = "the engine resolves any ${" + name + ":-…} form unconditionally, before any shell runs the body, so this body's assignment of " + name + " is dead: the read answers the engine's environment, not the value the body just set"
				}
				// The remedy is the rewrite the name's reads need, every
				// one of them: this finding is the only warning the author
				// gets for the name, and the bare rewrite applied to the
				// rest of them has to leave the body working. Bare is the
				// whole fix only where the braces carry nothing else:
				// dropping them from a `:-` form drops the fallback with
				// them, and dropping them where an identifier byte follows
				// the `}` feeds that byte to the name.
				remedy := "Read the variable bare ($" + name + ")"
				if d.glued {
					remedy += ", closing the word yourself (\"$" + name + "\"…) wherever an identifier byte follows the brace, because a bare $" + name + " would read those bytes as part of the name"
				}
				if d.hasDefault {
					remedy += ", and spell the fallback as a statement the shell runs ([ -z \"$" + name + "\" ] && " + name + "=\"…\") because a bare $" + name + " drops the default"
				}
				if !d.glued && !d.hasDefault {
					remedy += " so the shell answers"
				}
				// The other half of the remedy: an author who needs the
				// braces — a `js` template literal, a `:-` default the
				// downstream language reads — takes the engine out of the
				// way instead of rewording the body, and this check goes
				// silent on the node (ADR-130). Its price travels with it:
				// the ADR-093 overlay answers at expansion time only, so a
				// declining body's `${ITERION_*}` dial read is answered by
				// nobody — and the declaration is the node's three bodies',
				// which is what makes the trade worth naming here rather
				// than in a page the author may not open.
				remedy += "; or declare `env_expansion: none` on the node, which keeps its bodies' environment forms verbatim at the price of the ADR-093 overlay for those bodies — an `${ITERION_*}` dial read there is no longer answered by the engine"
				c.warnfAtProperty(DiagBracedEnvDefeatsAssignment, "tool", t.ID, b.where,
					"tool %q %s, body line %d: %s — %s. %s",
					t.ID, b.where, strings.Count(b.text[:d.at], "\n")+1, form, mechanism, remedy)
			}
		}
	}
}
