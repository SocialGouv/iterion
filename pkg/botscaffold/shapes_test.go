package botscaffold

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/runview"
)

// scaffoldAndCompile scaffolds spec and compiles the written main.bot the
// way a launch does — through the bundle loader and the runtime's own
// prompt merge — returning the bundle dir, the workflow, and how many
// prompts main.bot declared ITSELF (before the bundle's prompts/*.md).
func scaffoldAndCompile(t *testing.T, spec Spec) (string, *ir.Workflow, int) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), spec.Slug)
	if _, err := Scaffold(dir, spec); err != nil {
		t.Fatalf("Scaffold(%s): %v", spec.Shape, err)
	}
	src, err := os.ReadFile(filepath.Join(dir, "main.bot"))
	if err != nil {
		t.Fatal(err)
	}
	pr := parser.Parse("main.bot", string(src))
	if len(pr.Diagnostics) != 0 {
		t.Fatalf("%s: parse: %v", spec.Shape, pr.Diagnostics)
	}
	declared := len(pr.File.Prompts)
	b, err := bundle.OpenDir(dir)
	if err != nil {
		t.Fatalf("OpenDir: %v", err)
	}
	if err := runview.MergeBundlePrompts(pr.File, b); err != nil {
		t.Fatal(err)
	}
	cr := ir.Compile(pr.File)
	for _, d := range cr.Diagnostics {
		if d.Severity == ir.SeverityError && d.Code != ir.DiagMissingModelOrBackend {
			t.Errorf("%s: compile: %s", spec.Shape, d.Error())
		}
	}
	if cr.Workflow == nil {
		t.Fatalf("%s: no workflow", spec.Shape)
	}
	return dir, cr.Workflow, declared
}

// nodesOf collects the workflow's nodes of one concrete type.
func nodesOf[T ir.Node](w *ir.Workflow) []T {
	var out []T
	for _, n := range w.Nodes {
		if v, ok := n.(T); ok {
			out = append(out, v)
		}
	}
	return out
}

// typedFails are the workflow's DECLARED `fail <name>:` nodes — the
// implicit `fail` target every workflow carries is not one.
func typedFails(w *ir.Workflow) []*ir.FailNode {
	var out []*ir.FailNode
	for _, f := range nodesOf[*ir.FailNode](w) {
		if f.ID != "fail" {
			out = append(out, f)
		}
	}
	return out
}

func exists(t *testing.T, dir, rel string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel)))
	return err == nil
}

// edge returns the workflow's edge from → to, or nil.
func edge(w *ir.Workflow, from, to string) *ir.Edge {
	for _, e := range w.Edges {
		if e.From == from && e.To == to {
			return e
		}
	}
	return nil
}

// wantExit asserts the guardless exit edge from → to — the
// loop-exhaustion exit (a bare edge beside conditional siblings) or an
// `else` — the edge a shape exists to teach and the compiler does not
// require: without it the run dies NO_OUTGOING_EDGE with `validate` silent.
func wantExit(t *testing.T, w *ir.Workflow, from, to string) {
	t.Helper()
	e := edge(w, from, to)
	if e == nil {
		t.Errorf("want the exit edge %s -> %s", from, to)
		return
	}
	if e.Condition != "" || e.Expression != nil {
		t.Errorf("the exit edge %s -> %s is guarded (%q); it must be the bare fall-through", from, to, e.ExpressionSrc+e.Condition)
	}
}

