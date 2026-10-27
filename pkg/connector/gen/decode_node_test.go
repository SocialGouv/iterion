package gen

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// The decode walks a yaml.Node; these pin the three contracts that walk
// carries: scalar keys render through their literal form, aliases and merge
// keys expand like a plain Unmarshal, and a hostile document is refused with
// an error instead of taking the process down.

func TestNodeToAnyResolvesHexScalar(t *testing.T) {
	// yaml.v3 resolves 0x1A as the int 26; the walk must hand back the same
	// number the JSON twin of the same document carries.
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte("answer: 0x1A\n"), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	budget := maxParseNodes
	out, err := nodeToAny(&doc, &budget)
	if err != nil {
		t.Fatalf("nodeToAny: %v", err)
	}
	m, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("nodeToAny = %T, want map[string]any", out)
	}
	if m["answer"] != int(26) {
		t.Fatalf("answer = %v (%T), want int 26", m["answer"], m["answer"])
	}
}

func TestDecodeRejectsAliasCycleWithError(t *testing.T) {
	const y = `
openapi: 3.0.0
info: &loop
  title: x
  self: *loop
paths: {}
`
	// The pre-fix decoder recursed without a budget and died with an
	// unrecoverable fatal stack overflow on this document. The contract:
	// an error the caller can report.
	_, _, err := Generate([]byte(y), Options{ConnectorID: "probe"})
	if err == nil {
		t.Fatal("an alias cycle must be refused with an error, got nil")
	}
}

func TestDecodeReportsMalformedMergeValue(t *testing.T) {
	const y = `
openapi: 3.0.0
info: {title: Probe, version: "1.0"}
paths:
  /a:
    get:
      operationId: a
      responses:
        "200":
          <<: 5
`
	// v2 and plain v3 Unmarshal refuse a merge value that is neither a
	// mapping nor a sequence of mappings; the node walk must not go
	// silently lossy on the same document.
	_, _, err := Generate([]byte(y), Options{ConnectorID: "probe"})
	if err == nil {
		t.Fatal("a malformed merge value must be refused with an error, got nil")
	}
}
