package runtime

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// Parallel-branch admission reads a node's `fallbacks:` routes, because a route
// that leaves claw un-restricts whatever the node declared. DISPATCH resolves
// those routes with the run's vars (model.ClawExecutor.resolveChain →
// resolveRoutingField) and drops the ones whose `when:` gate those vars leave
// false — so admission asks the executor for that chain, through the
// EffectiveFallbackResolver seam, rather than re-deriving it from the IR.
//
// Re-derived, the two readings part on two cells and the guard is wrong on
// both: a route spelled `{{vars.fb}}` is read as a backend nothing resolved
// and refused, for a run whose vars put `claw` there — which bounds the node
// exactly as its own route does — and a route this run can never take is
// counted. The rows below are those cells, and the ones that must NOT move.
// The executor is the real *model.ClawExecutor with the run's vars, never a
// stub that answers what the test wants.

// twoFallbackRouteBranches is the witness topology: two branches sharing one
// worktree, each a claw agent that declares one reader and carries one
// `fallbacks:` route.
func twoFallbackRouteBranches(nodeBackend, routeBackend, when string) *ir.Workflow {
	return twoBranches(func(id string) ir.Node {
		n := agentNode(id, []string{"read_file"}, nodeBackend, nil)
		n.Fallbacks = []ir.Fallback{{
			Name:    "alt",
			Backend: routeBackend,
			Model:   "anthropic/claude-haiku-4-5",
			When:    when,
		}}
		return n
	})
}

// fallbackRouteEngine builds the engine admission runs on: the production
// executor, carrying the run's vars exactly as the engine hands them to it at
// run start (runInitState → SetVars).
func fallbackRouteEngine(t *testing.T, wf *ir.Workflow, vars map[string]any) *Engine {
	t.Helper()
	pinTheHostsAutoMemoryMode(t)
	exec := model.NewClawExecutor(model.NewRegistry(), wf)
	exec.SetVars(vars)
	return &Engine{workflow: wf, executor: exec}
}