// TestGalleryShapes holds every shape to the FORM it exists to teach.
// TestScaffold_SpecMatrix proves each one compiles; this proves that a
// shape edited down to the single-agent graph turns red — the loop
// template has its bounded back-edge and its typed fail, the fan-out
// template its router and its wait_all convergence, and so on.
func TestGalleryShapes(t *testing.T) {
	checks := map[string]func(t *testing.T, dir string, w *ir.Workflow, declaredPrompts int){
		"campaign-loop": func(t *testing.T, dir string, w *ir.Workflow, _ int) {
			if len(w.Loops) != 1 {
				t.Errorf("want one bounded loop, got %d", len(w.Loops))
			}
			if len(typedFails(w)) != 2 {
				t.Errorf("want the typed exhaustion and the typed misconfiguration, got %d typed fails", len(typedFails(w)))
			}
			if len(nodesOf[*ir.ToolNode](w)) != 1 {
				t.Errorf("want the verify tool")
			}
			if len(nodesOf[*ir.ComputeNode](w)) != 3 {
				t.Errorf("want the entry gate, the first-pass input and the deterministic gate, got %d computes", len(nodesOf[*ir.ComputeNode](w)))
			}
			// No verifier, no campaign: the entry gate refuses an unset
			// verify_command before any LLM call — a verifier that checks
			// nothing would make every verdict green.
			if w.Entry != "check" {
				t.Errorf("entry = %q, want the check gate", w.Entry)
			}
			if e := edge(w, "check", "misconfigured"); e == nil || e.Condition != "configured" || !e.Negated {
				t.Errorf("want check -> misconfigured when not configured, got %+v", e)
			}
			if e := edge(w, "check", "first_pass"); e == nil || e.Condition != "configured" || e.Negated {
				t.Errorf("want check -> first_pass when configured, got %+v", e)
			}
			if v, ok := w.Vars["verify_command"]; !ok || v.Default != "" {
				t.Errorf("verify_command default = %v; a placeholder verifier must not be a passing one", v)
			}
			if w.Worktree != "auto" {
				t.Errorf("worktree = %q, want auto (the campaign commits)", w.Worktree)
			}
			// The back-edge carries the verifier's output onto the next
			// pass; the exhaustion exit is the bare fall-through.
			if e := edge(w, "gate", "campaign"); e == nil || e.LoopName == "" || len(e.With) == 0 {
				t.Errorf("want the loop back-edge gate -> campaign carrying a with-mapping, got %+v", e)
			}
			// `as name(N)` allows N crossings = N+1 passes: the cap is derived
			// from max_passes-1 by the GATE on every pass (a resume never re-runs the
			// entry), so max_passes counts PASSES and a raised cap resumes.
			if l, ok := w.Loops["passes"]; !ok || !strings.Contains(l.MaxIterationsExpr, "outputs.gate.passes_after_first") {
				t.Errorf("loop passes cap = %+v; want it derived from the gate's passes_after_first", l)
			}
			wantExit(t, w, "gate", "passes_exhausted")
		},
		"review-fanout": func(t *testing.T, dir string, w *ir.Workflow, _ int) {
			// The reviewers read and never write: the gate is on, in deny
			// mode, with a read-only allow list, and a write tool is denied
			// by name — in place, they work in the operator's checkout.
			if w.Permission != "deny" {
				t.Errorf("permission = %q, want deny", w.Permission)
			}
			for _, rule := range []string{"Read(**)", "Grep", "Bash(git diff:*)"} {
				if !slices.Contains(w.PermissionAllow, rule) {
					t.Errorf("allow list lacks %s: %v", rule, w.PermissionAllow)
				}
			}
			for _, rule := range []string{"Edit(**)", "Write(**)", "Bash(git commit:*)"} {
				if !slices.Contains(w.PermissionDeny, rule) {
					t.Errorf("deny list lacks %s: %v", rule, w.PermissionDeny)
				}
			}
			var fanOut int
			for _, r := range nodesOf[*ir.RouterNode](w) {
				if r.RouterMode == ir.RouterFanOutAll {
					fanOut++
				}
			}
			if fanOut != 1 {
				t.Errorf("want one fan_out_all router, got %d", fanOut)
			}
			var readonly int
			for _, a := range nodesOf[*ir.AgentNode](w) {
				if a.Readonly {
					readonly++
				}
			}
			if readonly < 2 {
				t.Errorf("want at least two read-only reviewers, got %d", readonly)
			}
			// Read-only in FACT — a canary for the trap the shape shipped with,
			// not a proof of read-only-ness: no prompt of the shape says
			// `git add` (an intent-to-add in a parallel branch takes
			// .git/index.lock, is fatal when the sibling holds it, and the
			// loser's empty findings would read as an approve).
			for name, p := range w.Prompts {
				if strings.Contains(p.Body, "git add") {
					t.Errorf("prompt %q tells a read-only reviewer to write the index (git add)", name)
				}
			}
			// The convergence requires both reviewers' own account of having
			// read the diff, not merely their not blocking.
			var approved bool
			for _, c := range nodesOf[*ir.ComputeNode](w) {
				if c.ID != "verdict" {
					continue
				}
				for _, e := range c.Exprs {
					if e.Key != "approved" {
						continue
					}
					approved = true
					if !strings.Contains(e.Raw, "review_correctness.reviewed") || !strings.Contains(e.Raw, "review_security.reviewed") {
						t.Errorf("approved = %q; it must require both reviewers' `reviewed`", e.Raw)
					}
				}
			}
			if !approved {
				t.Errorf("want the deterministic `approved` expression on compute verdict")
			}
			var converge int
			for _, c := range nodesOf[*ir.ComputeNode](w) {
				if c.AwaitMode == ir.AwaitWaitAll {
					converge++
				}
			}
			if converge != 1 {
				t.Errorf("want one wait_all compute, got %d", converge)
			}
			// An EMPTY scope is a typed refusal, never an approve: the
			// deterministic gate counts the changed and untracked files before
			// the fan-out, and its two edges are the exhaustive pair.
			if w.Entry != "scope" {
				t.Errorf("entry = %q, want the scope gate", w.Entry)
			}
			if tools := nodesOf[*ir.ToolNode](w); len(tools) != 1 || tools[0].ID != "scope" || tools[0].OutputSchema == "" {
				t.Errorf("want the one scope tool with an output schema, got %+v", tools)
			}
			if e := edge(w, "scope", "nothing_to_review"); e == nil || e.Condition != "empty" || e.Negated {
				t.Errorf("want scope -> nothing_to_review when empty, got %+v", e)
			}
			if e := edge(w, "scope", "split"); e == nil || e.Condition != "empty" || !e.Negated {
				t.Errorf("want scope -> split when not empty, got %+v", e)
			}
			if len(typedFails(w)) != 2 {
				t.Errorf("want the typed blocked verdict and the typed empty-scope refusal, got %d typed fails", len(typedFails(w)))
			}
			// In place by default: a `worktree: auto` run starts from the
			// anchor COMMIT, without the pending work the shape reviews.
			if w.Worktree != "none" {
				t.Errorf("worktree = %q, want none (the pending work IS the scope)", w.Worktree)
			}
			wantExit(t, w, "verdict", "review_blocked")
			if e := edge(w, "verdict", "review_blocked"); e != nil && !e.IsElse {
				t.Errorf("verdict -> review_blocked must be the `else` edge")
			}
		},
		"plan-gate-implement": func(t *testing.T, dir string, w *ir.Workflow, _ int) {
			if len(nodesOf[*ir.HumanNode](w)) != 1 {
				t.Errorf("want the human gate")
			}
			if len(w.Loops) != 1 {
				t.Errorf("want the bounded re-plan loop, got %d loops", len(w.Loops))
			}
			if len(typedFails(w)) != 1 {
				t.Errorf("want the typed rejection")
			}
			if w.Worktree != "auto" {
				t.Errorf("worktree = %q, want auto (the implementer commits)", w.Worktree)
			}
			if len(nodesOf[*ir.ComputeNode](w)) != 1 {
				t.Errorf("want the first-round input compute, got %d computes", len(nodesOf[*ir.ComputeNode](w)))
			}
			if e := edge(w, "approve", "plan"); e == nil || e.LoopName == "" || len(e.With) == 0 {
				t.Errorf("want the re-plan back-edge approve -> plan carrying the feedback, got %+v", e)
			}
			wantExit(t, w, "approve", "plan_rejected")
		},
		"scheduled-digest": func(t *testing.T, dir string, w *ir.Workflow, _ int) {
			if len(nodesOf[*ir.ToolNode](w)) != 2 {
				t.Errorf("want the collect and verify tools, got %d tool nodes", len(nodesOf[*ir.ToolNode](w)))
			}
			if len(typedFails(w)) != 1 {
				t.Errorf("want the typed empty-digest failure")
			}
			wantExit(t, w, "verify_written", "nothing_written")
			m, err := bundle.LoadManifest(filepath.Join(dir, "manifest.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			if len(m.Invocations) != 1 || m.Invocations[0].Kind != bundle.InvocationKindSchedule ||
				m.Invocations[0].Schedule == nil || m.Invocations[0].Schedule.SuggestedCron == "" {
				t.Errorf("manifest invocations = %+v, want one schedule with a suggested cron", m.Invocations)
			}
		},
		"per-ticket-subbots": func(t *testing.T, dir string, w *ir.Workflow, _ int) {
			var each int
			for _, r := range nodesOf[*ir.RouterNode](w) {
				if r.RouterMode == ir.RouterFanOutEach {
					each++
				}
			}
			if each != 1 {
				t.Errorf("want one fan_out_each router, got %d", each)
			}
			subs := nodesOf[*ir.SubbotNode](w)
			if len(subs) != 1 || !subs[0].Isolated || subs[0].Source != "worker.bot" {
				t.Errorf("want one isolated subbot on worker.bot, got %+v", subs)
			}
			var converge int
			for _, c := range nodesOf[*ir.ComputeNode](w) {
				if c.AwaitMode == ir.AwaitWaitAll {
					converge++
				}
			}
			if converge != 1 {
				t.Errorf("want one wait_all compute, got %d", converge)
			}
			// The concurrency bound is what makes a fan-out over an
			// unbounded ticket list safe, and it is the one budget line a
			// shape writes itself. This is also the shape whose workflow
			// block opens a `budget:` the shared partials then render INTO
			// (the worktree dial, the sandbox/permission dials), at the
			// outer indent: a partial that stopped dedenting would turn its
			// own lines into budget properties and drop this one.
			if w.Budget == nil || w.Budget.MaxParallelBranches != 3 {
				t.Errorf("budget = %+v, want the shape's max_parallel_branches: 3", w.Budget)
			}
			// The child ships with the bundle and is a workflow of its own.
			src, err := os.ReadFile(filepath.Join(dir, "worker.bot"))
			if err != nil {
				t.Fatalf("worker.bot: %v", err)
			}
			if err := compileGuard("worker.bot", string(src), nil); err != nil {
				t.Error(err)
			}
		},
		"verified-action": func(t *testing.T, dir string, w *ir.Workflow, _ int) {
			var a *ir.ToolNode
			for _, tool := range nodesOf[*ir.ToolNode](w) {
				if tool.ID == "tag_release" {
					a = tool
				}
			}
			if a == nil || len(nodesOf[*ir.ToolNode](w)) != 2 {
				t.Fatalf("want the tag_free gate and the tag_release action, got %d tool nodes", len(nodesOf[*ir.ToolNode](w)))
			}
			if a.Goal == "" || a.Postcondition == "" || a.Policy != "recover" || a.Recovery == nil {
				t.Errorf("want the full quad (goal, postcondition, policy: recover, recovery), got goal=%q postcondition=%q policy=%q recovery=%v", a.Goal, a.Postcondition, a.Policy, a.Recovery)
			}
			// The shape's whole lesson is that the postcondition's JSON is
			// the node's output on every rung; without the schema wired the
			// declaration is dead (no diagnostic flags an unused schema).
			if a.OutputSchema == "" {
				t.Errorf("the verified action declares no output schema, so its postcondition's JSON reaches nothing")
			} else if _, ok := w.Schemas[a.OutputSchema]; !ok {
				t.Errorf("output schema %q is not declared", a.OutputSchema)
			}
			if w.Worktree != "auto" {
				t.Errorf("worktree = %q, want auto (the action commits and tags)", w.Worktree)
			}
			// The tag is REQUIRED and per run — git tags live in the shared
			// ref store and outlive the worktree, so a fixed default meets
			// its own previous tag on the next run: the entry gate refuses
			// an empty name before the agent runs, and the Spec ships none.
			if w.Entry != "check" {
				t.Errorf("entry = %q, want the check gate", w.Entry)
			}
			if e := edge(w, "check", "tag_unset"); e == nil || e.Condition != "configured" || !e.Negated {
				t.Errorf("want check -> tag_unset when not configured, got %+v", e)
			}
			if e := edge(w, "check", "tag_free"); e == nil || e.Condition != "configured" || e.Negated {
				t.Errorf("want check -> tag_free when configured, got %+v", e)
			}
			// A name already TAKEN is a typed refusal before the agent runs,
			// never a case for the recovery rung — whose only way to "heal" an
			// existing tag would be a force-move in the shared ref store.
			if e := edge(w, "tag_free", "tag_taken"); e == nil || e.Condition != "free" || !e.Negated {
				t.Errorf("want tag_free -> tag_taken when not free, got %+v", e)
			}
			if e := edge(w, "tag_free", "prepare"); e == nil || e.Condition != "free" || e.Negated {
				t.Errorf("want tag_free -> prepare when free, got %+v", e)
			}
			if len(typedFails(w)) != 2 {
				t.Errorf("want the typed unset-tag and taken-tag refusals, got %d typed fails", len(typedFails(w)))
			}
			if v, ok := w.Vars["tag"]; !ok || v.Default != "" {
				t.Errorf("tag default = %v; a fixed tag name collides with itself on the next run", v)
			}
		},
		"async-questions": func(t *testing.T, dir string, w *ir.Workflow, _ int) {
			var async int
			for _, a := range nodesOf[*ir.AgentNode](w) {
				if a.Interaction != ir.InteractionAsync {
					continue
				}
				async++
				// `interaction:` grants the ask tools by ensureToolPresent
				// on THIS list, and on claw a non-empty list is the whole
				// toolset — so an async node that declares none is left
				// with the ask tools alone and cannot do the work it is
				// supposed to keep doing while the answers arrive.
				var working int
				for _, name := range a.Tools {
					switch name {
					case "ask_user", "ask_user_async", "await_answers", "todo_write":
					default:
						working++
					}
				}
				if working == 0 {
					t.Errorf("agent %q declares no working tool (%v); on claw the interaction grant would be its whole toolset", a.ID, a.Tools)
				}
			}
			if async != 1 {
				t.Errorf("want one interaction: async agent, got %d", async)
			}
			gates := nodesOf[*ir.AwaitAnswersNode](w)
			if len(gates) != 1 || gates[0].Timeout <= 0 || gates[0].From == "" {
				t.Errorf("want one await_answers gate with a timeout and a from: node, got %+v", gates)
			}
		},
		"multi-file": func(t *testing.T, dir string, w *ir.Workflow, declared int) {
			if declared != 0 {
				t.Errorf("main.bot declares %d prompt(s); the shape keeps them all in prompts/*.md", declared)
			}
			for _, rel := range []string{"prompts/mission.md", "prompts/kickoff.md", "skills/house-style.md"} {
				if !exists(t, dir, rel) {
					t.Errorf("%s missing from the bundle", rel)
				}
			}
			for _, rel := range []string{"prompts/.gitkeep", "skills/.gitkeep"} {
				if exists(t, dir, rel) {
					t.Errorf("%s written into a directory the shape fills", rel)
				}
			}
			// The mechanism is load-bearing: without the bundle's prompts the
			// workflow does not compile, so a stem nothing ships is refused.
			src, err := os.ReadFile(filepath.Join(dir, "main.bot"))
			if err != nil {
				t.Fatal(err)
			}
			pr := parser.Parse("main.bot", string(src))
			var unknown int
			for _, d := range ir.Compile(pr.File).Diagnostics {
				if d.Code == ir.DiagUnknownPrompt {
					unknown++
				}
			}
			if unknown != 2 {
				t.Errorf("without prompts/*.md want the two prompt refs refused (C003), got %d", unknown)
			}
		},
	}
	for _, tpl := range Templates() {
		if tpl.Spec.Shape == "" {
			continue
		}
		check, ok := checks[tpl.Spec.Shape]
		if !ok {
			t.Errorf("shape %q has no form check", tpl.Spec.Shape)
			continue
		}
		t.Run(tpl.ID, func(t *testing.T) {
			spec := tpl.Spec
			spec.Slug = "shape-" + tpl.ID
			dir, w, declared := scaffoldAndCompile(t, spec)
			check(t, dir, w, declared)
		})
	}
	for shape := range checks {
		if !hasShape(shape) {
			t.Errorf("form check for %q names no shape in the gallery", shape)
		}
	}
}

// singleQuote mirrors the runtime's shell escaping of a substituted ref
// (pkg/backend/model.shellEscape): wrap in single quotes, closing and
// reopening around each one.
func singleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// TestCampaignLoopVerifyEmitsOnlyJSONOnStdout: the verify node's WHOLE
// stdout is parsed as one JSON object (model.parseToolNodeOutput) and, on
// a parse failure, collapses to `{"result": "<the raw text>"}` — no `ok`,
// so the gate expr reads nil and the campaign never converges. The
// repository's own checks therefore have to write on stderr, and this
// runs the rendered command with a verify_command that PRINTS: the
// placeholder default `true` prints nothing and hides the whole class.
func TestCampaignLoopVerifyEmitsOnlyJSONOnStdout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the shape's command is a bash command")
	}
	// Named, and a failure under CI: a runner missing one is a runner to
	// fix, and the message sends the reader there, not to the template.
	requireBins(t, "bash", "jq")
	tpl, ok := TemplateByID("campaign-loop")
	if !ok {
		t.Fatal("the campaign-loop template is gone")
	}
	spec := tpl.Spec
	spec.Slug = "stdout-campaign-loop"
	_, w, _ := scaffoldAndCompile(t, spec)
	tools := nodesOf[*ir.ToolNode](w)
	if len(tools) != 1 {
		t.Fatalf("want the one verify tool, got %d", len(tools))
	}
	for _, c := range []struct {
		name   string
		verify string
		wantOK bool
	}{
		{"green", "echo 'ok  github.com/x/y	0.4s'; true", true},
		{"red", "echo 'FAIL github.com/x/y'; false", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			command := strings.ReplaceAll(tools[0].Command, "{{vars.verify_command}}", singleQuote(c.verify))
			if strings.Contains(command, "{{") {
				t.Fatalf("a ref went unsubstituted, the test no longer runs what the runtime does: %q", command)
			}
			var stdout, stderr bytes.Buffer
			// bash, the shell the runtime pins for a tool node's command
			// (executor_tool.go): /bin/sh is dash on Debian-derived images
			// and cannot read the bashisms the command relies on.
			sh := exec.Command("bash", "-c", command)
			sh.Stdout, sh.Stderr = &stdout, &stderr
			if err := sh.Run(); err != nil {
				t.Fatalf("the node's own command must succeed whatever the checks say: %v (stderr %q)", err, stderr.String())
			}
			var out map[string]any
			if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
				t.Fatalf("stdout is not the single JSON object the runtime parses (%v); the checks' output leaked into it: %q", err, stdout.String())
			}
			ok, isBool := out["ok"].(bool)
			if !isBool {
				t.Fatalf("the node's output carries no bool `ok` (%v); the gate expr would read nil and never converge", out)
			}
			if ok != c.wantOK {
				t.Errorf("ok = %v, want %v", ok, c.wantOK)
			}
			// The checks' own log is what "see the run log" points at:
			// stderr reaches it through combineStreamsForLog.
			if !strings.Contains(stderr.String(), "github.com/x/y") {
				t.Errorf("the checks' own output did not reach stderr, so the run log would not have it: %q", stderr.String())
			}
		})
	}
}

