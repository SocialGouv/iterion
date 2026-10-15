package bots

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// planPhaseBots are the campaign bots carrying the cross-model plan
// phase (author → peer critique → author revise → campaign). The list is
// the contract: adding the phase to another bot means adding it here so
// the wiring below is guarded for it too.
var planPhaseBots = []string{
	"feature-dev",
	"app-dev",
	"branch-improve-loop",
	"whole-improve-loop",
	"feature-gap-fill",
	"test-coverage",
	"e2e-coverage",
}

// TestPlanPhaseWiring pins, per bot, the parts of the plan phase whose
// silent loss would not fail compilation:
//   - the plan_review / plan_review_policy vars (the launch resolver's
//     opt-in gate — without the declaration, reviewtopology injects
//     nothing and the phase is dead config);
//   - the phase nodes;
//   - the peer reviewer's `action: skip` fallback route gated on
//     plan_review_policy (the "continue and ignore" half of the policy).
func TestPlanPhaseWiring(t *testing.T) {
	for _, bot := range planPhaseBots {
		t.Run(bot, func(t *testing.T) {
			path := filepath.Join(bot, "main.bot")
			pr := parseBotUnit(path)
			cr := ir.Compile(pr.File)
			if cr.HasErrors() {
				t.Fatalf("%s does not compile: %+v", path, cr.Diagnostics)
			}
			wf := cr.Workflow

			// The peer policy defaults to "skip" fleet-wide: the
			// cross-model plan reviewer is an OPTIONAL enrichment, and
			// the primary family alone must always suffice — a dead
			// second-family credential parked a fixer holding a PR
			// hostage, and a stale pod key paused every cloud campaign
			// through plan_review auto + wait. "wait" stays the per-run
			// deliberate-spend opt-in.
			for varName, wantDefault := range map[string]string{
				"plan_review":        "auto",
				"plan_review_policy": "skip",
			} {
				v, ok := wf.Vars[varName]
				if !ok {
					t.Errorf("%s: var %s not declared — the launch resolver injects nothing without it", bot, varName)
					continue
				}
				if got, _ := v.Default.(string); got != wantDefault {
					t.Errorf("%s: var %s default = %q, want %q", bot, varName, got, wantDefault)
				}
			}

			for _, nodeID := range []string{"plan_topology", "plan", "plan_review", "plan_gate", "plan_revise"} {
				if _, ok := wf.Nodes[nodeID]; !ok {
					t.Errorf("%s: plan-phase node %q missing", bot, nodeID)
				}
			}

			peer, ok := wf.Nodes["plan_review"].(*ir.JudgeNode)
			if !ok {
				t.Fatalf("%s: plan_review is not a judge node", bot)
			}
			var skip *ir.Fallback
			for i := range peer.Fallbacks {
				if peer.Fallbacks[i].Action == ir.FallbackActionSkip {
					skip = &peer.Fallbacks[i]
				}
			}
			if skip == nil {
				t.Fatalf("%s: plan_review has no action:skip fallback route — the skip policy is dead", bot)
			}
			if !strings.Contains(skip.When, "plan_review_policy") {
				t.Errorf("%s: skip route's when: %q is not gated on plan_review_policy", bot, skip.When)
			}
			// on: [any] is load-bearing: under the shipped sandbox:auto a
			// claw failure flattens to a string at the IPC boundary and
			// classifies UNCLASSIFIED, which a FILTERED skip refuses by
			// design — the operator's `skip` policy would silently become
			// `wait` (Revi finding R1b58ea).
			if len(skip.On) != 1 || skip.On[0] != "any" {
				t.Errorf("%s: skip route's on: = %v, want [any] (a filtered skip is refused unclassified failures — sandboxed claw errors classify unclassified)", bot, skip.On)
			}
			// And the policy var must be enum'd so a typo'd --var fails at
			// launch instead of silently selecting wait (Rde458c).
			if pol := wf.Vars["plan_review_policy"]; pol == nil || len(pol.EnumValues) != 2 {
				t.Errorf("%s: plan_review_policy lacks its [enum: wait, skip]", bot)
			}
		})
	}
}

