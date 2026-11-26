package ciguard

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// ciEvent is one trigger of tests.yml as a job condition sees it: the
// `github` context the expressions read, whether the experimental delivery
// mode (docs/adr/173-experimental-delivery-mode.md) runs the heavy layer
// there, and the `needs` context heavyGateJob leaves behind it.
type ciEvent struct {
	name   string
	github map[string]any
	full   bool
	needs  map[string]any
}

// gateNeeds is the `needs` context heavyGateJob leaves: its result and, when
// it answered, its `heavy` output. GitHub renders a skipped or failed job's
// outputs empty.
func gateNeeds(result, heavy string) map[string]any {
	outputs := map[string]any{}
	if heavy != "" {
		outputs["heavy"] = heavy
	}
	return map[string]any{heavyGateJob: map[string]any{"result": result, "outputs": outputs}}
}

// ciEvents are the runs tests.yml serves. The merge queue, an unlabelled
// pull request and a push heavyGateJob decided light are LIGHT; the push the
// gate decided heavy, a push whose gate failed (failing toward more testing),
// the nightly, a dispatch and a `ci:full` pull request run the full suite.
// The gate skips every event but push.
var ciEvents = []ciEvent{
	{"push to main, heavy window elapsed", map[string]any{"event_name": "push", "event": map[string]any{}}, true, gateNeeds("success", "true")},
	{"push to main inside the heavy window", map[string]any{"event_name": "push", "event": map[string]any{}}, false, gateNeeds("success", "false")},
	{"push to main, heavy-gate failed", map[string]any{"event_name": "push", "event": map[string]any{}}, true, gateNeeds("failure", "")},
	{"nightly schedule", map[string]any{"event_name": "schedule", "event": map[string]any{}}, true, gateNeeds("skipped", "")},
	{"workflow_dispatch", map[string]any{"event_name": "workflow_dispatch", "event": map[string]any{}}, true, gateNeeds("skipped", "")},
	{"merge queue", map[string]any{"event_name": "merge_group", "event": map[string]any{
		"merge_group": map[string]any{"head_commit": map[string]any{"message": "feat(x): a change (#1)"}},
	}}, false, gateNeeds("skipped", "")},
	{"unlabelled pull request", map[string]any{"event_name": "pull_request", "event": map[string]any{
		"pull_request": map[string]any{"title": "feat(x): a change", "labels": []any{map[string]any{"name": "bug"}}},
	}}, false, gateNeeds("skipped", "")},
	{"ci:full pull request", map[string]any{"event_name": "pull_request", "event": map[string]any{
		"pull_request": map[string]any{"title": "feat(x): a change", "labels": []any{map[string]any{"name": "bug"}, map[string]any{"name": "ci:full"}}},
	}}, true, gateNeeds("skipped", "")},
}