// TestGalleryShapesAreTemplates: every shape directory is offered by
// exactly one template, and every template's shape exists — a shape
// dropped in templates/gallery/ without a gallery entry would be dead
// files, and an entry naming a missing shape a template that cannot
// scaffold.
func TestGalleryShapesAreTemplates(t *testing.T) {
	offered := map[string]int{}
	for _, tpl := range Templates() {
		if tpl.Spec.Shape == "" {
			continue
		}
		if !hasShape(tpl.Spec.Shape) {
			t.Errorf("template %q names unknown shape %q", tpl.ID, tpl.Spec.Shape)
		}
		offered[tpl.Spec.Shape]++
	}
	for _, s := range Shapes() {
		if offered[s] != 1 {
			t.Errorf("shape %q is offered by %d templates, want exactly one", s, offered[s])
		}
	}
	if len(Shapes()) < 8 {
		t.Errorf("gallery holds %d shapes, want the eight canonical ones", len(Shapes()))
	}
}

// TestGalleryShapesRenderEveryDial: each shape rendered with every
// optional dial set on top of its own spec — a model and a backend, skills,
// capabilities, extra vars, worktree, sandbox, permission, a budget, a
// cron — still compiles. A shape that ignores a partial, or fits one into
// the wrong block, turns red here rather than on an operator's form.
func TestGalleryShapesRenderEveryDial(t *testing.T) {
	for _, tpl := range Templates() {
		if tpl.Spec.Shape == "" {
			continue
		}
		t.Run(tpl.ID, func(t *testing.T) {
			spec := tpl.Spec
			spec.Slug = "dials-" + tpl.ID
			spec.DisplayName = "Every Dial"
			spec.Model = "anthropic/claude-opus-4-8"
			spec.Backend = "claude_code"
			spec.Skills = []string{"house-style"}
			spec.Capabilities = []string{"board.read"}
			spec.Vars = append(spec.Vars, VarSpec{Name: "topic", Type: "string", Default: "x", Description: "Extra."}, VarSpec{Name: "depth", Type: "int", Default: "2"})
			spec.Worktree = true
			spec.Sandbox = true
			spec.Permission = "ask"
			spec.MaxCostUSD = 3
			spec.MaxDuration = "1h"
			spec.ScheduleCron = "0 7 * * *"
			dir, w, _ := scaffoldAndCompile(t, spec)
			if w.Name != spec.WorkflowName() {
				t.Errorf("workflow = %q, want %q", w.Name, spec.WorkflowName())
			}
			if w.Worktree != "auto" || w.Sandbox == nil || w.Permission != "ask" || w.Budget == nil || w.Budget.MaxDuration == "" {
				t.Errorf("the dials did not reach the IR: worktree=%q sandbox=%v permission=%q budget=%+v", w.Worktree, w.Sandbox, w.Permission, w.Budget)
			}
			for _, a := range nodesOf[*ir.AgentNode](w) {
				if a.Model != spec.Model || a.Backend != spec.Backend {
					t.Errorf("agent %q: model=%q backend=%q, want the spec's", a.ID, a.Model, a.Backend)
				}
			}
			if !exists(t, dir, "manifest.yaml") {
				t.Errorf("manifest missing")
			}
		})
	}
}