// TestPlanPhaseCampaignEdgeMappings pins two authoring invariants the
// engine still relies on after #484. Exclusive FORWARD siblings are
// filtered to the selected edge; a loop-head RE-ENTRY still overlays
// the selected back-edge on every forward edge whose source has output
// (the back-edge is partial — it restates only the keys that change).
// So:
//
//  1. every FORWARD edge into `campaign` maps every campaign_input field
//     — an unmapped field is not "" but the raw {{input.x}} placeholder
//     leaking into the prompt;
//  2. no two LOOP back-edges into `campaign` map the same key — a shared
//     key lets the later-declared loop hijack it on every subsequent
//     pass (app-dev's draft_feedback/fail_log split exists because of
//     exactly this).
func TestPlanPhaseCampaignEdgeMappings(t *testing.T) {
	for _, bot := range planPhaseBots {
		t.Run(bot, func(t *testing.T) {
			path := filepath.Join(bot, "main.bot")
			cr := ir.Compile(parseBotUnit(path).File)
			if cr.HasErrors() {
				t.Fatalf("%s does not compile: %+v", path, cr.Diagnostics)
			}
			wf := cr.Workflow

			schema, ok := wf.Schemas["campaign_input"]
			if !ok {
				t.Fatal("campaign_input schema missing")
			}
			var fields []string
			for _, f := range schema.Fields {
				fields = append(fields, f.Name)
			}

			loopKeyOwners := map[string]string{} // key -> "from(loop)"
			for _, e := range wf.Edges {
				if e.To != "campaign" {
					continue
				}
				mapped := map[string]bool{}
				for _, m := range e.With {
					mapped[m.Key] = true
				}
				if e.IsBoundedIteration() {
					for k := range mapped {
						owner := e.From + "(" + e.LoopName + e.ForeachName + ")"
						if prev, dup := loopKeyOwners[k]; dup {
							t.Errorf("%s: loop back-edges %s and %s both map %q — the later-declared one hijacks it on every pass",
								bot, prev, owner, k)
						}
						loopKeyOwners[k] = owner
					}
					continue
				}
				for _, f := range fields {
					if !mapped[f] {
						t.Errorf("%s: forward edge %s -> campaign does not map %q — the prompt renders a raw {{input.%s}} placeholder on that path",
							bot, e.From, f, f)
					}
				}
			}
			if len(loopKeyOwners) == 0 {
				t.Errorf("%s: no loop back-edge into campaign found — the continuation loop is gone?", bot)
			}

			// Declaration-order invariant: an UNTRAVERSED forward edge still
			// contributes its mappings (every edge whose source has output
			// applies, later declaration winning), so every forward edge into
			// campaign that maps a plan field to a LITERAL BLANK (the
			// phase-off / no-prior-content fallback: plan_topology's and
			// plan_gate's) must be declared BEFORE every forward edge that
			// maps it to REAL content, or the blank's untraversed
			// contribution clobbers the real value on pass 1.
			//
			// Expressed on the VALUE (blank vs. non-blank), not on a
			// hardcoded source node id: most bots hand the revised plan to
			// campaign directly from plan_revise, but a bot may route it
			// through an extra deterministic hop first (branch-improve-loop's
			// plan_budget_gate, native:695) — the invariant is the same
			// either way, and pinning the literal source name here would
			// make this shared test bot-specific.
			planFields := []string{"plan", "plan_critique", "plan_responses", "plan_provenance"}
			type fieldMapping struct {
				idx   int
				from  string
				blank bool
			}
			byField := map[string][]fieldMapping{}
			for i, e := range wf.Edges {
				if e.To != "campaign" || e.IsBoundedIteration() {
					continue
				}
				for _, m := range e.With {
					if !slices.Contains(planFields, m.Key) {
						continue
					}
					blank := len(m.Refs) == 0 && m.Raw == ""
					byField[m.Key] = append(byField[m.Key], fieldMapping{idx: i, from: e.From, blank: blank})
				}
			}
			for _, f := range planFields {
				mappings := byField[f]
				firstReal := -1
				for _, fm := range mappings {
					if !fm.blank && (firstReal == -1 || fm.idx < firstReal) {
						firstReal = fm.idx
					}
				}
				if firstReal == -1 {
					t.Errorf("%s: no forward edge into campaign maps %q to real content — the plan phase's output never reaches campaign", bot, f)
					continue
				}
				for _, fm := range mappings {
					if fm.blank && fm.idx > firstReal {
						t.Errorf("%s: %s -> campaign (idx %d) maps %q BLANK but is declared AFTER the real mapping (idx %d) — its untraversed contribution would clobber the real value", bot, fm.from, fm.idx, f, firstReal)
					}
				}
			}
		})
	}
}