// TestEachEventRunsItsLayer EVALUATES the workflow's own conditions for each
// event instead of comparing their spelling: the heavy jobs' `if:` and the
// aggregators' light-run env are read from tests.yml and evaluated against the
// event's context. Per event:
//
//   - every heavy job (heavyLegs, heavyAdvisoryJobs) runs exactly where the
//     event is full — the merge queue runs only the light layer;
//   - heavyGateJob runs on push and nowhere else;
//   - every other gated leg and every required job runs on every event;
//   - every aggregator is green with the heavy legs at the result the event
//     gives them (`success` where they run, `skipped` where they skip), and
//     RED when a heavy leg reports `skipped` on a full event — on push, the
//     nightly, a dispatch and `ci:full`, a skipped heavy leg is a red one.
func TestEachEventRunsItsLayer(t *testing.T) {
	var wf struct{ Jobs map[string]map[string]any }
	if err := yaml.Unmarshal(mustRead(t, workflowPaths[0]), &wf); err != nil {
		t.Fatalf("parse %s: %v", workflowPaths[0], err)
	}
	str := func(v any) string { s, _ := v.(string); return s }
	aggregators := 0
	for _, ev := range ciEvents {
		runs := func(job string) bool {
			cond := str(wf.Jobs[job]["if"])
			if strings.TrimSpace(cond) == "" {
				return true
			}
			v, err := evalContexts(cond, ev.github, ev.needs)
			if err != nil {
				t.Fatalf("job %q `if:` %q: %v", job, cond, err)
			}
			return truthy(v)
		}
		for name := range wf.Jobs {
			heavy := heavyLegs[name] || heavyAdvisoryJobs[name]
			switch got := runs(name); {
			case heavy && got != ev.full:
				t.Errorf("%s: heavy job %q runs=%v, want %v — the heavy layer runs on the debounced push, the nightly, a dispatch and `ci:full`, and nowhere else", ev.name, name, got, ev.full)
			case name == heavyGateJob && got != (ev.github["event_name"] == "push"):
				t.Errorf("%s: %q runs=%v, want it on push and nowhere else", ev.name, name, got)
			case !heavy && (gatedJobs[name] || requiredChecks[name] || heldChecks[name]) && !got:
				t.Errorf("%s: light-layer job %q does not run — the light layer runs on every event", ev.name, name)
			}
		}
		for name, job := range wf.Jobs {
			needs, _ := job["needs"].([]any)
			if len(aggregatedLegs(needs)) == 0 {
				continue
			}
			if ev.name == ciEvents[0].name {
				aggregators++
			}
			env, _ := job["env"].(map[string]any)
			// verdict runs the aggregator's steps with every leg at the result
			// the event gives it, except heavy legs forced to heavyResult when
			// it is not empty, and env rendered the way GitHub renders it.
			verdict := func(heavyResult string) (bool, string) {
				vars := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()}
				for k, v := range env {
					value := str(v)
					if leg, ok := strings.CutPrefix(value, "${{ needs."); ok {
						leg = strings.TrimSuffix(leg, ".result }}")
						result := "success"
						switch {
						case heavyLegs[leg] && heavyResult != "":
							result = heavyResult
						case !runs(leg):
							result = "skipped"
						}
						vars = append(vars, k+"="+result)
						continue
					}
					rendered, err := renderValue(value, ev.github, ev.needs)
					if err != nil {
						t.Fatalf("aggregator %q env %s=%q: %v", name, k, value, err)
					}
					vars = append(vars, k+"="+rendered)
				}
				var out strings.Builder
				for _, s := range job["steps"].([]any) {
					cmd := exec.Command("bash", "--noprofile", "--norc", "-eo", "pipefail", "-c", str(s.(map[string]any)["run"]))
					cmd.Dir, cmd.Env = t.TempDir(), vars
					b, err := cmd.CombinedOutput()
					out.Write(b)
					if err != nil {
						return false, out.String()
					}
				}
				return true, out.String()
			}
			if green, out := verdict(""); !green {
				t.Errorf("%s: aggregator %q is RED with every leg at the result the event gives it — the run this mode designs could never pass:\n%s", ev.name, name, out)
			}
			if ev.full {
				if green, _ := verdict("skipped"); green {
					t.Errorf("%s: aggregator %q stays GREEN with its heavy legs skipped — on a full run a skipped heavy leg must be red", ev.name, name)
				}
			}
		}
	}
	if aggregators != 2 {
		t.Fatalf("found %d aggregators, want test and race", aggregators)
	}
}

// TestTheExpressionEvaluatorReadsGitHubsRules pins the evaluator the test
// above trusts, on the rules the workflow's conditions lean on: `||` and `&&`
// yield an operand, a missing property is null, string comparison and
// startsWith/contains ignore case, `*` maps over an array.
func TestTheExpressionEvaluatorReadsGitHubsRules(t *testing.T) {
	ctx := map[string]any{"event_name": "push", "event": map[string]any{
		"pull_request": map[string]any{"title": "Chore(Brew): update tap to v1", "labels": []any{map[string]any{"name": "CI:FULL"}}},
	}}
	for _, tc := range []struct {
		expr string
		want any
	}{
		{"github.event_name == 'PUSH'", true},
		{"github.event_name != 'push'", false},
		{"github.event.merge_group.head_commit.message || 'fallback'", "fallback"},
		{"github.event.merge_group || github.event_name", "push"},
		{"github.event_name && ''", ""},
		{"startsWith(github.event.pull_request.title, 'chore(brew): ')", true},
		{"contains(github.event.pull_request.labels.*.name, 'ci:full')", true},
		{"contains(github.event.missing.*.name, 'ci:full')", false},
		{"!(github.event_name == 'push' || false) && true", false},
		{"${{ !cancelled() && true }}", true},
		{"github.event.nope", nil},
	} {
		got, err := evalExpression(tc.expr, ctx)
		if err != nil {
			t.Errorf("%q: %v", tc.expr, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%q = %#v, want %#v", tc.expr, got, tc.want)
		}
	}
	for _, bad := range []string{"github.event_name ==", "format('{0}', 1)", "(true", "'unterminated"} {
		if _, err := evalExpression(bad, ctx); err == nil {
			t.Errorf("%q evaluated without error; the evaluator must refuse what it does not read", bad)
		}
	}
}

// renderValue renders an env value the way GitHub does: a value that is
// exactly one `${{ }}` becomes its result, a boolean as `true` / `false`;
// anything else is refused, since the aggregator's env carries no other shape.
func renderValue(value string, github, needs map[string]any) (string, error) {
	if !strings.HasPrefix(value, "${{") || !strings.HasSuffix(value, "}}") {
		return "", fmt.Errorf("not a single expression")
	}
	v, err := evalContexts(value, github, needs)
	if err != nil {
		return "", err
	}
	switch x := v.(type) {
	case bool:
		return fmt.Sprint(x), nil
	case string:
		return x, nil
	case nil:
		return "", nil
	}
	return "", fmt.Errorf("renders %T, which this evaluator does not format", v)
}

// truthy is GitHub's coercion to boolean for the types the evaluator yields.
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	}
	return true
}

