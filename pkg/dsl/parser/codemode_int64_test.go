package parser

import "testing"

// The documented `memory` ceiling has to PARSE.
//
// 2147483648 is scripthost.Ceiling().Memory and one past what an int32 holds,
// so a reader going through `strconv.Atoi` — whose int is 32 bits on a 32-bit
// build — refuses the very value the registry documents. This test is green
// on amd64 whatever the reader does; it only reddens under GOARCH=386, which
// is where it was seen red:
//
//	GOARCH=386 go test ./pkg/dsl/parser/ -run TestTheDocumentedMemoryCeilingParses
//
// Only amd64 and arm64 are released, so nothing shipped broken — the finding
// is the asymmetry: the AST field and the author writer were widened to int64
// on purpose, and the reader was the half left behind.
func TestTheDocumentedMemoryCeilingParses(t *testing.T) {
	t.Parallel()
	const src = `agent a:
  model: "m"
  backend: "claw"
  codemode:
    memory: 2147483648
    result_bytes: 8388608

workflow w:
  entry: a
  a -> done
`
	res, err := Parse("ceiling.bot", src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(res.Diagnostics) > 0 {
		t.Fatalf("the documented ceiling drew diagnostics: %v", res.Diagnostics)
	}
	cb := res.File.Agents[0].Codemode
	if cb == nil {
		t.Fatalf("no codemode block")
	}
	if cb.Memory != 2147483648 {
		t.Errorf("memory = %d, want 2147483648 (the platform ceiling)", cb.Memory)
	}
	if cb.ResultBytes != 8388608 {
		t.Errorf("result_bytes = %d, want 8388608", cb.ResultBytes)
	}
}