// TestFeatureDevKimiQuotaFallbacks pins the repair worker's escape hatch from
// a closed Claude subscription window. This is intentionally feature-dev-only:
// other campaign bots retain their own provider topology.
func TestFeatureDevKimiQuotaFallbacks(t *testing.T) {
	path := filepath.Join("feature-dev", "main.bot")
	cr := ir.Compile(parseBotUnit(path).File)
	if cr.HasErrors() {
		t.Fatalf("%s does not compile: %+v", path, cr.Diagnostics)
	}
	wf := cr.Workflow

	for _, nodeID := range []string{"plan", "plan_revise", "review", "finalize_mr"} {
		node, ok := wf.Nodes[nodeID]
		if !ok {
			t.Fatalf("feature-dev: node %q missing", nodeID)
		}
		llm, ok := node.(ir.LLMNode)
		if !ok {
			t.Fatalf("feature-dev: node %q is %T, want LLM node", nodeID, node)
		}
		if got := llm.GetLLMFields().Backend; got != "claude_code" {
			t.Errorf("feature-dev: node %q primary backend = %q, want claude_code", nodeID, got)
		}
		var kimi *ir.Fallback
		for i := range llm.GetFallbacks() {
			if llm.GetFallbacks()[i].Name == "kimi_quota" {
				kimi = &llm.GetFallbacks()[i]
			}
		}
		if kimi == nil {
			t.Errorf("feature-dev: node %q lacks the kimi_quota rescue route", nodeID)
			continue
		}
		if kimi.Backend != "kimi" || kimi.Model != "kimi-code/kimi-for-coding" {
			t.Errorf("feature-dev: node %q kimi_quota = backend %q model %q, want Kimi coding route", nodeID, kimi.Backend, kimi.Model)
		}
		if len(kimi.On) != 2 || kimi.On[0] != "usage_window" || kimi.On[1] != "unavailable" {
			t.Errorf("feature-dev: node %q kimi_quota triggers = %v, want [usage_window unavailable]", nodeID, kimi.On)
		}
	}

	// verify_build declares interaction: human, which is inert on kimi (no
	// ask_user wiring on CLI-agent backends, #1910; the diagnostics wave names it C271): a kimi_quota
	// route would run the gate without ever pausing, so the node has none
	// and usage-window recovery waits for Claude instead.
	verify, ok := wf.Nodes["verify_build"].(ir.LLMNode)
	if !ok {
		t.Fatalf("feature-dev: node %q is %T, want LLM node", "verify_build", wf.Nodes["verify_build"])
	}
	for i := range verify.GetFallbacks() {
		if verify.GetFallbacks()[i].Backend == "kimi" {
			t.Error("feature-dev: verify_build cannot fall back to Kimi, where its interaction: human is inert (#1910)")
		}
	}

	campaign, ok := wf.Nodes["campaign"].(*ir.AgentNode)
	if !ok {
		t.Fatalf("feature-dev: campaign is %T, want *ir.AgentNode", wf.Nodes["campaign"])
	}
	if campaign.Interaction != ir.InteractionAsync || campaign.Backend != "claude_code" {
		t.Errorf("feature-dev: campaign must retain Claude's async interaction, got %q on %q", campaign.Interaction, campaign.Backend)
	}
	for _, fallback := range campaign.GetFallbacks() {
		if fallback.Backend == "kimi" {
			t.Error("feature-dev: campaign cannot fall back to Kimi, which lacks async question tools")
		}
	}

	revise, ok := wf.Nodes["plan_revise"].(*ir.AgentNode)
	if !ok {
		t.Fatalf("feature-dev: plan_revise is %T, want *ir.AgentNode", wf.Nodes["plan_revise"])
	}
	if revise.Session != ir.SessionFresh {
		t.Errorf("feature-dev: plan_revise session = %q, want fresh so the Kimi route never inherits a Claude session", revise.Session)
	}

	peer, ok := wf.Nodes["plan_review"].(*ir.JudgeNode)
	if !ok {
		t.Fatalf("feature-dev: plan_review is %T, want *ir.JudgeNode", wf.Nodes["plan_review"])
	}
	if peer.GetLLMFields().Backend != "kimi" || peer.GetLLMFields().Model != "kimi-code/kimi-for-coding" {
		t.Errorf("feature-dev: plan_review = backend %q model %q, want the permitted Kimi reviewer", peer.GetLLMFields().Backend, peer.GetLLMFields().Model)
	}
	// And the Kimi credential is the ONLY one it can spend: no route re-pins
	// the model or the backend, so this peer runs or it does not run at all.
	// The page and pkg/reviewtopology both say so — `plan_review: auto`
	// counts the claude/gpt pair, which is not this credential, so a
	// claude + kimi host needs `plan_review: on` to get the review.
	for _, route := range peer.GetFallbacks() {
		if route.Model != "" || route.Backend != "" {
			t.Errorf("feature-dev: plan_review's %q route re-pins model %q / backend %q — the page and TestFamiliesFromReportCountsOnlyParticipatingCredentials both rest on this peer having one credential and no alternative", route.Name, route.Model, route.Backend)
		}
	}
}