// TestGalleryShapesResolveTheWorktreeDialOff: with the dial explicitly
// OFF, every shape's RESOLVED worktree value is `none` — the value read
// is the IR's, after ir.defaultWorktreeMode, which is the whole point: an
// unset `worktree:` resolves to "auto", so a shape that writes nothing on
// the off branch, or hardcodes `auto`, ships the opposite of the
// operator's explicit choice. The table names every shape, so a shape
// that stops honouring the dial is pinned. Which shapes default the dial
// ON (the ones that commit) is TestBlankTemplateIsolatesByDefault's.
func TestGalleryShapesResolveTheWorktreeDialOff(t *testing.T) {
	want := map[string]string{
		"campaign-loop":       "none",
		"plan-gate-implement": "none",
		"verified-action":     "none",
		"review-fanout":       "none",
		"scheduled-digest":    "none",
		"per-ticket-subbots":  "none",
		"async-questions":     "none",
		"multi-file":          "none",
	}
	seen := map[string]bool{}
	for _, tpl := range Templates() {
		if tpl.Spec.Shape == "" {
			continue
		}
		t.Run(tpl.ID, func(t *testing.T) {
			spec := tpl.Spec
			spec.Slug = "off-" + tpl.ID
			spec.Worktree = false
			_, w, _ := scaffoldAndCompile(t, spec)
			exp, ok := want[spec.Shape]
			if !ok {
				t.Fatalf("shape %q has no expected dial-OFF worktree value — add it to the table", spec.Shape)
			}
			seen[spec.Shape] = true
			if w.Worktree != exp {
				t.Errorf("worktree = %q with the dial off, want %q", w.Worktree, exp)
			}
		})
	}
	for shape := range want {
		if !seen[shape] {
			t.Errorf("shape %q is in the table but no template scaffolds it", shape)
		}
	}
}

