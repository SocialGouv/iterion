package botimport

import (
	"fmt"
	"strings"
	"testing"

	"github.com/dop251/goja/token"
)

// These are characterization tests for the parse layer (parseScript and
// its statement walker): they pin CURRENT behavior, quirks included, on
// authored inline sources — never executed, only parsed (goja AST).

func mustParse(t *testing.T, src string) *script {
	t.Helper()
	s, err := parseScript("test.js", []byte(src))
	if err != nil {
		t.Fatalf("parseScript: %v", err)
	}
	return s
}

func kindsOf(steps []*step) []stepKind {
	out := make([]stepKind, len(steps))
	for i, st := range steps {
		out[i] = st.kind
	}
	return out
}

func wantKinds(t *testing.T, steps []*step, want ...stepKind) {
	t.Helper()
	got := kindsOf(steps)
	if len(got) != len(want) {
		t.Fatalf("step kinds = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("step kinds = %v, want %v", got, want)
		}
	}
}

func reportHas(entries []ReportEntry, substr string) bool {
	for _, e := range entries {
		if strings.Contains(e.Text, substr) {
			return true
		}
	}
	return false
}

// ---- statement classification ----

func TestParseStatementClassification(t *testing.T) {
	tests := []struct {
		name  string
		src   string
		kinds []stepKind
		check func(t *testing.T, s *script)
	}{
		{
			name:  "empty source yields no steps",
			src:   "",
			kinds: nil,
		},
		{
			name:  "phase and log calls",
			src:   "phase('Build')\nlog('starting')",
			kinds: []stepKind{stepPhase, stepLog},
			check: func(t *testing.T, s *script) {
				if s.steps[0].text != "Build" {
					t.Errorf("phase text = %q, want Build", s.steps[0].text)
				}
				if s.steps[1].text != "starting" {
					t.Errorf("log text = %q, want starting", s.steps[1].text)
				}
			},
		},
		{
			name:  "log with template renders holes as angle-bracketed snippets",
			src:   "log(`processing ${x} items`)",
			kinds: []stepKind{stepLog},
			check: func(t *testing.T, s *script) {
				if s.steps[0].text != "processing <x> items" {
					t.Errorf("log text = %q", s.steps[0].text)
				}
			},
		},
		{
			name:  "log with no arguments is empty text",
			src:   "log()",
			kinds: []stepKind{stepLog},
			check: func(t *testing.T, s *script) {
				if s.steps[0].text != "" {
					t.Errorf("log text = %q, want empty", s.steps[0].text)
				}
			},
		},
		{
			// QUIRK (pinned): phase() with a non-static argument silently
			// vanishes — no step, no report entry.
			name:  "phase with dynamic argument silently dropped",
			src:   "phase(currentPhase)",
			kinds: nil,
			check: func(t *testing.T, s *script) {
				if s.report.NeedsAttention() {
					t.Errorf("expected NO report entries (current quirk), got %+v", s.report)
				}
			},
		},
		{
			name:  "top-level return",
			src:   "log('a')\nreturn",
			kinds: []stepKind{stepLog, stepReturn},
		},
		{
			name:  "top-level throw is an unmapped statement placeholder",
			src:   "throw new Error('x')",
			kinds: []stepKind{stepUnknown},
			check: func(t *testing.T, s *script) {
				if !reportHas(s.report.Placeholders, "unmapped statement kept as comment") {
					t.Errorf("placeholders = %+v", s.report.Placeholders)
				}
			},
		},
		{
			name:  "debugger statement is an unmapped placeholder",
			src:   "debugger",
			kinds: []stepKind{stepUnknown},
		},
		{
			name:  "try/finally inlines the body and drops handlers",
			src:   "try {\n  await agent('risky', {label: 'risky'})\n} finally {\n  log('cleanup')\n}",
			kinds: []stepKind{stepAgent},
			check: func(t *testing.T, s *script) {
				if !reportHas(s.report.Dropped, "try/catch/finally") {
					t.Errorf("dropped = %+v", s.report.Dropped)
				}
				// The finally block is dropped entirely — no stepLog.
				for _, st := range s.steps {
					if st.kind == stepLog {
						t.Error("finally body must not be walked")
					}
				}
			},
		},
		{
			name:  "helper function declaration dropped by name",
			src:   "function helper(a) { return a }\nagent('x')",
			kinds: []stepKind{stepAgent},
			check: func(t *testing.T, s *script) {
				if !reportHas(s.report.Dropped, "helper function helper()") {
					t.Errorf("dropped = %+v", s.report.Dropped)
				}
			},
		},
		{
			name:  "if/else kept as placeholder comment",
			src:   "if (x) { log('a') } else { log('b') }",
			kinds: []stepKind{stepUnknown},
			check: func(t *testing.T, s *script) {
				if !reportHas(s.report.Placeholders, "if/else branch kept as comment") {
					t.Errorf("placeholders = %+v", s.report.Placeholders)
				}
			},
		},
		{
			name:  "guard-style if return becomes done exit",
			src:   "if (x.ok) return",
			kinds: []stepKind{stepCondExit},
			check: func(t *testing.T, s *script) {
				c := s.steps[0].cond
				if c.exit != "done" {
					t.Errorf("exit = %q, want done", c.exit)
				}
				if c.condJS != "x.ok" {
					t.Errorf("condJS = %q", c.condJS)
				}
			},
		},
		{
			name:  "guard-style if throw becomes fail exit",
			src:   "if (x.bad) throw new Error('no')",
			kinds: []stepKind{stepCondExit},
			check: func(t *testing.T, s *script) {
				if s.steps[0].cond.exit != "fail" {
					t.Errorf("exit = %q, want fail", s.steps[0].cond.exit)
				}
			},
		},
		{
			name:  "side-effect-only if is dropped",
			src:   "if (x.ok) log('fine')",
			kinds: nil,
			check: func(t *testing.T, s *script) {
				if !reportHas(s.report.Dropped, "conditional side-effect dropped") {
					t.Errorf("dropped = %+v", s.report.Dropped)
				}
			},
		},
		{
			name:  "for-of is a placeholder, body not imported",
			src:   "for (const item of items) {\n  await agent(`p ${item}`)\n}",
			kinds: []stepKind{stepUnknown},
			check: func(t *testing.T, s *script) {
				if !reportHas(s.report.Placeholders, "for…of over a collection") {
					t.Errorf("placeholders = %+v", s.report.Placeholders)
				}
			},
		},
		{
			name:  "local mutation dropped",
			src:   "lastFailure = 3",
			kinds: nil,
			check: func(t *testing.T, s *script) {
				if !reportHas(s.report.Dropped, "local mutation") {
					t.Errorf("dropped = %+v", s.report.Dropped)
				}
			},
		},
		{
			name:  "unmapped call kept as comment",
			src:   "fs.readFile('x')",
			kinds: []stepKind{stepUnknown},
			check: func(t *testing.T, s *script) {
				if !reportHas(s.report.Placeholders, "unmapped call kept as comment") {
					t.Errorf("placeholders = %+v", s.report.Placeholders)
				}
			},
		},
		{
			name:  "await agent as bare expression statement",
			src:   "await agent('just do it')",
			kinds: []stepKind{stepAgent},
			check: func(t *testing.T, s *script) {
				if s.steps[0].agent.resultVar != "" {
					t.Errorf("resultVar = %q, want empty", s.steps[0].agent.resultVar)
				}
			},
		},
		{
			name:  "export prefix is stripped position-preservingly",
			src:   "export const meta = { name: 'flow' }\nagent('go')",
			kinds: []stepKind{stepMeta, stepAgent},
			check: func(t *testing.T, s *script) {
				if s.steps[0].meta.name != "flow" {
					t.Errorf("meta.name = %q", s.steps[0].meta.name)
				}
				if s.steps[1].line != 2 {
					t.Errorf("agent line = %d, want 2", s.steps[1].line)
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := mustParse(t, tc.src)
			wantKinds(t, s.steps, tc.kinds...)
			if tc.check != nil {
				tc.check(t, s)
			}
		})
	}
}

func TestParseErrorPaths(t *testing.T) {
	if _, err := parseScript("bad.js", []byte("const x = {{{")); err == nil {
		t.Error("unparsable JS must error")
	} else if !strings.Contains(err.Error(), "javascript parse error") {
		t.Errorf("error = %v, want javascript parse error wrapper", err)
	}
}

func TestParseScriptStem(t *testing.T) {
	s, err := parseScript("dir/my-flow.js", []byte("agent('x')"))
	if err != nil {
		t.Fatal(err)
	}
	if s.stem != "my-flow" {
		t.Errorf("stem = %q, want my-flow", s.stem)
	}
}

// ---- bindings ----

func TestParseBindings(t *testing.T) {
	src := strings.Join([]string{
		"const target = 'src/'",
		"const tmpl = `hello`",
		"const n = 3",
		"const f = 1.5",
		"const bug = (args && args.bug) || ''",
		"const dyn = Date.now()",
		"let mut",
		"const interp = `hi ${x}`",
	}, "\n")
	s := mustParse(t, src)

	wantBindings := map[string]struct {
		kind bindingKind
		str  string
		num  int
	}{
		"target": {kind: bindStaticString, str: "src/"},
		"tmpl":   {kind: bindStaticString, str: "hello"},
		"n":      {kind: bindNumber, num: 3},
		"f":      {kind: bindLocal}, // QUIRK: non-integer const → local, no report
		"bug":    {kind: bindArgDerived},
		"dyn":    {kind: bindLocal},
		"mut":    {kind: bindLocal},
		"interp": {kind: bindLocal},
	}
	for name, want := range wantBindings {
		b := s.bindings[name]
		if b == nil {
			t.Errorf("binding %q missing", name)
			continue
		}
		if b.kind != want.kind {
			t.Errorf("binding %q kind = %v, want %v", name, b.kind, want.kind)
		}
		if b.str != want.str {
			t.Errorf("binding %q str = %q, want %q", name, b.str, want.str)
		}
		if b.num != want.num {
			t.Errorf("binding %q num = %d, want %d", name, b.num, want.num)
		}
	}

	// args-derived promotes to a hole var report entry.
	if !reportHas(s.report.Holes, "`bug` derived from args → var `bug`") {
		t.Errorf("holes = %+v", s.report.Holes)
	}
	// Unrecognized call binding is dropped visibly.
	if !reportHas(s.report.Dropped, "call binding `dyn = ") {
		t.Errorf("dropped = %+v", s.report.Dropped)
	}
	// None of these produce steps.
	wantKinds(t, s.steps)
}

func TestParseBindingDestructuring(t *testing.T) {
	s := mustParse(t, "const {a, b} = thing")
	wantKinds(t, s.steps, stepUnknown)
	if !reportHas(s.report.Placeholders, "destructuring binding kept as comment") {
		t.Errorf("placeholders = %+v", s.report.Placeholders)
	}
}

func TestParseBindingObjectLiteralNotSchema(t *testing.T) {
	s := mustParse(t, "const cfg = { retries: 3 }")
	wantKinds(t, s.steps)
	if b := s.bindings["cfg"]; b == nil || b.kind != bindLocal {
		t.Errorf("cfg binding = %+v, want bindLocal", s.bindings["cfg"])
	}
	if !reportHas(s.report.Dropped, "object literal `cfg` has no .bot equivalent") {
		t.Errorf("dropped = %+v", s.report.Dropped)
	}
}

func TestParseDynamicConstDropped(t *testing.T) {
	s := mustParse(t, "const x = a ? 'y' : 'z'")
	if b := s.bindings["x"]; b == nil || b.kind != bindLocal {
		t.Fatalf("x binding = %+v", s.bindings["x"])
	}
	if !reportHas(s.report.Dropped, "const `x` = <dynamic JS> dropped") {
		t.Errorf("dropped = %+v", s.report.Dropped)
	}
}

// ---- meta ----

func TestParseMeta(t *testing.T) {
	src := strings.Join([]string{
		"export const meta = {",
		"  name: 'My Flow',",
		"  description: 'Does things',",
		"  phases: [",
		"    {title: 'One', detail: 'first'},",
		"    {title: 'Two'},",
		"    {detail: 'no title — dropped'},",
		"  ],",
		"}",
	}, "\n")
	s := mustParse(t, src)
	wantKinds(t, s.steps, stepMeta)
	m := s.steps[0].meta
	if m.name != "My Flow" || m.description != "Does things" {
		t.Errorf("meta = %+v", m)
	}
	if len(m.phases) != 2 {
		t.Fatalf("phases = %+v, want 2 (title-less entry dropped)", m.phases)
	}
	if m.phases[0].title != "One" || m.phases[0].detail != "first" {
		t.Errorf("phase[0] = %+v", m.phases[0])
	}
	if m.phases[1].title != "Two" || m.phases[1].detail != "" {
		t.Errorf("phase[1] = %+v", m.phases[1])
	}
}

// ---- schemas ----

func TestParseSchemaObject(t *testing.T) {
	src := strings.Join([]string{
		"const outSchema = {",
		"  type: 'object',",
		"  required: ['a'],",
		"  additionalProperties: false,",
		"  properties: {",
		"    a: { type: 'string', description: 'field a', enum: ['x', 'y'] },",
		"    count: { type: 'integer', enum: [1, 2] },",
		"    flag: { type: 'boolean' },",
		"    ratio: { type: 'number' },",
		"    tags: { type: 'array', items: { type: 'string' } },",
		"    blobs: { type: 'array', items: { type: 'object' } },",
		"    misc: { type: 'weird' },",
		"    raw: 'notanobject',",
		"  },",
		"}",
	}, "\n")
	s := mustParse(t, src)
	wantKinds(t, s.steps, stepSchema)
	sc := s.steps[0].schema
	if sc.constName != "outSchema" {
		t.Errorf("constName = %q", sc.constName)
	}
	if b := s.bindings["outSchema"]; b == nil || b.kind != bindSchema {
		t.Errorf("binding = %+v, want bindSchema", s.bindings["outSchema"])
	}

	want := []schemaField{
		{name: "a", typ: "string", desc: "field a", enum: []string{"x", "y"}},
		{name: "count", typ: "int"}, // numeric enum values unrepresentable → nil
		{name: "flag", typ: "bool"},
		{name: "ratio", typ: "float"},
		{name: "tags", typ: "string[]"},
		{name: "blobs", typ: "json"}, // array of non-string items
		{name: "misc", typ: "json"},  // unknown type keyword
		{name: "raw", typ: "json"},   // property value not an object literal
	}
	if len(sc.fields) != len(want) {
		t.Fatalf("fields = %+v, want %d fields", sc.fields, len(want))
	}
	for i, w := range want {
		g := sc.fields[i]
		if g.name != w.name || g.typ != w.typ || g.desc != w.desc {
			t.Errorf("field[%d] = %+v, want %+v", i, g, w)
		}
		if fmt.Sprint(g.enum) != fmt.Sprint(w.enum) {
			t.Errorf("field[%d] enum = %v, want %v", i, g.enum, w.enum)
		}
	}

	// `type`/`required` are silently absorbed; other keywords drop visibly.
	if !reportHas(s.report.Dropped, "JSON-schema keyword `additionalProperties`") {
		t.Errorf("dropped = %+v", s.report.Dropped)
	}
	if reportHas(s.report.Dropped, "keyword `required`") {
		t.Error("required must be silently absorbed, not reported dropped")
	}
}

func TestLooksLikeJSONSchemaByTypeObjectOnly(t *testing.T) {
	// `type: 'object'` alone (no properties) is enough to classify.
	s := mustParse(t, "const empty = { type: 'object' }")
	wantKinds(t, s.steps, stepSchema)
	if len(s.steps[0].schema.fields) != 0 {
		t.Errorf("fields = %+v, want none", s.steps[0].schema.fields)
	}
}

// ---- agent calls ----

func TestParseAgentCallOptions(t *testing.T) {
	src := strings.Join([]string{
		"const outSchema = { type: 'object', properties: { ok: { type: 'boolean' } } }",
		"const r = await agent('do it', {",
		"  label: 'worker#1',",
		"  phase: 'build',",
		"  model: 'anthropic/claude-sonnet-4-6',",
		"  effort: 'high',",
		"  schema: outSchema,",
		"  temperature: 0.2,",
		"})",
	}, "\n")
	s := mustParse(t, src)
	wantKinds(t, s.steps, stepSchema, stepAgent)
	ag := s.steps[1].agent
	if ag.resultVar != "r" {
		t.Errorf("resultVar = %q", ag.resultVar)
	}
	if ag.label != "worker#1" {
		t.Errorf("label = %q (sanitization happens at lowering, not parse)", ag.label)
	}
	if ag.phase != "build" || ag.model != "anthropic/claude-sonnet-4-6" || ag.effort != "high" {
		t.Errorf("opts = %+v", ag)
	}
	if ag.schemaConst != "outSchema" || ag.inlineSchema != nil {
		t.Errorf("schema ref = %q / %+v", ag.schemaConst, ag.inlineSchema)
	}
	if !reportHas(s.report.Dropped, "agent option `temperature` has no .bot equivalent") {
		t.Errorf("dropped = %+v", s.report.Dropped)
	}
	if b := s.bindings["r"]; b == nil || b.kind != bindAgent {
		t.Errorf("r binding = %+v, want bindAgent", s.bindings["r"])
	}
	if len(ag.prompt) != 1 || ag.prompt[0].text != "do it" || ag.prompt[0].expr != nil {
		t.Errorf("prompt = %+v", ag.prompt)
	}
}

func TestParseAgentInlineSchema(t *testing.T) {
	s := mustParse(t, "await agent('check', { label: 'checker', schema: { type: 'object', properties: { ok: { type: 'boolean' } } } })")
	wantKinds(t, s.steps, stepAgent)
	ag := s.steps[0].agent
	if ag.inlineSchema == nil || len(ag.inlineSchema.fields) != 1 {
		t.Fatalf("inlineSchema = %+v", ag.inlineSchema)
	}
	if ag.inlineSchema.fields[0].name != "ok" || ag.inlineSchema.fields[0].typ != "bool" {
		t.Errorf("field = %+v", ag.inlineSchema.fields[0])
	}
}

func TestParseAgentTemplatedLabelPrefix(t *testing.T) {
	src := "const iter = 2\nawait agent('fix', { label: `fixer#${iter}` })"
	s := mustParse(t, src)
	wantKinds(t, s.steps, stepAgent)
	if got := s.steps[0].agent.label; got != "fixer" {
		t.Errorf("label = %q, want fixer (templated suffix trimmed)", got)
	}
}

func TestParseAgentDynamicLabelEmpty(t *testing.T) {
	s := mustParse(t, "await agent('fix', { label: labelVar })")
	wantKinds(t, s.steps, stepAgent)
	if got := s.steps[0].agent.label; got != "" {
		t.Errorf("label = %q, want empty for a fully dynamic label", got)
	}
}

// ---- prompt flattening ----

func TestPromptParts(t *testing.T) {
	src := strings.Join([]string{
		"const target = 'src/'",
		"const r1 = await agent('plain prompt', {label: 'a1'})",
		"const r2 = await agent(`fix ${target} now`, {label: 'a2'})",
		"const r3 = await agent('start ' + target + ' end', {label: 'a3'})",
		"const r4 = await agent(pickPrompt(), {label: 'a4'})",
	}, "\n")
	s := mustParse(t, src)
	wantKinds(t, s.steps, stepAgent, stepAgent, stepAgent, stepAgent)

	type partShape struct {
		text string
		hole bool
	}
	want := [][]partShape{
		{{text: "plain prompt"}},
		{{text: "fix "}, {hole: true}, {text: " now"}},
		{{text: "start "}, {hole: true}, {text: " end"}},
		{{hole: true}},
	}
	for i, shapes := range want {
		parts := s.steps[i].agent.prompt
		if len(parts) != len(shapes) {
			t.Errorf("agent %d: %d parts, want %d (%+v)", i+1, len(parts), len(shapes), parts)
			continue
		}
		for j, sh := range shapes {
			p := parts[j]
			if sh.hole && p.expr == nil {
				t.Errorf("agent %d part %d: want hole, got text %q", i+1, j, p.text)
			}
			if !sh.hole && (p.expr != nil || p.text != sh.text) {
				t.Errorf("agent %d part %d = %+v, want text %q", i+1, j, p, sh.text)
			}
		}
	}
}

// ---- fan-outs ----

func TestParsePromiseAllMap(t *testing.T) {
	src := "const results = await Promise.all(items.map(m => agent(`work ${m}`, {label: 'worker'})))"
	s := mustParse(t, src)
	wantKinds(t, s.steps, stepFanOut)
	fan := s.steps[0].fan
	if fan.mode != "fan_out_each" {
		t.Errorf("mode = %q", fan.mode)
	}
	if fan.param != "m" || fan.resultVar != "results" {
		t.Errorf("fan = %+v", fan)
	}
	if len(fan.agents) != 1 || fan.agents[0].label != "worker" {
		t.Errorf("agents = %+v", fan.agents)
	}
	if fan.overExpr == nil {
		t.Error("overExpr must carry the items expression")
	}
	if b := s.bindings["results"]; b == nil || b.kind != bindLocal {
		t.Errorf("results binding = %+v, want bindLocal", s.bindings["results"])
	}
}

func TestParsePromiseAllNonMapNotRecognized(t *testing.T) {
	s := mustParse(t, "await Promise.all(promises)")
	wantKinds(t, s.steps, stepUnknown)
	if !reportHas(s.report.Placeholders, "unmapped call kept as comment") {
		t.Errorf("placeholders = %+v", s.report.Placeholders)
	}
}

func TestParseParallelThunks(t *testing.T) {
	src := "await parallel([() => agent('a', {label: 'one'}), () => agent('b', {label: 'two'})])"
	s := mustParse(t, src)
	wantKinds(t, s.steps, stepFanOut)
	fan := s.steps[0].fan
	if fan.mode != "fan_out_all" {
		t.Errorf("mode = %q", fan.mode)
	}
	if len(fan.agents) != 2 || fan.agents[0].label != "one" || fan.agents[1].label != "two" {
		t.Errorf("agents = %+v", fan.agents)
	}
}

func TestParseParallelWithoutAgentsNotRecognized(t *testing.T) {
	s := mustParse(t, "await parallel([1, 2])")
	wantKinds(t, s.steps, stepUnknown)
}

func TestParsePipelineStages(t *testing.T) {
	src := "const out = await pipeline(items, (it) => agent(`s1 ${it}`, {label: 'st1'}), (it2) => agent(`s2 ${it2}`, {label: 'st2'}))"
	s := mustParse(t, src)
	wantKinds(t, s.steps, stepFanOut)
	fan := s.steps[0].fan
	if fan.mode != "fan_out_each" {
		t.Errorf("mode = %q", fan.mode)
	}
	// The FIRST stage's parameter name wins for the whole pipeline.
	if fan.param != "it" {
		t.Errorf("param = %q, want it", fan.param)
	}
	if len(fan.agents) != 2 {
		t.Fatalf("agents = %+v", fan.agents)
	}
	if fan.resultVar != "out" {
		t.Errorf("resultVar = %q", fan.resultVar)
	}
}

func TestParsePipelineTooFewArgsNotRecognized(t *testing.T) {
	s := mustParse(t, "const out = await pipeline(items)")
	wantKinds(t, s.steps)
	if !reportHas(s.report.Dropped, "call binding `out = ") {
		t.Errorf("dropped = %+v", s.report.Dropped)
	}
}

func TestStageAgentsBlockBody(t *testing.T) {
	// Arrow with a block body: agent() found in a declaration initializer
	// and in a return argument.
	src := "await parallel([(x) => { const r = agent('a', {label: 'blk'}); log('side'); return r }])"
	s := mustParse(t, src)
	wantKinds(t, s.steps, stepFanOut)
	fan := s.steps[0].fan
	if len(fan.agents) != 1 || fan.agents[0].label != "blk" {
		t.Errorf("agents = %+v", fan.agents)
	}
}

// ---- loops ----

func TestParseBoundedForLoop(t *testing.T) {
	src := strings.Join([]string{
		"for (let i = 1; i <= 4; i++) {",
		"  const check = await agent('check', {label: 'checker'})",
		"  if (check.verdict === 'clean') break",
		"}",
	}, "\n")
	s := mustParse(t, src)
	wantKinds(t, s.steps, stepLoop)
	li := s.steps[0].loop
	if !li.bounded || li.maxIter != 4 {
		t.Errorf("loop = bounded=%v maxIter=%d, want bounded 4", li.bounded, li.maxIter)
	}
	if li.fuel != 25 {
		t.Errorf("fuel = %d, want 25 (default)", li.fuel)
	}
	if li.breakCond == nil || li.breakJS != "check.verdict === 'clean'" {
		t.Errorf("breakJS = %q", li.breakJS)
	}
	wantKinds(t, li.body, stepAgent)
}

func TestParseForLoopBoundFromConst(t *testing.T) {
	src := "const N = 3\nfor (let i = 1; i <= N; i++) {\n  await agent('x', {label: 'w'})\n}"
	s := mustParse(t, src)
	wantKinds(t, s.steps, stepLoop)
	if li := s.steps[0].loop; !li.bounded || li.maxIter != 3 {
		t.Errorf("loop = %+v, want bounded via const N=3", li)
	}
}

func TestParseForLoopUnknownBoundUnbounded(t *testing.T) {
	src := "for (let i = 0; i < limit; i++) {\n  await agent('x', {label: 'w'})\n}"
	s := mustParse(t, src)
	wantKinds(t, s.steps, stepLoop)
	if li := s.steps[0].loop; li.bounded || li.maxIter != 0 {
		t.Errorf("loop = %+v, want unbounded (bound not statically known)", li)
	}
}

func TestParseForLoopDescendingUnbounded(t *testing.T) {
	src := "for (let i = 5; i > 0; i--) {\n  await agent('x', {label: 'w'})\n}"
	s := mustParse(t, src)
	wantKinds(t, s.steps, stepLoop)
	if s.steps[0].loop.bounded {
		t.Error("descending comparison must not be treated as a static bound")
	}
}

func TestParseWhileLoopWithBreak(t *testing.T) {
	src := "while (true) {\n  const r = await agent('poll', {label: 'poller'})\n  if (r.done) break\n}"
	s := mustParse(t, src)
	wantKinds(t, s.steps, stepLoop)
	li := s.steps[0].loop
	if li.bounded {
		t.Error("while loop must be unbounded")
	}
	if li.breakCond == nil || li.breakJS != "r.done" {
		t.Errorf("breakJS = %q", li.breakJS)
	}
}

func TestParseDoWhileLoop(t *testing.T) {
	src := "do {\n  await agent('x', {label: 'w'})\n} while (again)"
	s := mustParse(t, src)
	wantKinds(t, s.steps, stepLoop)
	if s.steps[0].loop.bounded {
		t.Error("do/while must be unbounded")
	}
}

func TestParseLoopUnconditionalBreakDropped(t *testing.T) {
	src := "while (true) {\n  await agent('x', {label: 'w'})\n  break\n}"
	s := mustParse(t, src)
	wantKinds(t, s.steps, stepLoop)
	if s.steps[0].loop.breakCond != nil {
		t.Error("unconditional break must not become a break condition")
	}
	if !reportHas(s.report.Dropped, "unconditional break dropped") {
		t.Errorf("dropped = %+v", s.report.Dropped)
	}
}

func TestParseLoopSecondBreakKeptAsComment(t *testing.T) {
	src := strings.Join([]string{
		"while (true) {",
		"  const r = await agent('work', {label: 'w'})",
		"  if (r.done) break",
		"  if (r.failed) break",
		"}",
	}, "\n")
	s := mustParse(t, src)
	wantKinds(t, s.steps, stepLoop)
	li := s.steps[0].loop
	if li.breakJS != "r.done" {
		t.Errorf("first break wins: breakJS = %q", li.breakJS)
	}
	wantKinds(t, li.body, stepAgent, stepUnknown)
	if !reportHas(s.report.Placeholders, "second break condition kept as comment") {
		t.Errorf("placeholders = %+v", s.report.Placeholders)
	}
}

func TestParseComputeLoopImportedOnce(t *testing.T) {
	src := "for (let i = 0; i < 3; i++) {\n  log('tick')\n}"
	s := mustParse(t, src)
	// No agent in the body → the body is imported once, not as a loop.
	wantKinds(t, s.steps, stepLog)
	if !reportHas(s.report.Dropped, "loop without agent() calls") {
		t.Errorf("dropped = %+v", s.report.Dropped)
	}
}

func TestParseLoopContinueDropped(t *testing.T) {
	src := "while (true) {\n  await agent('x', {label: 'w'})\n  continue\n}"
	s := mustParse(t, src)
	wantKinds(t, s.steps, stepLoop)
	if !reportHas(s.report.Dropped, "continue statement dropped") {
		t.Errorf("dropped = %+v", s.report.Dropped)
	}
}

// QUIRK (pinned): handleLoop only counts top-level stepAgent steps when
// deciding whether the loop is agentic. A loop whose only agents live
// inside a fan-out (stepFanOut) is treated as a compute loop: the body
// is imported once and the lifted break condition is discarded without
// its own report entry (the only note is the misleading "loop without
// agent() calls" drop).
func TestParseLoopWithOnlyFanOutLosesLoop(t *testing.T) {
	src := strings.Join([]string{
		"while (true) {",
		"  await parallel([() => agent('a', {label: 'pa'}), () => agent('b', {label: 'pb'})])",
		"  if (stop) break",
		"}",
	}, "\n")
	s := mustParse(t, src)
	wantKinds(t, s.steps, stepFanOut)
	if !reportHas(s.report.Dropped, "loop without agent() calls") {
		t.Errorf("dropped = %+v", s.report.Dropped)
	}
	// The break condition vanishes without any dedicated report entry.
	if reportHas(s.report.Placeholders, "stop") || reportHas(s.report.Dropped, "stop") {
		t.Error("pinned quirk changed: break condition now reported — update this test")
	}
}

// ---- line numbers ----

func TestStepLinesAreOriginalFileLines(t *testing.T) {
	src := "const meta = { name: 'x' }\n\nagent('hello')"
	s := mustParse(t, src)
	wantKinds(t, s.steps, stepMeta, stepAgent)
	if s.steps[0].line != 1 {
		t.Errorf("meta line = %d, want 1", s.steps[0].line)
	}
	if s.steps[1].line != 3 {
		t.Errorf("agent line = %d, want 3 (wrapper offset compensated)", s.steps[1].line)
	}
}

// ---- pure helpers ----

func TestStripExport(t *testing.T) {
	tests := []struct{ in, want string }{
		{"export const x = 1", "       const x = 1"},
		{"  export const x = 1", "         const x = 1"},
		{"const x = 1", "const x = 1"},
		{"reexport const x = 1", "reexport const x = 1"},
		{"export const a = 1\nexport const b = 2", "       const a = 1\n       const b = 2"},
		{"", ""},
	}
	for _, tc := range tests {
		if got := stripExport(tc.in); got != tc.want {
			t.Errorf("stripExport(%q) = %q, want %q", tc.in, got, tc.want)
		}
		// Position preservation: same byte length always.
		if len(stripExport(tc.in)) != len(tc.in) {
			t.Errorf("stripExport(%q) changed length", tc.in)
		}
	}
}

func TestSanitizeIdent(t *testing.T) {
	tests := []struct{ in, want string }{
		{"My Flow", "my_flow"},
		{"Fixer#2", "fixer_2"},
		{"worker-1", "worker_1"},
		{"a.b", "a_b"},
		{"CamelCase", "camelcase"},
		{"9lives", "n_9lives"},
		{"___", "unnamed"},
		{"", "unnamed"},
		{"É-à", "unnamed"}, // non-ASCII dropped, separators trimmed away
		{"already_ok", "already_ok"},
	}
	for _, tc := range tests {
		if got := sanitizeIdent(tc.in); got != tc.want {
			t.Errorf("sanitizeIdent(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFirstLine(t *testing.T) {
	if got := firstLine("one line"); got != "one line" {
		t.Errorf("firstLine = %q", got)
	}
	if got := firstLine("first\nsecond"); got != "first …" {
		t.Errorf("firstLine = %q", got)
	}
}

func TestCondOp(t *testing.T) {
	tests := []struct {
		tok     token.Token
		op, neg string
	}{
		{token.STRICT_EQUAL, "==", "!="},
		{token.EQUAL, "==", "!="},
		{token.STRICT_NOT_EQUAL, "!=", "=="},
		{token.NOT_EQUAL, "!=", "=="},
		{token.LESS, "<", ">="},
		{token.LESS_OR_EQUAL, "<=", ">"},
		{token.GREATER, ">", "<="},
		{token.GREATER_OR_EQUAL, ">=", "<"},
		{token.PLUS, "", ""},
	}
	for _, tc := range tests {
		op, neg := condOp(tc.tok)
		if op != tc.op || neg != tc.neg {
			t.Errorf("condOp(%v) = (%q, %q), want (%q, %q)", tc.tok, op, neg, tc.op, tc.neg)
		}
	}
}

func TestQuoteListAndOneLine(t *testing.T) {
	if got := quoteList([]string{"a", "b"}); got != `"a", "b"` {
		t.Errorf("quoteList = %s", got)
	}
	if got := quoteList(nil); got != "" {
		t.Errorf("quoteList(nil) = %q", got)
	}
	if got := oneLine("a\n  b\tc"); got != "a b c" {
		t.Errorf("oneLine = %q", got)
	}
}

// ---- report helpers ----

func TestFormatEntries(t *testing.T) {
	got := FormatEntries([]ReportEntry{
		{Line: 3, Text: "mapped agent"},
		{Line: 0, Text: "whole-file note"},
	})
	want := []string{"js:3 mapped agent", "whole-file note"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("FormatEntries = %v, want %v", got, want)
	}
}

func TestReportNeedsAttentionAndHeader(t *testing.T) {
	r := &Report{SourceFile: "x.js"}
	if r.NeedsAttention() {
		t.Error("empty report must not need attention")
	}
	if !strings.Contains(r.header(), "Clean import") {
		t.Errorf("clean header = %q", r.header())
	}
	r.dropped(2, "gone")
	if !r.NeedsAttention() {
		t.Error("dropped entry must flag attention")
	}
	h := r.header()
	if !strings.Contains(h, "## IMPORT REPORT") || !strings.Contains(h, "js:2 gone") {
		t.Errorf("header = %q", h)
	}
	if strings.Contains(h, "Clean import") {
		t.Error("non-clean report must not claim a clean import")
	}
	// Mapped-only report is not attention-worthy.
	r2 := &Report{SourceFile: "y.js"}
	r2.mapped(1, "agent() → agent a")
	if r2.NeedsAttention() {
		t.Error("mapped-only report must not need attention")
	}
}