// TestPlanRevisePromptCarriesThePlanAndNamesItAsData pins both halves of the
// one channel that reaches `plan_revise`'s model on every route: its own
// prompts.
//
// CARRIES. The node answers a peer critique of a plan the same role authored,
// and six of the seven run `session: inherit_if_available` — a best-effort
// session the engine degrades to fresh when its backing state is gone
// (pkg/backend/model/executor_retry.go, which names this very node) — while
// feature-dev's is deliberately `fresh`. So the prompt is the only channel
// that carries the plan context on every route, and the ratchet
// (llm_input_reaches_its_prompt_test.go) cannot hold that alone: it reads
// "carried ⇒ rendered", so dropping a field from the input schema, from every
// `with` mapping and from the prompt together leaves it green while the author
// stops seeing its own plan. This is the positive half — the names the prompt
// owes, whatever the session did.
//
// NAMES IT AS DATA. Every value it shows is a model's text or this
// repository's, and `readonly: true` withholds the file-writing tools while
// keeping the shell, so each field the prompt shows is named in the node's
// UNTRUSTED INPUT BOUNDARY paragraph: the NAME in backticks, never a
// reference (TestUntrustedInputBoundaryParagraphInterpolatesNothing refuses
// one). A field rendered under a label the paragraph never claimed as data is
// the hole this closes, and it is read off what the prompt RENDERS, so the
// next field added to the prompt owes its name here without anyone
// remembering to come back.
//
// AND SHOWS IT IN THE OTHER HALF. Naming bounds the paragraph, not the
// channel the value arrives on: the paragraph itself calls the system prompt
// authoritative, so a field rendered there arrives as mission however
// carefully the paragraph claims it as data — and neither the naming half nor
// the interpolation guard looks past the paragraph's own lines. Every input
// field is therefore refused in the system prompt, named or not: the remedy is
// one line away (the user prompt, where all six already render), so a field
// nobody classified falls on the refusing side.
//
// Scoped to this family on purpose: here every value the prompt shows is a
// model's text, so "shows it ⇒ names it" is the whole rule. Catalogue-wide the
// same predicate would also demand a name for the engine's and the operator's
// own values (`workspace_dir`, `base_sha`, a budget), which the paragraph
// should not label as untrusted data — that rule needs per-field provenance
// first. The sibling `plan_review` is out for that reason and not for lack of
// a subject: it shows the same three author values, app-dev's paragraph claims
// `plan` in prose only, and branch-improve-loop's also shows `large` — a flag
// a tool node computes from `git diff --stat`, not a model's text, so the
// premise this rule rests on is already gone one node over.
func TestPlanRevisePromptCarriesThePlanAndNamesItAsData(t *testing.T) {
	// The plan context the author can no longer remember, plus the critique it
	// adjudicates: its own `plan`, `assumptions` and `risks`, and the peer's
	// `concerns`, `suggestions` and `blocking` verdict.
	owed := []string{"plan", "assumptions", "risks", "concerns", "suggestions", "blocking"}
	for _, bot := range planPhaseBots {
		t.Run(bot, func(t *testing.T) {
			path := filepath.Join(bot, "main.bot")
			cr := ir.Compile(parseBotUnit(path).File)
			if cr.HasErrors() {
				t.Fatalf("%s does not compile: %+v", path, cr.Diagnostics)
			}
			wf := cr.Workflow
			node, isLLM := wf.Nodes["plan_revise"].(ir.LLMNode)
			if !isLLM {
				t.Fatalf("%s: plan_revise is %T, want an LLM node", bot, wf.Nodes["plan_revise"])
			}
			// The premise both halves rest on: the session is never a
			// promised channel. `inherit_if_available` is best-effort by
			// its own name — the engine marks it droppable and the retry
			// re-sends the prompt without it — and `fresh` carries
			// nothing. Under a strict `session: inherit` the prompt would
			// stop being the only channel on every route, and the node
			// comments and manifest lines that call this session
			// best-effort would be false.
			agent, isAgent := wf.Nodes["plan_revise"].(*ir.AgentNode)
			if !isAgent {
				t.Fatalf("%s: plan_revise is %T, want *ir.AgentNode", bot, wf.Nodes["plan_revise"])
			}
			if agent.Session != ir.SessionFresh && agent.Session != ir.SessionInheritIfAvailable {
				t.Errorf("%s: plan_revise's session = %q — its prompts are written as the only channel the model has on every route, which holds for `fresh` and for the best-effort `inherit_if_available`, not for a session the engine promises to carry", bot, agent.Session)
			}
			// THE reading of "a channel of this node renders it", shared with
			// the ratchet and the prose guard: the real resolver over this
			// node's own channels, never a grep of the prompt body.
			reading, carries := missingInputFields(t, wf, "plan_revise")
			if !carries {
				t.Fatalf("%s: plan_revise's input carries nothing the bot put there — its input schema and every mapping onto it are gone", bot)
			}
			missing := toSet(reading.missing)
			fields := inputFields(t, wf, "plan_revise", reading.schema)
			var shown []string
			for _, f := range fields {
				// `_`-prefixed keys are the engine's own (`_session_id`): the
				// runtime reads them, no model does.
				if strings.HasPrefix(f.name, "_") || missing[f.name] {
					continue
				}
				shown = append(shown, f.name)
			}
			for _, field := range owed {
				if !slices.Contains(shown, field) {
					t.Errorf("%s: no channel of plan_revise delivers %q — on a route where the session did not survive the author answers a critique of a plan it cannot see. Render {{input.%s}} in the node's own prompt; dropping the field does not make the value visible, because the session is not a channel (docs/agents/bots/input-reaches-the-model.md)",
						bot, field, field)
				}
			}

			sys := wf.Prompts[node.GetLLMFields().SystemPrompt]
			if sys == nil {
				t.Fatalf("%s: plan_revise declares no system prompt, so it carries no %s paragraph", bot, untrustedInputBoundaryMarker)
			}
			usr := wf.Prompts[node.GetLLMFields().UserPrompt]
			if usr == nil {
				t.Fatalf("%s: plan_revise declares no user prompt — the half every field this node shows renders in", bot)
			}
			paragraphs := untrustedInputBoundaryParagraphs(sys.Body)
			if len(paragraphs) == 0 {
				t.Fatalf("%s: plan_revise's system prompt carries no %s paragraph — TestCatalogUntrustedInputBoundaryOnActingPrompts owns that half and should have reddened first", bot, untrustedInputBoundaryMarker)
			}
			var b strings.Builder
			for _, para := range paragraphs {
				b.WriteString(strings.Join(para, "\n"))
				b.WriteString("\n")
			}
			boundary := b.String()
			for _, field := range shown {
				if !strings.Contains(boundary, "`"+field+"`") {
					t.Errorf("%s: plan_revise's prompt shows %q but its %s paragraph does not name it — a value arriving under a label the boundary never claimed as data. Write `%s` into the paragraph: the NAME in backticks, since a reference there would render the untrusted value into the authoritative half of the prompt",
						bot, field, untrustedInputBoundaryMarker, field)
				}
			}
			// WHERE it renders, not only whether it is named — read with the
			// resolver the ratchet uses, over the system prompt alone, so
			// what counts is what the engine would actually deliver there
			// (a padded `{{ input.x }}` included, the literal escape not),
			// never a grep of the body.
			inSystem := renderedInputNames(fields, []string{sys.Body}, sys.TemplateRefs)
			for _, f := range fields {
				if strings.HasPrefix(f.name, "_") || !inSystem[f.name] {
					continue
				}
				t.Errorf("%s: plan_revise's system prompt renders input %q — the half its own %s paragraph calls authoritative, where another model's text arrives as mission however the paragraph names it. Render it in the user prompt instead",
					bot, f.name, untrustedInputBoundaryMarker)
			}
			// And the same half refuses every value the RUN produced, not
			// only the ones arriving on this node's input: the peer's
			// critique is also `{{outputs.plan_review.concerns}}` — the very
			// mapping the plan_gate edge writes — and the reading above looks
			// for input markers alone, so it is blind to that name. The
			// allowlist decides, never a list of namespaces to refuse:
			// ir.RefKind grows by appending, so a namespace added after this
			// guard was written is refused until someone classifies it.
			for _, ref := range sys.TemplateRefs {
				if authoritativeHalfNamespaces[ref.Kind] {
					continue
				}
				t.Errorf("%s: plan_revise's system prompt interpolates %s — a value this run produced, delivered into the half its own %s paragraph calls authoritative. Render it in the user prompt, or widen authoritativeHalfNamespaces if the namespace is the operator's or the engine's",
					bot, ref.Raw, untrustedInputBoundaryMarker)
			}
			// And it claims no memory of its own. `inherit_if_available`
			// degrades to fresh and feature-dev's node is `fresh`, so "earlier
			// in this session" — the phrasing six of these seven prompts
			// carried — promises a channel no route owes. Held here rather
			// than in refutedSessionClaims because the phrase is true
			// elsewhere: copilot's prompt says "Memory from earlier in this
			// session" about a brief the host projects and hedges it ("may be
			// empty … if the session was restarted").
			for _, half := range []struct{ name, body string }{{"system", sys.Body}, {"user", usr.Body}} {
				if strings.Contains(strings.ToLower(half.body), "in this session") {
					t.Errorf("%s: plan_revise's %s prompt claims something \"in this session\" — this node runs %s, which carries no conversation the author could remember. Say what rides the prompt instead: the plan and the critique are mapped onto its input and rendered by its own prompts",
						bot, half.name, agent.Session)
				}
			}
		})
	}
}

