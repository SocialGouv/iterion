package ir

import (
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// Every exported field of a compiled workflow is named where two programs
// differ by it: a reader of the difference — a refused save, a refused
// conversion, E056 on the .bot beside an author document — is told which
// construct to go and read, never only what the difference is not. A field
// the IR gains later is named by the walk, with no list to keep.
// theCatchAll is the sentence the walk ends on, which names no construct:
// no field of a workflow may be left to it, and no caller can reach it —
// every field is exported, so the walk names one whenever the two workflows
// are not DeepEqual.
const theCatchAll = "the workflows differ"

func TestFirstWorkflowDifferenceNamesEveryFieldItDiffersBy(t *testing.T) {
	base := &Workflow{Name: "w", Entry: "a", Nodes: map[string]Node{"a": &AgentNode{BaseNode: BaseNode{ID: "a"}}}}
	// No field of a compiled workflow is unexported, so the walk's reflect
	// pass reaches every one of them and the catch-all below is dead code
	// kept only as a total function's last line.
	for i := 0; i < reflect.TypeOf(*base).NumField(); i++ {
		if f := reflect.TypeOf(*base).Field(i); !f.IsExported() {
			t.Fatalf("%s is unexported: the walk cannot name it, and the catch-all would be reached", f.Name)
		}
	}
	// And it answers "" — never a difference — where there is no pair of
	// workflows to compare.
	for _, tc := range []struct {
		name string
		a, b *Workflow
	}{
		{"two workflows that are one", base, base},
		{"no left side", nil, base},
		{"no right side", base, nil},
	} {
		if why := FirstWorkflowDifference(tc.a, tc.b); why != "" {
			t.Errorf("%s: named %q, want no difference", tc.name, why)
		}
	}
	rt := reflect.TypeOf(*base)
	exported := 0
	saying := map[string]string{} // a field → the sentence it is given
	says := map[string][]string{} // a sentence → the fields it is given for
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if !f.IsExported() {
			continue
		}
		exported++
		other := *base
		if !setDifferently(reflect.ValueOf(&other).Elem().Field(i)) {
			t.Fatalf("%s (%s): the test has no differing value for this kind", f.Name, f.Type)
		}
		if reflect.DeepEqual(base, &other) {
			t.Fatalf("%s: the value set is the one already there", f.Name)
		}
		why := FirstWorkflowDifference(base, &other)
		if why == "" {
			t.Errorf("%s: two workflows that differ by it are named no difference", f.Name)
			continue
		}
		if why == theCatchAll {
			t.Errorf("%s: the difference is %q — the walk's catch-all, which names no construct", f.Name, why)
			continue
		}
		saying[f.Name] = why
		says[why] = append(says[why], f.Name)
	}
	// A sentence is ONE field's. A blanket one — the same words for every
	// field it does not name — is no name at all, and reading the field's
	// own word out of it cannot see that: a field whose word the blanket
	// sentence happens to carry would pass ("Name" is in "a field this
	// comparison does not name"). Every field of a shared sentence is named
	// here, never the second one the walk happened to reach.
	shared := map[string]bool{}
	for _, why := range slices.Sorted(maps.Keys(says)) {
		fields := says[why]
		if len(fields) < 2 {
			continue
		}
		t.Errorf("%d fields are given one sentence, %q, which therefore names none of them: %s", len(fields), why, strings.Join(fields, ", "))
		for _, name := range fields {
			shared[name] = true
		}
	}
	// And named means the construct is IN the sentence: a sentence that
	// names what the difference is not would otherwise count as naming
	// every field.
	named := 0
	for _, name := range slices.Sorted(maps.Keys(saying)) {
		why := saying[name]
		if !strings.Contains(strings.ToLower(why), strings.ToLower(strings.TrimSuffix(name, "s"))) {
			t.Errorf("%s: the difference is %q — the construct to go and read is not named in it", name, why)
			continue
		}
		if shared[name] {
			continue
		}
		named++
	}
	if named != exported || exported == 0 {
		t.Fatalf("%d of the workflow's %d exported fields named", named, exported)
	}
	// Two edge slices of the SAME length differing at one index — the shape
	// a reordered declaration reaches, a group expansion's internal edges
	// among them: the edge is named, where the walk's own case names the
	// field. The length branch is what the sweep above exercises.
	ab, bc := &Edge{From: "a", To: "b"}, &Edge{From: "b", To: "c"}
	twoEdges := &Workflow{Name: "w", Entry: "a", Nodes: base.Nodes, Edges: []*Edge{ab, bc}}
	swapped := &Workflow{Name: "w", Entry: "a", Nodes: base.Nodes, Edges: []*Edge{bc, ab}}
	if why := FirstWorkflowDifference(twoEdges, swapped); why != "edge a -> b differs" {
		t.Fatalf("two edge slices differing at one index are named %q, want the edge itself", why)
	}
	// The field's own name where no case has better words for it.
	deny := *base
	deny.Permission = "deny"
	if why := FirstWorkflowDifference(base, &deny); why != "the workflows differ in Permission" {
		t.Fatalf("a permission difference is named %q", why)
	}
}