func TestParallelAdmissionReadsAFallbackRouteTheWayDispatchResolvesIt(t *testing.T) {
	cases := []struct {
		name        string
		nodeBackend string
		route       string
		when        string
		vars        map[string]any
		admitted    bool
		reason      string // the refusal must carry this
		notReason   string // and must not name this
		cause       string // the per-branch cause, worded over the route the run takes
		why         string
	}{
		{
			name:        "a template route the run resolves to claw",
			nodeBackend: "claw",
			route:       "{{vars.fb}}",
			vars:        map[string]any{"fb": "claw"},
			admitted:    true,
			why:         "the route runs on claw, where the node's `tools:` list is its bound — the same bound its own route has",
		},
		{
			name:        "a template route the run resolves to claude_code",
			nodeBackend: "claw",
			route:       "{{vars.fb}}",
			vars:        map[string]any{"fb": "claude_code"},
			admitted:    false,
			reason:      "claude_code",
			notReason:   "{{vars.fb}}",
			cause:       `node "a" falls back to claude_code`,
			why:         "the chain leaves claw for a route the declared list does not bound, and the reason names the route the run takes",
		},
		{
			name:        "a template route the run resolves to codex, which is off claw",
			nodeBackend: "claw",
			route:       "{{vars.fb}}",
			vars:        map[string]any{"fb": "codex"},
			admitted:    false,
			reason:      "a `fallbacks:` route onto codex leaves claw",
			notReason:   "{{vars.fb}}",
			why:         "the claw→CLI rule stands on the resolved route too: a pre-run reading does not credit codex's own sandbox mode to a node whose primary is claw",
		},
		{
			name:        "a template route nothing resolves",
			nodeBackend: "claw",
			route:       "{{vars.fb}}",
			vars:        nil,
			admitted:    false,
			reason:      "{{vars.fb}}",
			why:         "the run puts no value there, so the route stays unresolved and the pessimistic reading stands",
		},
		{
			name:        "a `when:` gate this run's vars leave false",
			nodeBackend: "claw",
			route:       "claude_code",
			when:        "vars.rescue == 'on'",
			vars:        map[string]any{"rescue": "off"},
			admitted:    true,
			why:         "dispatch filters that route out of the chain for this run, so nothing the node may run leaves claw",
		},
		{
			name:        "a `when:` gate this run's vars leave true",
			nodeBackend: "claw",
			route:       "claude_code",
			when:        "vars.rescue == 'on'",
			vars:        map[string]any{"rescue": "on"},
			admitted:    false,
			reason:      "claude_code",
			why:         "the route is in the chain, and it runs the full native toolset",
		},
		{
			name:        "a `when:` gate this run's vars leave broken",
			nodeBackend: "claw",
			route:       "claude_code",
			when:        "vars.rescue ===== (",
			vars:        map[string]any{"rescue": "on"},
			admitted:    true,
			why:         "the executor deactivates a route whose gate it cannot evaluate, in the chain dispatch walks as much as in this reading, so the route admission drops is one no run takes — and the compiler refuses a gate that does not parse",
		},
		{
			name:        "a template route the run resolves to the node's own backend name",
			nodeBackend: "codex",
			route:       "{{vars.fb}}",
			vars:        map[string]any{"fb": "codex"},
			admitted:    true,
			why:         "codex bounds this node by the names it declares, on its own route and on the resolved one",
		},
		{
			name:        "a template route the run resolves to a route codex's list cannot bound",
			nodeBackend: "codex",
			route:       "{{vars.fb}}",
			vars:        map[string]any{"fb": "claude_code"},
			admitted:    false,
			reason:      "claude_code",
			notReason:   "{{vars.fb}}",
			cause:       `node "a" falls back to claude_code`,
			why:         "the resolved route takes the list as --disallowedTools over a roster iterion does not own",
		},
		{
			name:        "a route over a namespace no run resolves before the node runs",
			nodeBackend: "claw",
			route:       "{{outputs.pick.backend}}",
			vars:        map[string]any{"fb": "claw"},
			admitted:    false,
			reason:      "still spelled as {{outputs.pick.backend}}",
			why:         "the executor resolves `vars.` and keeps every other span as written (C148), so the route is still a backend nothing decided",
		},
		{
			name:        "a template route the run resolves to the `auto` NAME",
			nodeBackend: "claw",
			route:       "{{vars.fb}}",
			vars:        map[string]any{"fb": "auto"},
			admitted:    true,
			why:         "`auto` on a route is a name the registry does not have, so the route dies the moment the chain is needed and widens nothing",
		},
		{
			name:        "a literal claw route stays admitted",
			nodeBackend: "claw",
			route:       "claw",
			vars:        nil,
			admitted:    true,
			why:         "a route that stays on claw changes nothing about what the node can do",
		},
		{
			name:        "a literal claude_code route stays refused",
			nodeBackend: "claw",
			route:       "claude_code",
			vars:        nil,
			admitted:    false,
			reason:      "claude_code",
			why:         "the authored CLI route is the cell the guard was written for",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wf := twoFallbackRouteBranches(tc.nodeBackend, tc.route, tc.when)
			e := fallbackRouteEngine(t, wf, tc.vars)
			err := e.validateWorkspaceSafety("fan", wf.Edges)
			if tc.admitted {
				if err != nil {
					t.Fatalf("refused two branches a run admits: %s\n  %v", tc.why, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("admitted two branches onto one shared worktree: %s", tc.why)
			}
			rerr, ok := err.(*RuntimeError)
			if !ok || rerr.Code != ErrCodeWorkspaceSafety {
				t.Fatalf("err = %v, want the typed workspace-safety refusal", err)
			}
			if tc.reason != "" && !strings.Contains(rerr.Message, tc.reason) {
				t.Errorf("the refusal does not name %q, so the author cannot act on it: %q", tc.reason, rerr.Message)
			}
			if tc.cause != "" && !strings.Contains(rerr.Message, tc.cause) {
				t.Errorf("the per-branch cause is not worded over the route the run takes (%s): %q", tc.cause, rerr.Message)
			}
			if tc.notReason != "" && strings.Contains(rerr.Message, tc.notReason) {
				t.Errorf("the refusal names %q, a spelling the verdict no longer reads: %q", tc.notReason, rerr.Message)
			}
		})
	}
}

// forwardsTheTwoOlderSeams wraps the production executor and answers the
// backend and tool-surface seams but not the fallback one: the shape an
// embedder's wrapper has the day a third seam lands. Admission must keep
// today's reading there — a route it cannot resolve is read pessimistically —
// never the permissive one.
type forwardsTheTwoOlderSeams struct {
	NodeExecutor
	inner *model.ClawExecutor
}

func (f forwardsTheTwoOlderSeams) EffectiveToolNames(node ir.Node, mayEscalateToUltracode bool) []string {
	return f.inner.EffectiveToolNames(node, mayEscalateToUltracode)
}

func (f forwardsTheTwoOlderSeams) EffectiveBackendName(node ir.Node) string {
	return f.inner.EffectiveBackendName(node)
}

// An executor that cannot resolve the routes keeps the pessimistic reading: the
// zero value of this seam is the refusal, never the passthrough.
func TestAnExecutorThatCannotResolveTheRoutesKeepsThePessimisticReading(t *testing.T) {
	wf := twoFallbackRouteBranches("claw", "{{vars.fb}}", "")
	pinTheHostsAutoMemoryMode(t)
	production := model.NewClawExecutor(model.NewRegistry(), wf)
	production.SetVars(map[string]any{"fb": "claw"})

	var buf bytes.Buffer
	e := &Engine{
		workflow: wf,
		executor: forwardsTheTwoOlderSeams{production, production},
		logger:   iterlog.New(iterlog.LevelWarn, &buf),
	}
	err := e.validateWorkspaceSafety("fan", wf.Edges)
	if err == nil {
		t.Fatal("a wrapper that cannot resolve a `fallbacks:` route admitted two branches on the strength of a route nothing read — the seam's absence must refuse, not pass through")
	}
	if !strings.Contains(err.Error(), "{{vars.fb}}") {
		t.Errorf("the refusal does not name the unresolved route: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "forwardsTheTwoOlderSeams") || !strings.Contains(out, "EffectiveFallbackResolver") {
		t.Errorf("the run does not name the executor that cannot resolve the routes, nor the seam it lacks: %q", out)
	}

	// The executor that answers says nothing: a line an operator sees on every
	// healthy run is a line they learn to ignore.
	var quiet bytes.Buffer
	ok := &Engine{workflow: wf, executor: production, logger: iterlog.New(iterlog.LevelWarn, &quiet)}
	if err := ok.validateWorkspaceSafety("fan", wf.Edges); err != nil {
		t.Fatalf("the production executor refused a route this run resolves to claw: %v", err)
	}
	if strings.Contains(quiet.String(), "EffectiveFallbackResolver") {
		t.Errorf("the production executor warned about a seam it answers: %q", quiet.String())
	}

	// The other half of what that silence costs: a route the run's `when:`
	// gate deactivates is counted anyway, where the executor that evaluates
	// the gate admits the pair.
	gated := twoFallbackRouteBranches("claw", "claude_code", "vars.rescue == 'on'")
	gatedExec := model.NewClawExecutor(model.NewRegistry(), gated)
	gatedExec.SetVars(map[string]any{"rescue": "off"})
	if err := (&Engine{workflow: gated, executor: forwardsTheTwoOlderSeams{gatedExec, gatedExec}}).validateWorkspaceSafety("fan", gated.Edges); err == nil {
		t.Error("a wrapper that cannot read the chain admitted a pair on a route it never evaluated the gate of — the seam's absence must keep counting that route")
	}
	if err := (&Engine{workflow: gated, executor: gatedExec}).validateWorkspaceSafety("fan", gated.Edges); err != nil {
		t.Errorf("the production executor counted a route its own chain drops for this run: %v", err)
	}
}

// Both leaf executors answer the seam, and that is a COMPILE-time guarantee
// (the pins beside the interface and in pkg/dryrun). This test names the class
// the pins protect, so removing a pin AND its method is a red test rather than
// a build that still passes and a guard that quietly re-reads the IR.
func TestTheProductionExecutorAnswersTheFallbackRouteSeam(t *testing.T) {
	var claw any = (*model.ClawExecutor)(nil)
	if _, ok := claw.(EffectiveFallbackResolver); !ok {
		t.Error("*model.ClawExecutor no longer resolves `fallbacks:` routes for admission — a `{{vars.…}}` route would be refused again although the run resolves it onto a backend the declaration bounds")
	}
	// dryrun's executor is pinned in its own package: pkg/dryrun imports
	// pkg/runtime, so this package cannot name it.
}

// The warning names the executor that cannot resolve the routes, and needs a
// logger — which `runtime.New` does not install. So the refusal says it too,
// exactly as it does for the two older seams (unansweredCause): a run whose
// fan-outs are refused on a route its own chain resolves must not have to be
// read backwards, and the author's `{{vars.fb}}` is not the thing to change.
func TestTheRefusalNamesTheExecutorThatCouldNotResolveTheRoutes(t *testing.T) {
	wf := twoFallbackRouteBranches("claw", "{{vars.fb}}", "")
	pinTheHostsAutoMemoryMode(t)
	production := model.NewClawExecutor(model.NewRegistry(), wf)
	production.SetVars(map[string]any{"fb": "claw"})
	if err := (&Engine{workflow: wf, executor: production}).validateWorkspaceSafety("fan", wf.Edges); err != nil {
		t.Fatalf("precondition: the executor that resolves the route admits the pair: %v", err)
	}

	err := (&Engine{workflow: wf, executor: forwardsTheTwoOlderSeams{production, production}}).validateWorkspaceSafety("fan", wf.Edges)
	if err == nil {
		t.Fatal("precondition: an executor that cannot resolve the routes keeps the pessimistic reading and refuses")
	}
	for _, want := range []string{
		"runtime.forwardsTheTwoOlderSeams",
		"runtime.EffectiveFallbackResolver",
		"EffectiveFallbackBackends",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q, so nothing in it points at the executor that left the route unread: %v", want, err)
		}
	}

	// And it says it only where that is true: the executor that answers the
	// seam is never blamed for a route its own reading leaves as written.
	unresolvable := twoFallbackRouteBranches("claw", "{{outputs.pick.backend}}", "")
	answering := model.NewClawExecutor(model.NewRegistry(), unresolvable)
	rerr := (&Engine{workflow: unresolvable, executor: answering}).validateWorkspaceSafety("fan", unresolvable.Edges)
	if rerr == nil {
		t.Fatal("precondition: a route over a namespace no run resolves before the node runs is refused")
	}
	if strings.Contains(rerr.Error(), "EffectiveFallbackResolver") {
		t.Errorf("the refusal blames the seam on an executor that answers it: %v", rerr)
	}

	// And never for a reason the fallback seam decides NOTHING about. A node's
	// own `backend:` is the backend seam's field — the arm that words it
	// returns before any route is read — so a clause blaming the fallback seam
	// there names the cause the verdict did not use, and the proof is that its
	// remedy, followed to the letter, leaves the refusal word for word the
	// same.
	ownField := twoBranches(func(id string) ir.Node {
		return agentNode(id, []string{"read_file"}, "{{vars.b}}", nil)
	})
	for _, id := range []string{"a", "b"} {
		if fbs := ownField.Nodes[id].(ir.LLMNode).GetFallbacks(); len(fbs) != 0 {
			t.Fatalf("precondition: node %q declares %d `fallbacks:` routes, so this cell is not the one it is about", id, len(fbs))
		}
	}
	ownExec := model.NewClawExecutor(model.NewRegistry(), ownField)
	twoSeams := (&Engine{workflow: ownField, executor: forwardsTheTwoOlderSeams{ownExec, ownExec}}).validateWorkspaceSafety("fan", ownField.Edges)
	if twoSeams == nil {
		t.Fatal("precondition: a pair whose own `backend:` nothing resolved before the run is refused the shared worktree")
	}
	if strings.Contains(twoSeams.Error(), "EffectiveFallbackResolver") {
		t.Errorf("the refusal blames the fallback seam for the node's own `backend:`, a field that seam does not read: %v", twoSeams)
	}
	followed := (&Engine{workflow: ownField, executor: forwardsEverySeamTheRemedyNames{forwardsTheTwoOlderSeams{ownExec, ownExec}}}).validateWorkspaceSafety("fan", ownField.Edges)
	if followed == nil || followed.Error() != twoSeams.Error() {
		t.Errorf("following the clause's remedy moved this refusal, so a clause naming it here would not be inert after all:\n  before: %v\n  after:  %v", twoSeams, followed)
	}

	// The same reading, one arm further: the TOOL-surface arm, which runs
	// before every route arm. Its subject is a tool on a surface the EXECUTOR
	// answers over the routes IT resolved (EffectiveToolNames →
	// EffectiveToolNamesOnRoutes), so the engine's fallback seam cannot move
	// it — measured the same way, with the remedy followed to the letter — and
	// this node declares no route for that seam to have left unread.
	surfaceArm := twoBranches(func(id string) ir.Node {
		return agentNode(id, []string{"read_file"}, "claw", func(n *ir.AgentNode) { n.AutoMemory = "on" })
	})
	for _, id := range []string{"a", "b"} {
		if fbs := surfaceArm.Nodes[id].(ir.LLMNode).GetFallbacks(); len(fbs) != 0 {
			t.Fatalf("precondition: node %q declares %d `fallbacks:` routes, so this cell is not the one it is about", id, len(fbs))
		}
	}
	surfaceExec := model.NewClawExecutor(model.NewRegistry(), surfaceArm)
	surfaceSeams := forwardsTheTwoOlderSeams{surfaceExec, surfaceExec}
	if held := surfaceExec.EffectiveToolNames(surfaceArm.Nodes["a"], false); !slices.Contains(held, "write_file") {
		t.Fatalf("precondition: the surface this cell rests on no longer holds a writer the node's list does not declare: %v", held)
	}
	onSurface := (&Engine{workflow: surfaceArm, executor: surfaceSeams}).validateWorkspaceSafety("fan", surfaceArm.Edges)
	if onSurface == nil {
		t.Fatal("precondition: a pair holding a writer its `tools:` list does not declare is refused the shared worktree")
	}
	if !strings.Contains(onSurface.Error(), `holds "write_file"`) {
		t.Fatalf("precondition: this refusal is not the tool-surface arm's: %v", onSurface)
	}
	if strings.Contains(onSurface.Error(), "EffectiveFallbackResolver") {
		t.Errorf("the refusal blames the fallback seam for a tool the EXECUTOR's own surface answer puts there, a reading that seam does not take: %v", onSurface)
	}
	surfaceFollowed := (&Engine{workflow: surfaceArm, executor: forwardsEverySeamTheRemedyNames{surfaceSeams}}).validateWorkspaceSafety("fan", surfaceArm.Edges)
	if surfaceFollowed == nil || surfaceFollowed.Error() != onSurface.Error() {
		t.Errorf("following the clause's remedy moved this refusal, so a clause naming it here would not be inert after all:\n  before: %v\n  after:  %v", onSurface, surfaceFollowed)
	}

	// And it names an EXECUTOR, never one of this package's reading stand-ins:
	// each answers one seam and no node work, so none of them is a thing an
	// operator can implement — naming its Go type would be the cause the
	// verdict did not use.
	for _, standIn := range []effectiveBackendResolver{
		nil,
		unansweredBackend{executorType: "runtime.decoratorWithoutTheSeam"},
		mostPermissiveRoute{},
		answersRoute("claw"),
	} {
		if clause := routeReadingClause(nil, standIn); clause != "" {
			t.Errorf("the reading stand-in %T is named as an executor that cannot resolve the routes: %q", standIn, clause)
		}
	}
}

// The unread-route clause names the executor that left a node's `fallbacks:`
// routes unread and the method to implement, so it is honest beside one kind of
// reason only: one whose subject IS a route. Elsewhere it names a cause the
// verdict did not use.
//
// Held as an IMPLICATION over a corpus rather than as a table of sites: the
// engine picks the arm, the sweep reads the sentence it printed, and a wording
// added later that appends the clause beside a subject the fallback seam
// decides nothing about reddens here without this test having enumerated it.
// Three rounds of this review loop each found another such site; what a per-site
// table cannot do is fail for the site nobody listed.
// causesBeforeTheirClauses splits a refusal into the causes it joins with
// "; " and keeps, for each, the text its clause was appended to. Every cause
// can carry the clause, so every cause's subject is swept — a sweep cut at the
// first clause reads only the first branch's subject, and a heterogeneous
// refusal hides the second.
func causesBeforeTheirClauses(err error) []string {
	var subjects []string
	for _, cause := range strings.Split(err.Error(), "; ") {
		subject, _, _ := strings.Cut(cause, " (executor ")
		subjects = append(subjects, subject)
	}
	return subjects
}

func TestTheUnreadRouteClauseNamesOnlyAReasonAboutARoute(t *testing.T) {
	pinTheHostsAutoMemoryMode(t)
	// The subjects of the reasons that name a route — the honest callers of the
	// clause. One of these spellings must be in any reason that carries it.
	routeSubjects := []string{"falls back to", "`fallbacks:` route", "route onto", "route still spelled as"}
	namesARoute := func(subject string) bool {
		return slices.ContainsFunc(routeSubjects, func(s string) bool { return strings.Contains(subject, s) })
	}

	writerOnTheSurface := func(route string) *ir.Workflow {
		return twoBranches(func(id string) ir.Node {
			n := agentNode(id, []string{"read_file"}, "claw", func(n *ir.AgentNode) { n.AutoMemory = "on" })
			if route != "" {
				n.Fallbacks = []ir.Fallback{{Name: "alt", Backend: route, Model: "anthropic/claude-haiku-4-5"}}
			}
			return n
		})
	}
	withClause := 0
	for _, tc := range []struct {
		name string
		wf   *ir.Workflow
	}{
		{"a writer on the surface and no `fallbacks:` entry at all", writerOnTheSurface("")},
		{"a writer on the surface and a literal route", writerOnTheSurface("claude_code")},
		{"a writer on the surface and a templated route", writerOnTheSurface("{{vars.fb}}")},
		{"a route onto a backend no `tools:` list bounds", twoFallbackRouteBranches("claw", "claude_code", "")},
		{"a route still spelled as a template", twoFallbackRouteBranches("claw", "{{vars.fb}}", "")},
		{"a route under a `when:` gate", twoFallbackRouteBranches("claw", "claude_code", "vars.rescue == 'on'")},
		{"the node's own `backend:` templated", twoBranches(func(id string) ir.Node {
			return agentNode(id, []string{"read_file"}, "{{vars.b}}", nil)
		})},
		{"a node on a backend its `tools:` list does not bound", twoBranches(func(id string) ir.Node {
			return agentNode(id, []string{"read_file"}, "claude_code", nil)
		})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exec := model.NewClawExecutor(model.NewRegistry(), tc.wf)
			// The run's vars, as the engine hands them to the executor: `b` is
			// deliberately absent, which is what leaves the own-`backend:` row
			// a field nothing resolved before the run.
			exec.SetVars(map[string]any{"fb": "claw", "rescue": "off"})
			err := (&Engine{workflow: tc.wf, executor: forwardsTheTwoOlderSeams{exec, exec}}).validateWorkspaceSafety("fan", tc.wf.Edges)
			if err == nil {
				t.Fatalf("precondition: an executor that leaves the routes unread refuses this pair — a clean verdict says nothing about the clause")
			}
			// The sweep parses the join the formatter prints: a two-branch
			// refusal must split into two causes, or a join the formatter
			// stops printing reads every refusal as one whole-text subject —
			// and route words anywhere in it satisfy the implication below.
			if msg := err.Error(); strings.Contains(msg, "2 branches contain mutating nodes") {
				if got := len(causesBeforeTheirClauses(err)); got != 2 {
					t.Errorf("the sweep read %d causes out of a two-branch refusal: it must split on the join mutationCauses prints — %q", got, msg)
				}
			}
			if !strings.Contains(err.Error(), "EffectiveFallbackResolver") {
				return
			}
			withClause++
			for _, subject := range causesBeforeTheirClauses(err) {
				if !namesARoute(subject) {
					t.Errorf("the refusal blames the executor for leaving the routes unread beside a reason whose subject is not a route, so its remedy is not what moves this verdict: %q", subject)
				}
			}
		})
	}
	if withClause == 0 {
		t.Errorf("no program of this corpus reached the clause at all, so the implication above is vacuous: the sweep proves nothing about where it is printed")
	}

	// The sweep exists for the wording site nobody has listed yet, and the
	// shape it is for does not print today: every corpus row builds its two
	// branches alike, so both causes are worded the same. It is held here as
	// the refusal that site would print — the clause beside a subject that is
	// not a route, in the SECOND cause — and the sweep must redden on that
	// cause, not stop at the first one's.
	heterogeneous := errors.New(`workspace safety violation: 2 branches contain mutating nodes [b1 b2] — node "b1": a ` + "`fallbacks:`" + ` route still spelled as {{vars.fb}} is unresolved before the run, so no backend it may take bounds what the node holds (executor EffectiveFallbackResolver does not answer a node's fallback routes: implement EffectiveFallbackBackends); node "b2": writes files, where a ` + "`tools:`" + ` list does not bound what the node holds (executor EffectiveFallbackResolver does not answer a node's fallback routes: implement EffectiveFallbackBackends)`)
	subjects := causesBeforeTheirClauses(heterogeneous)
	if len(subjects) != 2 {
		t.Fatalf("the sweep did not read both causes of the refusal shape it exists for: %q", subjects)
	}
	if namesARoute(subjects[1]) {
		t.Errorf("the sweep misses a second cause pairing the clause with a subject that is not a route: %q", subjects[1])
	}
}