var exprToken = regexp.MustCompile(`^\s*(\|\||&&|==|!=|!|\(|\)|,|'(?:[^']|'')*'|[A-Za-z_][A-Za-z0-9_-]*(?:\.(?:[A-Za-z_][A-Za-z0-9_-]*|\*))*)`)

// evalExpression evaluates the subset of GitHub Actions expressions the
// workflow's job conditions use: string literals, true/false/null, context
// paths under `github` (with `*` over arrays), `!`, `==`, `!=`, `&&`, `||`,
// parentheses and the functions cancelled(), startsWith() and contains().
// Anything else is an error, never a guess. A `needs` path is refused: it
// reads through evalContexts, which models that context.
func evalExpression(expr string, github map[string]any) (any, error) {
	return evalContexts(expr, github, nil)
}

// evalContexts is evalExpression with the `needs` context modelled too; a
// nil needs refuses every `needs` path rather than reading it as null.
func evalContexts(expr string, github, needs map[string]any) (any, error) {
	expr = strings.TrimSpace(expr)
	if strings.HasPrefix(expr, "${{") && strings.HasSuffix(expr, "}}") {
		expr = strings.TrimSpace(expr[3 : len(expr)-2])
	}
	var toks []string
	for rest := expr; strings.TrimSpace(rest) != ""; {
		m := exprToken.FindStringSubmatch(rest)
		if m == nil {
			return nil, fmt.Errorf("unreadable at %q", rest)
		}
		toks = append(toks, m[1])
		rest = rest[len(m[0]):]
	}
	p := &exprParser{toks: toks, github: github, needs: needs}
	v, err := p.or()
	if err != nil {
		return nil, err
	}
	if p.pos != len(p.toks) {
		return nil, fmt.Errorf("trailing tokens %v", p.toks[p.pos:])
	}
	return v, nil
}

type exprParser struct {
	toks   []string
	pos    int
	github map[string]any
	needs  map[string]any
}

func (p *exprParser) peek() string {
	if p.pos < len(p.toks) {
		return p.toks[p.pos]
	}
	return ""
}

func (p *exprParser) or() (any, error) {
	v, err := p.and()
	for err == nil && p.peek() == "||" {
		p.pos++
		var r any
		if r, err = p.and(); err == nil && !truthy(v) {
			v = r
		}
	}
	return v, err
}

func (p *exprParser) and() (any, error) {
	v, err := p.eq()
	for err == nil && p.peek() == "&&" {
		p.pos++
		var r any
		if r, err = p.eq(); err == nil && truthy(v) {
			v = r
		}
	}
	return v, err
}

func (p *exprParser) eq() (any, error) {
	l, err := p.unary()
	if err != nil {
		return nil, err
	}
	if op := p.peek(); op == "==" || op == "!=" {
		p.pos++
		r, err := p.unary()
		if err != nil {
			return nil, err
		}
		same := looseEqual(l, r)
		return same == (op == "=="), nil
	}
	return l, nil
}

func (p *exprParser) unary() (any, error) {
	if p.peek() == "!" {
		p.pos++
		v, err := p.unary()
		return !truthy(v), err
	}
	return p.primary()
}