// setDifferently gives v a value no zero value equals, whatever its kind:
// enough for DeepEqual to part the two workflows, nothing more.
func setDifferently(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.String:
		v.SetString("another")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(1)
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 1, 1))
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		m.SetMapIndex(reflect.New(v.Type().Key()).Elem(), reflect.New(v.Type().Elem()).Elem())
		v.Set(m)
	default:
		return false
	}
	return true
}

// FirstWorkflowDifference names the node a difference is about by sorted
// id, never by a map's iteration: with several nodes missing, or several
// appeared, the same one is named on every call.
func TestFirstWorkflowDifferenceNamesTheSameNodeOnEveryRun(t *testing.T) {
	nodes := func(ids ...string) map[string]Node {
		m := map[string]Node{}
		for _, id := range ids {
			m[id] = &AgentNode{BaseNode: BaseNode{ID: id}}
		}
		return m
	}
	many := &Workflow{Nodes: nodes("zeta", "mid", "alpha", "omega")}
	none := &Workflow{Nodes: nodes()}
	for i := 0; i < 30; i++ {
		if got := FirstWorkflowDifference(many, none); got != `node "alpha" is missing` {
			t.Fatalf("call %d: %s, want the first missing node by id", i, got)
		}
		if got := FirstWorkflowDifference(none, many); got != `node "alpha" appeared` {
			t.Fatalf("call %d: %s, want the first appeared node by id", i, got)
		}
	}
}

// Where both sides compiled a workflow, SameProgram names what those
// workflows differ by — never the sorted diagnostic codes, which name no
// construct a reader could go and open. Every caller shows that answer to
// someone: a refused save in the studio, a refused conversion of an author
// document, E056 on the .bot beside one. The codes stay the answer where
// one side compiled no workflow, and where two equal workflows still carry
// a difference the compiler saw.
func TestSameProgramNamesTheWorkflowsDifferenceNotTheCodes(t *testing.T) {
	withDiag := func(wf *Workflow, codes ...DiagCode) *CompileResult {
		cr := &CompileResult{Workflow: wf}
		for _, c := range codes {
			cr.Diagnostics = append(cr.Diagnostics, Diagnostic{Code: c, Severity: SeverityError})
		}
		return cr
	}
	node := func(id string) map[string]Node {
		return map[string]Node{id: &AgentNode{BaseNode: BaseNode{ID: id}}}
	}
	one, other := &Workflow{Name: "w", Nodes: node("a")}, &Workflow{Name: "w", Nodes: node("b")}

	// The shape E056 measured it on: the same code on each side, in another
	// number, while the two workflows differ by a node.
	if why := SameProgram(withDiag(one, "C003", "C003"), withDiag(other, "C003")); why != `node "a" is missing` {
		t.Errorf("two differing workflows are named %q, want the node", why)
	}
	// Two workflows that are one, and a diagnostic only one side carries:
	// the codes are the only thing that can see it.
	if why := SameProgram(withDiag(one, "C003"), withDiag(one)); why != "diagnostics differ: [C003] vs []" {
		t.Errorf("a difference no workflow holds is named %q, want the codes", why)
	}
	// One side compiled no workflow at all.
	if why := SameProgram(withDiag(one), withDiag(nil)); why != "one side compiled to a workflow and the other did not" {
		t.Errorf("a side that compiled nothing is named %q", why)
	}
	if why := SameProgram(withDiag(nil, "C006"), withDiag(nil)); why != "diagnostics differ: [C006] vs []" {
		t.Errorf("two sides that compiled nothing are named %q, want the codes", why)
	}
	// And one program is one program, whichever way it is asked.
	for _, tc := range []struct{ a, b *CompileResult }{
		{withDiag(one, "C003"), withDiag(one, "C003")},
		{withDiag(nil), withDiag(nil)},
	} {
		if why := SameProgram(tc.a, tc.b); why != "" {
			t.Errorf("one program is named a difference: %q", why)
		}
	}
}