// authoritativeHalfNamespaces are the reference namespaces a system prompt
// carrying an UNTRUSTED INPUT BOUNDARY paragraph may interpolate: the
// operator's launch values, the engine's own run facts, a materialised secret
// and the literal `{{` escape. Everything else is a value the RUN produced —
// another node's output, an artifact, a loop or foreach binding, an
// attachment, this node's input — which the paragraph calls data while
// calling this half authoritative.
//
// An allowlist on purpose: ir.RefKind is appended to (pkg/dsl/ir/ir.go), so a
// namespace that did not exist when this guard was written falls on the
// refusing side instead of arriving unexamined in the mission half.
var authoritativeHalfNamespaces = map[ir.RefKind]bool{
	ir.RefVars:        true,
	ir.RefRun:         true,
	ir.RefSecrets:     true,
	ir.RefLiteralOpen: true,
}

// TestAuthoritativeHalfNamespacesClassifiesEveryNamespaceTheDSLCanWrite
// executes the allowlist in both directions, through the parser the engine
// uses, so widening it is a red test and not a silent policy change — and the
// roster is ir's own, so a namespace appended to RefKind reddens here until
// someone says whether the mission half may carry it.
func TestAuthoritativeHalfNamespacesClassifiesEveryNamespaceTheDSLCanWrite(t *testing.T) {
	cases := []struct {
		expr    string
		sayable bool
	}{
		{"{{vars.workspace_dir}}", true},
		{"{{run.tree_noise}}", true},
		{"{{secrets.deploy_credential}}", true},
		{`{{"{{"}}`, true},
		{"{{input.concerns}}", false},
		{"{{ input.concerns }}", false},
		{"{{outputs.plan_review.concerns}}", false},
		{"{{artifacts.plan}}", false},
		{"{{attachments.spec.path}}", false},
		{"{{loop.campaign_loop.previous_output}}", false},
		{"{{each.lot.item}}", false},
	}
	classified := map[ir.RefKind]bool{}
	for _, c := range cases {
		refs, err := ir.ParseRefs(c.expr)
		if err != nil || len(refs) != 1 {
			t.Fatalf("ir.ParseRefs(%q) = %d ref(s), err %v — want exactly one, so this case still says what it means", c.expr, len(refs), err)
		}
		classified[refs[0].Kind] = true
		if got := authoritativeHalfNamespaces[refs[0].Kind]; got != c.sayable {
			t.Errorf("%s is namespace %q: sayable in the authoritative half = %v, want %v", c.expr, refs[0].Kind, got, c.sayable)
		}
	}
	for k := ir.RefKind(0); k.String() != "unknown"; k++ {
		if !classified[k] {
			t.Errorf("reference namespace %q is not classified above — say whether a plan_revise system prompt may interpolate it, and add it to authoritativeHalfNamespaces only if it is the operator's or the engine's value", k)
		}
	}
}