func (p *exprParser) primary() (any, error) {
	tok := p.peek()
	if tok == "" {
		return nil, fmt.Errorf("unexpected end of expression")
	}
	p.pos++
	switch {
	case tok == "(":
		v, err := p.or()
		if err != nil {
			return nil, err
		}
		if p.peek() != ")" {
			return nil, fmt.Errorf("missing )")
		}
		p.pos++
		return v, nil
	case strings.HasPrefix(tok, "'"):
		if len(tok) < 2 || !strings.HasSuffix(tok, "'") {
			return nil, fmt.Errorf("unterminated string %s", tok)
		}
		return strings.ReplaceAll(tok[1:len(tok)-1], "''", "'"), nil
	case tok == "true", tok == "false":
		return tok == "true", nil
	case tok == "null":
		return nil, nil
	case p.peek() == "(":
		return p.call(tok)
	case tok == "github" || strings.HasPrefix(tok, "github."):
		return resolvePath(p.github, strings.Split(tok, ".")[1:]), nil
	case tok == "needs" || strings.HasPrefix(tok, "needs."):
		if p.needs == nil {
			return nil, fmt.Errorf("%s reads the needs context, which this evaluation does not model", tok)
		}
		return resolvePath(p.needs, strings.Split(tok, ".")[1:]), nil
	}
	return nil, fmt.Errorf("unsupported token %q", tok)
}

func (p *exprParser) call(name string) (any, error) {
	p.pos++ // (
	var args []any
	for p.peek() != ")" {
		if len(args) > 0 {
			if p.peek() != "," {
				return nil, fmt.Errorf("%s: missing , between arguments", name)
			}
			p.pos++
		}
		v, err := p.or()
		if err != nil {
			return nil, err
		}
		args = append(args, v)
	}
	p.pos++ // )
	switch {
	case name == "cancelled" && len(args) == 0:
		return false, nil
	case name == "startsWith" && len(args) == 2:
		return strings.HasPrefix(strings.ToLower(fmt.Sprint(orEmpty(args[0]))), strings.ToLower(fmt.Sprint(orEmpty(args[1])))), nil
	case name == "contains" && len(args) == 2:
		if list, ok := args[0].([]any); ok {
			for _, item := range list {
				if looseEqual(item, args[1]) {
					return true, nil
				}
			}
			return false, nil
		}
		return strings.Contains(strings.ToLower(fmt.Sprint(orEmpty(args[0]))), strings.ToLower(fmt.Sprint(orEmpty(args[1])))), nil
	}
	return nil, fmt.Errorf("unsupported function %s/%d", name, len(args))
}

func orEmpty(v any) any {
	if v == nil {
		return ""
	}
	return v
}

// looseEqual is GitHub's `==` for the values here: strings compare without
// case, null equals null, booleans compare as booleans, and an object or an
// array equals nothing here.
func looseEqual(a, b any) bool {
	switch x := a.(type) {
	case string:
		y, ok := b.(string)
		return ok && strings.EqualFold(x, y)
	case bool:
		y, ok := b.(bool)
		return ok && x == y
	case nil:
		return b == nil
	}
	return false
}

// resolvePath walks a context path; `*` maps the rest of the path over an
// array, and a missing property is null.
func resolvePath(v any, path []string) any {
	for i, seg := range path {
		if seg == "*" {
			list, ok := v.([]any)
			if !ok {
				return []any{}
			}
			out := []any{}
			for _, item := range list {
				out = append(out, resolvePath(item, path[i+1:]))
			}
			return out
		}
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[seg]
	}
	return v
}

