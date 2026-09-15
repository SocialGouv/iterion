package ast_test

import (
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)

// A JSON value travels as bytes, and the transport's nil-element walk does
// not visit them one by one: 16 MiB of default reads in well under a
// second. Walked byte by byte it took three — the walk is a reflection
// over every element of every list of the document, and a byte is an
// element too unless the walk knows better.
func TestUnmarshalFileDoesNotWalkAJSONValueByteByByte(t *testing.T) {
	if raceEnabled || testing.Short() {
		t.Skip("a timing witness: measured without the race detector")
	}
	big := strings.Repeat("x", 16<<20)
	raw := []byte(`{"contracts":[{"name":"c","inputs":[{"name":"x","type":"string","default":"` + big + `"}]}]}`)
	start := time.Now()
	f, err := ast.UnmarshalFile(raw)
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 1500*time.Millisecond {
		t.Fatalf("16 MiB of JSON took %s to read: the nil walk visits the bytes", d)
	}
	if got := len(f.Contracts[0].Inputs[0].Default); got != len(big)+2 {
		t.Fatalf("the default came back %d bytes long, want %d", got, len(big)+2)
	}
}