// TestSpecValidate_RejectsUnknownShape: the shape is checked by name, with
// the list, before anything renders.
func TestSpecValidate_RejectsUnknownShape(t *testing.T) {
	s := minimalSpec()
	s.Shape = "no-such-shape"
	if err := s.Validate(); err == nil {
		t.Fatal("an unknown shape passed Validate")
	}
}

// TestSpecValidate_RequiresTheShapeVars: a shape's files reference vars
// by name and the vars block is rendered from the Spec, so a var the
// operator dropped from the form (the studio lets a row be deleted) is
// refused by Validate with the missing names — not met as a compiler
// diagnostic behind a 500 after the form. The list is DERIVED from the
// templates, so a shape gaining a reference cannot drift from the check.
func TestSpecValidate_RequiresTheShapeVars(t *testing.T) {
	for _, tpl := range Templates() {
		if tpl.Spec.Shape == "" {
			continue
		}
		refs := shapeVarRefs(tpl.Spec.Shape)
		declared := map[string]bool{}
		for _, v := range tpl.Spec.Vars {
			declared[v.Name] = true
		}
		for _, name := range refs {
			if !declared[name] {
				t.Errorf("template %q references {{vars.%s}} but its Spec does not declare it", tpl.ID, name)
			}
		}
		if len(refs) == 0 {
			continue
		}
		spec := tpl.Spec
		spec.Slug = "novars-" + tpl.ID
		spec.Vars = nil
		err := spec.Validate()
		if err == nil {
			t.Errorf("%s: a spec without the shape's vars (%v) passed Validate", tpl.ID, refs)
			continue
		}
		for _, name := range refs {
			if !strings.Contains(err.Error(), name) {
				t.Errorf("%s: the refusal %q does not name the missing var %q", tpl.ID, err, name)
			}
		}
	}
	// The child worker.bot declares its own vars: they are not the Spec's.
	for _, name := range shapeVarRefs("per-ticket-subbots") {
		if name == "ticket_id" || name == "title" {
			t.Errorf("shapeVarRefs(per-ticket-subbots) lists the child's own var %q", name)
		}
	}
}