// refutedSessionClaims are the sentences this family carried about a session
// the engine does not promise, in the voice its own pages wrote them. The
// revise turn runs on the best-effort `inherit_if_available` — or, on
// feature-dev, on no session at all — so a page promising the SAME author
// session, or a prompt promising it carries everything, states a guarantee no
// route owes: the plan and the critique ride the prompt, and that is the whole
// claim.
//
// Needles, not a predicate: every phrase below was in the tree at 6 to 18
// sites and got there by copy-paste, which is the shape a needle catches and a
// predicate cannot — the idea has endless paraphrases, so the premise itself
// is executed by TestPlanRevisePromptCarriesThePlanAndNamesItAsData instead.
// Matched case-folded against the page with its comment markers stripped and
// its whitespace collapsed, because these claims wrap across two `##` lines
// ("the SAME\n## author session challenges …") and are written in CAPS where
// they are made. The qualified, true forms stay sayable: "the same session"
// about an HTTP session (golden-master), about a CLI session (docs/backends.md)
// or about copilot's question resume is left alone, and so are the dated
// records of the decision as it was taken (a manifest changelog line, an ADR),
// which the globs below do not read.
var refutedSessionClaims = []string{
	"same author session",
	"author again, same session",
	"author (same session)",
	"as the complete context",
	"the plan and the critique in full",
	"loses no plan",
	"loses no context",
}

