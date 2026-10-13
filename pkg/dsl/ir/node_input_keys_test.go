package ir

import (
	"slices"
	"strings"
	"testing"
)

// NodeInputKeys answers "can this node's input hold x?" — the question every
// text naming `input.x` depends on, and the one a schema alone cannot answer:
// the runtime builds the input from the edges' `with` mappings and the entry
// floor, never from the schema (pkg/runtime/engine_resolve.go).
const nodeInputKeysSrc = `
vars:
  brief: string = "from the launch"
  steer: string

schema s:
  ok: bool

schema second_in:
  declared: string

prompt pu:
  Brief: {{vars.brief}}

agent first:
  model: "m"
  user: pu
  output: s

agent second:
  model: "m"
  user: pu
  input: second_in
  output: s

workflow test:
  entry: first
  first -> second with { mapped: "{{outputs.first.ok}}" }
  second -> done
`

func TestNodeInputKeysUnionOfSchemaAndEdges(t *testing.T) {
	w := mustCompile(t, nodeInputKeysSrc)

	got := NodeInputKeys(w, "second")
	// The schema's own field, and a key only an edge maps: the runtime writes
	// a mapped key into the input whether the schema declares it or not.
	for _, want := range []string{"declared", "mapped"} {
		if !got[want] {
			if want == "mapped" {
				t.Errorf("NodeInputKeys(second) misses %q, which `first -> second with` maps: a text naming input.%s would be judged dead though the runtime delivers it", want, want)
				continue
			}
			t.Errorf("NodeInputKeys(second) misses %q, which its input schema declares", want)
		}
	}
	if got["brief"] {
		t.Error("NodeInputKeys(second) holds \"brief\": a var is the ENTRY node's floor, not every node's — a non-entry node sees a var only through an edge that maps it")
	}
	if got["absent"] {
		t.Error("NodeInputKeys(second) holds \"absent\", which nothing declares or maps")
	}
}

// The entry node's input is the run's: the launch payload (whose keys name a
// var) over the defaults the run seeds. A read of a DEFAULTED var is answered
// there, with no edge in sight — which is why the mapping guards exempt it. A
// var with no default counts too, and only a launch value answers it: the set
// is what the input CAN hold, an upper bound on this node
// (pkg/runtime/engine_resolve.go gates the floor on HasDefault).
func TestNodeInputKeysEntryNodeCarriesTheVarFloor(t *testing.T) {
	w := mustCompile(t, nodeInputKeysSrc)

	got := NodeInputKeys(w, "first")
	if !got["brief"] {
		t.Error("NodeInputKeys(entry) misses the defaulted var \"brief\": the entry floor seeds it, so {{input.brief}} on the entry node is answered by the run")
	}
	if !got["steer"] {
		t.Error("NodeInputKeys(entry) misses the undefaulted var \"steer\": a launch may supply any declared var, so the key is one the input can hold — the set is an upper bound here, not the floor itself")
	}
}

// A node id the workflow does not carry, and a nil workflow: the callers are
// guards walking a catalogue, so an unknown name is an empty set, never a
// panic that reads as a bundle defect.
func TestNodeInputKeysUnknownNodeIsEmpty(t *testing.T) {
	w := mustCompile(t, nodeInputKeysSrc)

	if got := NodeInputKeys(w, "nowhere"); len(got) != 0 {
		keys := make([]string, 0, len(got))
		for k := range got {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		t.Errorf("NodeInputKeys(unknown node) = %s, want nothing", strings.Join(keys, ","))
	}
	if got := NodeInputKeys(nil, "first"); len(got) != 0 {
		t.Errorf("NodeInputKeys(nil workflow) returned %d key(s), want nothing", len(got))
	}
}