// TestCheckAnnexPaths: an annex that would escape the bundle or land on a
// file the scaffold writes itself — main.bot outside the compile guard —
// is refused by name, before anything is written.
func TestCheckAnnexPaths(t *testing.T) {
	for _, rel := range []string{"main.bot", "manifest.yaml", "README.md", ".gitignore", "presets/example.md", "../escape.md", "/abs.md", "prompts/../../x.md"} {
		if err := checkAnnexPaths(map[string][]byte{rel: nil}); err == nil {
			t.Errorf("annex %q was accepted", rel)
		}
	}
	if err := checkAnnexPaths(map[string][]byte{"worker.bot": nil, "prompts/mission.md": nil}); err != nil {
		t.Errorf("legitimate annexes refused: %v", err)
	}
}

// TestScaffold_GeneratedErrorIsTyped: a workflow the compiler refuses
// because of the operator's own text is a GeneratedError, the type the
// creation surfaces answer as a client error with the diagnostic.
func TestScaffold_GeneratedErrorIsTyped(t *testing.T) {
	spec := minimalSpec()
	spec.Instructions = "Mission with {{vars.nope}}, a var nobody declared."
	_, err := Scaffold(filepath.Join(t.TempDir(), spec.Slug), spec)
	var generated *GeneratedError
	if !errors.As(err, &generated) {
		t.Fatalf("want a *GeneratedError, got %T: %v", err, err)
	}
	if !strings.Contains(generated.Detail, "nope") {
		t.Errorf("the error does not carry the diagnostic: %v", err)
	}
}