// TestNoPlanPhasePageClaimsASessionTheEngineDoesNotPromise scans what a reader
// and a model actually read — every bundle README, every verify skill, every
// bot source and shared node library — for a claim this family's session
// cannot honour. Over the class rather than over the four files that carried
// it: the claim crossed four bundles by copy-paste once already, and the next
// bundle to grow a plan phase will be written by copying one of them.
func TestNoPlanPhasePageClaimsASessionTheEngineDoesNotPromise(t *testing.T) {
	for _, page := range pagesAReaderAndAModelRead(t) {
		raw, err := os.ReadFile(page)
		if err != nil {
			t.Fatalf("read %s: %v", page, err)
		}
		low, collapsed := strings.ToLower(string(raw)), collapsedPage(string(raw))
		for _, claim := range refutedSessionClaims {
			if !strings.Contains(low, claim) && !strings.Contains(collapsed, claim) {
				continue
			}
			t.Errorf("%s claims %q — plan_revise runs the best-effort `inherit_if_available`, "+
				"and feature-dev's runs no session at all, so no route owes the author its "+
				"earlier conversation nor a complete context. Say what rides the prompt: the "+
				"plan and the critique are mapped onto this node's input and rendered by its "+
				"own prompts", page, claim)
		}
	}
}

// collapsedPage reads a page the way its reader does: case-folded, comment and
// list markers off, every line joined into one run of words. A claim an author
// wrapped across two lines is one phrase again.
func collapsedPage(raw string) string {
	var words []string
	for _, line := range strings.Split(strings.ToLower(raw), "\n") {
		words = append(words, strings.Fields(strings.TrimLeft(strings.TrimSpace(line), "#/*- "))...)
	}
	return strings.Join(words, " ")
}