// TestTriggersAndConcurrencyKeepEachWitness holds tests.yml's `on:` and
// `concurrency:` to the compensations of the experimental delivery mode
// (docs/adr/173-experimental-delivery-mode.md): every event ciEvents
// designs is a real trigger, the nightly carries at least one cron, and the
// concurrency group and cancel-in-progress, rendered for each event, bound the
// post-merge witness while a newer commit of a pull request still cancels the
// older run.
//
// Every push to `main` shares ONE group without cancel-in-progress: one push
// run in flight, at most one pending, a newer pending run replacing the older
// one, and the run in progress never cancelled. The heavy load after merges is
// one witness of the merged batch at a time, not one per merge. A `queue:` key
// on the group would keep every pending push run and lift that bound. The
// nightly and a dispatch keep groups of their own, so no push displaces them.
func TestTriggersAndConcurrencyKeepEachWitness(t *testing.T) {
	var wf struct {
		On          map[string]any `yaml:"on"`
		Concurrency struct {
			Group            string `yaml:"group"`
			CancelInProgress any    `yaml:"cancel-in-progress"`
			Queue            any    `yaml:"queue"`
		} `yaml:"concurrency"`
	}
	if err := yaml.Unmarshal(mustRead(t, workflowPaths[0]), &wf); err != nil {
		t.Fatalf("parse %s: %v", workflowPaths[0], err)
	}
	for _, ev := range ciEvents {
		name := ev.github["event_name"].(string)
		if _, ok := wf.On[name]; !ok {
			t.Errorf("%s: event %q is not a trigger of %s", ev.name, name, workflowPaths[0])
		}
	}
	if crons, _ := wf.On["schedule"].([]any); len(crons) == 0 {
		t.Errorf("the nightly has no cron: `on.schedule` is %#v, want at least one `- cron:` entry", wf.On["schedule"])
	}
	// Each event carries only the payload GitHub gives it: a push its
	// before/after/compare and head_commit, a pull request its
	// pull_request, the nightly and a dispatch none. Every per-push or
	// per-run field it models is distinct per sha; run_attempt stays "1", as
	// two first attempts share it. renderTemplate renders an undefined path
	// as "", so a group keyed on a path no context models would render alike
	// for two pushes here and differ on GitHub: the strict check below fails
	// on any `github.` path the concurrency block reads that ctxFor lacks.
	ctxFor := func(event, ref, sha string) map[string]any {
		payload := map[string]any{}
		switch event {
		case "push":
			payload = map[string]any{
				"after": sha, "before": "before-" + sha, "compare": "compare-" + sha,
				"head_commit": map[string]any{
					"id": sha, "timestamp": "ts-" + sha, "message": "msg-" + sha, "tree_id": "tree-" + sha,
				},
			}
		case "pull_request":
			payload = map[string]any{"pull_request": map[string]any{"head": map[string]any{"sha": sha}}}
		}
		return map[string]any{
			"workflow": "🧪 Tests", "event_name": event, "ref": ref, "sha": sha,
			"run_id": "run-" + sha, "run_number": "n-" + sha, "run_attempt": "1",
			"event": payload,
		}
	}
	exprs := []string{wf.Concurrency.Group}
	if v, ok := wf.Concurrency.CancelInProgress.(string); ok {
		exprs = append(exprs, v)
	}
	for _, expr := range exprs {
		for _, m := range githubPath.FindAllStringSubmatch(expr, -1) {
			path := strings.Split(m[1][1:], ".")
			modelled := false
			for _, ev := range []string{"push", "pull_request", "schedule", "workflow_dispatch"} {
				if resolvePath(ctxFor(ev, "r", "s"), path) != nil {
					modelled = true
				}
			}
			if !modelled {
				t.Fatalf("concurrency reads github%s, which ctxFor does not model: it would render \"\" here and a real value on GitHub", m[1])
			}
		}
	}
	group := func(event, ref, sha string) string {
		g, err := renderTemplate(wf.Concurrency.Group, ctxFor(event, ref, sha))
		if err != nil {
			t.Fatalf("concurrency group %q: %v", wf.Concurrency.Group, err)
		}
		return g
	}
	cancels := func(event, ref string) bool {
		switch v := wf.Concurrency.CancelInProgress.(type) {
		case bool:
			return v
		case string:
			r, err := renderTemplate(v, ctxFor(event, ref, "aaa"))
			if err != nil {
				t.Fatalf("cancel-in-progress %q: %v", v, err)
			}
			switch r {
			case "true":
				return true
			case "false":
				return false
			}
			t.Fatalf("cancel-in-progress %q renders %q on %s, want true or false", v, r, event)
		}
		t.Fatalf("cancel-in-progress is %#v, want a boolean or an expression", wf.Concurrency.CancelInProgress)
		return false
	}
	const main = "refs/heads/main"
	if a, b := group("push", main, "aaa"), group("push", main, "bbb"); a != b {
		t.Errorf("two pushes to main land in different groups (%q, %q): every merge starts its own heavy run, and the post-merge load is no longer capped at one witness", a, b)
	}
	if cancels("push", main) {
		t.Error("cancel-in-progress is true on push: each merge would cancel the witness in progress, and a steady merge rate would never finish one")
	}
	if wf.Concurrency.Queue != nil {
		t.Errorf("concurrency carries `queue: %v`: every pending push run would be kept, one heavy run per merge, and the witness is no longer bounded", wf.Concurrency.Queue)
	}
	for _, other := range []string{"schedule", "workflow_dispatch"} {
		if p, o := group("push", main, "aaa"), group(other, main, "aaa"); p == o {
			t.Errorf("push and %s on main share the concurrency group %q: a merge would displace the %s run", other, p, other)
		}
	}
	if s, d := group("schedule", main, "aaa"), group("workflow_dispatch", main, "aaa"); s == d {
		t.Errorf("the nightly and a dispatch on main share the concurrency group %q: each cancels the other", s)
	}
	if a, b := group("pull_request", "refs/pull/1/merge", "aaa"), group("pull_request", "refs/pull/1/merge", "bbb"); a != b {
		t.Errorf("two commits of one pull request land in different groups (%q, %q): a superseded commit's run is no longer cancelled", a, b)
	}
	if a, b := group("pull_request", "refs/pull/1/merge", "aaa"), group("pull_request", "refs/pull/2/merge", "aaa"); a == b {
		t.Errorf("two pull requests share the concurrency group %q: one cancels the other", a)
	}
	if !cancels("pull_request", "refs/pull/1/merge") {
		t.Error("cancel-in-progress is false on pull_request: a superseded pull request commit keeps its run")
	}
}

