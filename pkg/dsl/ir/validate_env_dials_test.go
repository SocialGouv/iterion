package ir

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// C315 — ADR-132 stage 1: a run reads no environment
// ---------------------------------------------------------------------------

// envDialDiags returns every C315 finding of a compile, in the order the pass
// emitted them.
func envDialDiags(r *CompileResult) []*Diagnostic {
	var out []*Diagnostic
	for i := range r.Diagnostics {
		if r.Diagnostics[i].Code == DiagEnvDialInRunSetting {
			out = append(out, &r.Diagnostics[i])
		}
	}
	return out
}

// envDialNames is the set of env names a compile's C315 findings name, so a
// case asserts on WHICH dial was found rather than on how many.
func envDialNames(r *CompileResult) []string {
	var names []string
	for _, d := range envDialDiags(r) {
		for _, m := range regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`).FindAllStringSubmatch(d.Message, -1) {
			names = append(names, m[1])
		}
	}
	sort.Strings(names)
	return names
}

// TestEnvDialIsNamedWhateverItsPrefix is the decision the guard exists to
// hold: the dial class is ANY env name, not the `ITERION_` family. Measured
// when this was written: 22 of the names the shipped catalogue dials in a
// routing field are not `ITERION_` (nine `VETTY_`, five `BMADY_`, four
// `FEED_`, two `REARCHI_`, `RESCUE_PROVIDER`, `DEVBOX_SETUP_MODEL`), on 46
// authored lines — including dep-update-guard's human-node escalation model,
// the one case a prefix guard would have left undecided at the launch gate
// forever.
//
// Mutation that reddens it: narrow the predicate to the `ITERION_` family —
// in bracedEnvNames, `if !strings.HasPrefix(r.Name, "ITERION_") { continue }`
// — and every row but the ITERION one goes silent while the guard still
// reports something, which is the failure a count-only assertion would miss.
func TestEnvDialIsNamedWhateverItsPrefix(t *testing.T) {
	for name, tc := range map[string]struct {
		src      string
		wantName string
		wantNode string
		wantSaid string // a substring of the DSL field the message must name
	}{
		"an agent's model, the main family": {
			src:      "prompt p:\n  hi\n\nagent emit:\n  model: \"${ITERION_VIBE_MODEL_EMIT:-claude-opus-5-5}\"\n  user: p\n\nworkflow w:\n  entry: emit\n  emit -> done\n",
			wantName: "ITERION_VIBE_MODEL_EMIT",
			wantNode: "emit",
			wantSaid: "model:",
		},
		"a human node's companion model, another family": {
			src:      "schema s:\n  ok: bool\n\nprompt p:\n  hi\n\nhuman escalate:\n  instructions: p\n  interaction: llm\n  model: \"${VETTY_MODEL_ESCALATE:-claude-opus-5-5}\"\n  output: s\n\nworkflow w:\n  entry: escalate\n  escalate -> done\n",
			wantName: "VETTY_MODEL_ESCALATE",
			wantNode: "escalate",
			wantSaid: "model:",
		},
		"a judge's backend": {
			src:      "schema s:\n  ok: bool\n\nprompt p:\n  hi\n\njudge gate:\n  backend: \"${BMADY_BACKEND_GATE:-claw}\"\n  model: \"m\"\n  user: p\n  output: s\n\nworkflow w:\n  entry: gate\n  gate -> done\n",
			wantName: "BMADY_BACKEND_GATE",
			wantNode: "gate",
			wantSaid: "backend:",
		},
		"a rescue route's provider hint": {
			src:      "prompt p:\n  hi\n\nagent emit:\n  model: \"m\"\n  user: p\n  fallbacks:\n    rescue:\n      model: \"glm-5.3\"\n      provider: \"${RESCUE_PROVIDER:-zai}\"\n\nworkflow w:\n  entry: emit\n  emit -> done\n",
			wantName: "RESCUE_PROVIDER",
			wantNode: "emit",
			wantSaid: "provider:",
		},
		"an llm router's own model": {
			src:      "prompt p:\n  hi\n\nagent a:\n  model: \"m\"\n  user: p\n\nagent b:\n  model: \"m\"\n  user: p\n\nrouter pick:\n  mode: llm\n  model: \"${REARCHI_MODEL_PICK:-claude-opus-5-5}\"\n  user: p\n\nworkflow w:\n  entry: pick\n  pick -> a\n  pick -> b\n  a -> done\n  b -> done\n",
			wantName: "REARCHI_MODEL_PICK",
			wantNode: "pick",
			wantSaid: "model:",
		},
		"the workflow's own default backend": {
			src:      "prompt p:\n  hi\n\nagent emit:\n  model: \"m\"\n  user: p\n\nworkflow w:\n  default_backend: \"${DEVBOX_SETUP_BACKEND:-claw}\"\n  entry: emit\n  emit -> done\n",
			wantName: "DEVBOX_SETUP_BACKEND",
			wantNode: "",
			wantSaid: "default_backend:",
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := compileText(t, tc.src)
			got := envDialDiags(r)
			if len(got) != 1 {
				t.Fatalf("want exactly one C315 finding, got %d: %v", len(got), envDialNames(r))
			}
			if got[0].Severity != SeverityWarning {
				t.Errorf("C315 is severity %v, want warning — stage 1 names dials, it does not refuse them", got[0].Severity)
			}
			if got[0].NodeID != tc.wantNode {
				t.Errorf("C315 attributed to node %q, want %q", got[0].NodeID, tc.wantNode)
			}
			if !strings.Contains(got[0].Message, "${"+tc.wantName+"}") {
				t.Errorf("message %q does not name the dial ${%s}", got[0].Message, tc.wantName)
			}
			if !strings.Contains(got[0].Message, tc.wantSaid) {
				t.Errorf("message %q does not name the field %q the author has to edit", got[0].Message, tc.wantSaid)
			}
		})
	}
}

// TestEnvDialSilentWhereThereIsNoEnvRead: the reader is the engine's own
// segmentation, and the pass is silent on every form the expansion does not
// answer — a literal route, a `{{vars.x}}` reference (decided by the launch,
// not by the environment), and the parameter-expansion forms
// looksLikeEnvRef refuses.
//
// Mutation that reddens it: replace bracedEnvNames' BracedEnvReads walk with
// a `strings.Contains(value, "$")` test and report the whole field — the
// literal row stays silent but the `{{vars.x}}` and `${X#y}` rows fire, and
// a guard that fires on a value the engine never reads from the environment
// sends the author to edit a field that was already correct.
func TestEnvDialSilentWhereThereIsNoEnvRead(t *testing.T) {
	for name, src := range map[string]string{
		"a literal route":                "prompt p:\n  hi\n\nagent emit:\n  model: \"claude-opus-5-5\"\n  backend: \"claw\"\n  provider: \"anthropic\"\n  user: p\n\nworkflow w:\n  entry: emit\n  emit -> done\n",
		"a launch-decided var route":     "prompt p:\n  hi\n\nvars:\n  model: string = \"claude-opus-5-5\"\n\nagent emit:\n  model: \"{{vars.model}}\"\n  user: p\n\nworkflow w:\n  entry: emit\n  emit -> done\n",
		"a parameter-expansion form":     "prompt p:\n  hi\n\nagent emit:\n  model: \"${MODEL#native:}\"\n  user: p\n\nworkflow w:\n  entry: emit\n  emit -> done\n",
		"a dotted template literal":      "prompt p:\n  hi\n\nagent emit:\n  model: \"${p.name}\"\n  user: p\n\nworkflow w:\n  entry: emit\n  emit -> done\n",
		"a body declining the expansion": "tool gate:\n  env_expansion: none\n  command: |\n    echo \"${ITERION_DISPATCHER_PORT}\"\n\nworkflow w:\n  entry: gate\n  gate -> done\n",
	} {
		t.Run(name, func(t *testing.T) {
			r := compileText(t, src)
			if got := envDialNames(r); len(got) != 0 {
				t.Errorf("C315 fired on a value the engine reads no environment for: %v", got)
			}
		})
	}
}

// TestEngineSuppliedNamesAreCarvedOutOfVarDefaultsOnly: the carve-out is a
// property of the SURFACE, not of the name, and the two surfaces have
// different readers.
//
//   - In a var default, varExpandFn answers the engine-supplied names from
//     run state (engine_resolve.go's engineSuppliedVarFns), so
//     `${PROJECT_DIR}` there is §1's engine WRITE, not a run read. Naming it
//     would fire on every bot that locates its own workspace — 108 braced
//     reads of `${PROJECT_DIR}` / `${PROJECT_SCRATCH_DIR}` in the shipped
//     catalogue when this was written, 40 of them `workspace_dir:` var
//     declarations — with no successor to offer.
//   - In a tool body the reader is lookupToolEnv: the `ITERION_` overlay,
//     then os.LookupEnv (pkg/backend/model/executor_tool.go). Run state is
//     never consulted, so the SAME name read there is a read of the engine
//     process's environment and is named. Carving the list out of the body
//     walk too would have silenced the defect class this record exists to
//     find, which is what the first draft of this guard did.
//
// Mutation that reddens it: add the isEngineSuppliedName carve-out back to
// toolBodyEnvSites — the body row for `${PROJECT_SCRATCH_DIR}` goes silent;
// or drop it from varDefaultEnvSites — the var row fires. The two are
// asserted apart because one mutation must not be covered by the other's
// assertion.
func TestEngineSuppliedNamesAreCarvedOutOfVarDefaultsOnly(t *testing.T) {
	for name, tc := range map[string]struct {
		src      string
		want     []string
		where    string
		wantSaid string // what the message must name, so the author knows where to edit
	}{
		"a body reading the engine's own scratch dir, which no body reader answers": {
			src:      "tool gate:\n  command: |\n    ls \"${PROJECT_SCRATCH_DIR}\"\n\nworkflow w:\n  entry: gate\n  gate -> done\n",
			want:     []string{"PROJECT_SCRATCH_DIR"},
			where:    "tool body",
			wantSaid: "command body",
		},
		"a body reading a dial": {
			src:      "tool gate:\n  command: |\n    curl \"localhost:${ITERION_DISPATCHER_PORT}\"\n\nworkflow w:\n  entry: gate\n  gate -> done\n",
			want:     []string{"ITERION_DISPATCHER_PORT"},
			where:    "tool body",
			wantSaid: "command body",
		},
		"a var default seeded by the engine": {
			src:   "vars:\n  workspace: string = \"${PROJECT_DIR}\"\n\ntool gate:\n  command: \"true\"\n\nworkflow w:\n  entry: gate\n  gate -> done\n",
			want:  nil,
			where: "var default",
		},
		"a var default reading a knob": {
			src:      "vars:\n  max_duration: string = \"${ITERION_RGAA_MAX_DURATION:-45m}\"\n\ntool gate:\n  command: \"true\"\n\nworkflow w:\n  entry: gate\n  gate -> done\n",
			want:     []string{"ITERION_RGAA_MAX_DURATION"},
			where:    "var default",
			wantSaid: `var "max_duration"`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := compileText(t, tc.src)
			got := envDialNames(r)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("C315 over a %s named %v, want %v", tc.where, got, tc.want)
			}
			if tc.wantSaid != "" {
				msgs := envDialDiags(r)
				if len(msgs) != 1 || !strings.Contains(msgs[0].Message, tc.wantSaid) {
					t.Errorf("the finding does not name %q, the line the author has to edit: %v", tc.wantSaid, msgs)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The class guard
// ---------------------------------------------------------------------------

// routeFieldNames are the field names that carry a route: the value decides
// which model, which backend, which credential, which binary and which bound
// a run uses. A string field of a node spelled one of these is a dial site.
var routeFieldNames = map[string]bool{
	"Model":            true,
	"Backend":          true,
	"Provider":         true,
	"Command":          true,
	"ReasoningEffort":  true,
	"Timeout":          true,
	"InteractionModel": true,
}

// nodeTypesForTheClassWalk is one zero value per node type the package
// defines. TestTheNodeTypeRegistryIsComplete pins it against the package's
// own source, so a new node type cannot be added without appearing here.
func nodeTypesForTheClassWalk() map[string]Node {
	return map[string]Node{
		"AgentNode":        &AgentNode{},
		"AwaitAnswersNode": &AwaitAnswersNode{},
		"ComputeNode":      &ComputeNode{},
		"DoneNode":         &DoneNode{},
		"EmitNode":         &EmitNode{},
		"FailNode":         &FailNode{},
		"HumanNode":        &HumanNode{},
		"JudgeNode":        &JudgeNode{},
		"RouterNode":       &RouterNode{},
		"SubbotNode":       &SubbotNode{},
		"ToolNode":         &ToolNode{},
		"WaitNode":         &WaitNode{},
	}
}

// routeFieldsJudgedElsewhere are the route-shaped fields routeSites does NOT
// enumerate, each with the walk that judges it instead. A field may sit here
// only with a reason a reader can check.
var routeFieldsJudgedElsewhere = map[string]string{
	// A tool node's Command is a BODY, not a route: the engine expands it and
	// a shell runs it. toolBodyEnvSites judges it, asserted by
	// TestEngineSuppliedNamesAreNotDials' "a body reading a dial" row.
	"ToolNode.Command": "the tool body walk (toolBodyEnvSites)",
}

// TestTheNodeTypeRegistryIsComplete scans the package's own source for
// NodeKind implementations, the way diag_codes_test.go scans it for codes —
// Go types are not enumerable at runtime, so the registry above is a literal,
// and a literal rots unless something compares it with the tree.
func TestTheNodeTypeRegistryIsComplete(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob the package sources: %v", err)
	}
	declared := map[string]bool{}
	re := regexp.MustCompile(`func \(n \*([A-Za-z0-9_]+Node)\) NodeKind\(\)`)
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, m := range re.FindAllStringSubmatch(string(data), -1) {
			declared[m[1]] = true
		}
	}
	if len(declared) == 0 {
		t.Fatal("found no NodeKind implementation in the package sources — the scanner regex is stale and this guard is now guarding nothing")
	}
	registry := nodeTypesForTheClassWalk()
	for name := range declared {
		if _, ok := registry[name]; !ok {
			t.Errorf("%s implements Node and is not in nodeTypesForTheClassWalk — add it, so the route-field walk below judges its fields too", name)
		}
	}
	for name := range registry {
		if !declared[name] {
			t.Errorf("nodeTypesForTheClassWalk lists %s, which no longer implements Node — the registry is describing a tree that moved", name)
		}
	}
}

// TestEveryRouteFieldIsEnumerated is the Nth-variant guard of the dial class.
// For every node type, every route-shaped string field — including the ones
// reached through an embedded group (LLMFields, InteractionFields) or a
// pointer (a verified action's RecoverySpec) — is given a unique sentinel,
// and routeSites must reach it. A new route field, or a new node type
// carrying one, reddens here instead of shipping a dial the guard never
// looks at.
//
// Mutation that reddens it: delete any line of routeSites' add() calls (the
// human node's model, a fallback's provider, the recovery model) — the
// sentinel for that field is never reached and the failure names it.
func TestEveryRouteFieldIsEnumerated(t *testing.T) {
	var reached, missing []string
	for typeName, node := range nodeTypesForTheClassWalk() {
		want := map[string]string{} // sentinel → TypeName.FieldPath
		seedRouteFields(t, reflect.ValueOf(node).Elem(), typeName, typeName, want)
		if len(want) == 0 {
			continue
		}
		// A fallback route only exists once the node declares one, and the
		// walk must see its fields: give the two carriers one authored route.
		switch n := node.(type) {
		case *AgentNode:
			n.Fallbacks = []Fallback{{Name: "rescue"}}
		case *JudgeNode:
			n.Fallbacks = []Fallback{{Name: "rescue"}}
		}
		for sentinel, path := range seedFallbackFields(node, typeName) {
			want[sentinel] = path
		}

		w := &Workflow{Nodes: map[string]Node{"n": node}}
		seen := map[string]bool{}
		for _, s := range routeSites(w) {
			seen[s.value] = true
		}
		for sentinel, path := range want {
			switch {
			case seen[sentinel]:
				reached = append(reached, path)
			case routeFieldsJudgedElsewhere[path] != "":
				reached = append(reached, path+" (via "+routeFieldsJudgedElsewhere[path]+")")
			default:
				missing = append(missing, path)
			}
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("routeSites does not reach these route fields:\n  %s\nEvery one of them selects where a run goes, so a `${NAME}` written there is a dial no guard sees: enumerate the field in routeSites, or record it in routeFieldsJudgedElsewhere with the walk that judges it.", strings.Join(missing, "\n  "))
	}
	// A walk that stopped reading reaches nothing and reports nothing: the
	// count is the floor that tells the two apart.
	if len(reached) < 14 {
		t.Fatalf("the class walk reached only %d route fields (%v) — a reach this thin is a reflection walk that stopped descending, not a tree with nothing to judge", len(reached), reached)
	}
}

// seedRouteFields assigns a unique sentinel to every route-shaped string
// field of v, descending through embedded structs and through pointer fields
// it allocates (a RecoverySpec is nil until a node declares one). It records
// sentinel → dotted path for the caller to assert on.
func seedRouteFields(t *testing.T, v reflect.Value, typeName, path string, want map[string]string) {
	t.Helper()
	vt := v.Type()
	for i := 0; i < vt.NumField(); i++ {
		f, fv := vt.Field(i), v.Field(i)
		if !f.IsExported() {
			continue
		}
		switch {
		case f.Type.Kind() == reflect.String && routeFieldNames[f.Name]:
			sentinel := fmt.Sprintf("${DIAL_%s_%s}", strings.ToUpper(typeName), strings.ToUpper(f.Name))
			fv.SetString(sentinel)
			want[sentinel] = typeName + "." + f.Name
		case f.Anonymous && f.Type.Kind() == reflect.Struct:
			seedRouteFields(t, fv, typeName, path+"."+f.Name, want)
		case f.Type.Kind() == reflect.Pointer && f.Type.Elem().Kind() == reflect.Struct && f.Type.Elem().Name() != "":
			// A nil pointer group carries no field until a node declares it;
			// allocating it is how the walk judges what the author may write.
			if fv.IsNil() {
				fv.Set(reflect.New(f.Type.Elem()))
			}
			seedRouteFields(t, fv.Elem(), typeName, path+"."+f.Name, want)
		}
	}
}

// seedFallbackFields seeds the route fields of a node's authored fallback and
// returns sentinel → dotted path.
func seedFallbackFields(node Node, typeName string) map[string]string {
	out := map[string]string{}
	for i := range nodeFallbacks(node) {
		var fb *Fallback
		switch n := node.(type) {
		case *AgentNode:
			fb = &n.Fallbacks[i]
		case *JudgeNode:
			fb = &n.Fallbacks[i]
		}
		fb.Backend = "${DIAL_" + strings.ToUpper(typeName) + "_FALLBACK_BACKEND}"
		fb.Model = "${DIAL_" + strings.ToUpper(typeName) + "_FALLBACK_MODEL}"
		fb.Provider = "${DIAL_" + strings.ToUpper(typeName) + "_FALLBACK_PROVIDER}"
		out[fb.Backend] = typeName + ".Fallback.Backend"
		out[fb.Model] = typeName + ".Fallback.Model"
		out[fb.Provider] = typeName + ".Fallback.Provider"
	}
	return out
}