// TestDocsLinksDiffStepRunsOnlyWhereItHasABase pins the "Links broken by this
// change" step to the events whose payload names a base (`pull_request`'s
// base sha, `push`'s `before`). On the nightly and a dispatch SINCE would
// render empty and docslinks would report the whole backlog as broken by
// "this change"; the informational whole-tree step covers those runs.
func TestDocsLinksDiffStepRunsOnlyWhereItHasABase(t *testing.T) {
	var wf struct {
		Jobs map[string]struct {
			If    string           `yaml:"if"`
			Steps []map[string]any `yaml:"steps"`
		}
	}
	if err := yaml.Unmarshal(mustRead(t, workflowPaths[0]), &wf); err != nil {
		t.Fatalf("parse %s: %v", workflowPaths[0], err)
	}
	job, ok := wf.Jobs["docs-links"]
	if !ok {
		t.Fatalf("%s has no docs-links job", workflowPaths[0])
	}
	var cond string
	found := false
	for _, s := range job.Steps {
		if s["name"] == "Links broken by this change" {
			found = true
			cond, _ = s["if"].(string)
		}
	}
	if !found {
		t.Fatalf("docs-links has no step %q", "Links broken by this change")
	}
	for _, ev := range ciEvents {
		jobRuns := true
		if strings.TrimSpace(job.If) != "" {
			v, err := evalExpression(job.If, ev.github)
			if err != nil {
				t.Fatalf("docs-links `if:` %q: %v", job.If, err)
			}
			jobRuns = truthy(v)
		}
		if !jobRuns {
			continue
		}
		stepRuns := true
		if strings.TrimSpace(cond) != "" {
			v, err := evalExpression(cond, ev.github)
			if err != nil {
				t.Fatalf("step `if:` %q: %v", cond, err)
			}
			stepRuns = truthy(v)
		}
		name := ev.github["event_name"].(string)
		hasBase := name == "pull_request" || name == "push"
		if stepRuns != hasBase {
			t.Errorf("%s: \"Links broken by this change\" runs=%v, want %v — it diffs against a base only pull_request and push carry", ev.name, stepRuns, hasBase)
		}
	}
}

// githubPath matches a `github.` context path inside an expression.
var githubPath = regexp.MustCompile(`\bgithub((?:\.[A-Za-z0-9_*-]+)+)`)

var templateExpr = regexp.MustCompile(`\$\{\{.*?\}\}`)

// renderTemplate renders a string holding any number of `${{ }}`
// expressions, each through evalExpression, the way GitHub renders a
// concurrency group: null as the empty string, a boolean as true/false.
func renderTemplate(s string, github map[string]any) (string, error) {
	var firstErr error
	out := templateExpr.ReplaceAllStringFunc(s, func(m string) string {
		v, err := evalExpression(m, github)
		if err != nil && firstErr == nil {
			firstErr = fmt.Errorf("%s: %w", m, err)
		}
		switch x := v.(type) {
		case nil:
			return ""
		case string:
			return x
		case bool:
			return fmt.Sprint(x)
		}
		if firstErr == nil {
			firstErr = fmt.Errorf("%s renders %T, which this evaluator does not format", m, v)
		}
		return ""
	})
	return out, firstErr
}
