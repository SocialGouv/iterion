package ast_test

import (
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)

// A JSON value travels as bytes, and the transport's nil-element walk does
// not visit them one by one. The witness is relative: the same 16 MiB in a
// plain string field is the control, which factors the machine out — a
// loaded CI runner slows both alike and never reads as a regression.
// Healthy, the default costs a few times the control (json.Valid, a decode
// and a re-marshal on top of the decode both share); walked byte by byte it
// cost about twenty times. Each side takes the best of three runs, so a
// garbage collection landing on one of them does not decide the verdict.
func TestUnmarshalFileDoesNotWalkAJSONValueByteByByte(t *testing.T) {
	if raceEnabled || testing.Short() {
		t.Skip("a timing witness: measured without the race detector")
	}
	big := strings.Repeat("x", 16<<20)
	control := []byte(`{"contracts":[{"name":"c","responsibility":"` + big + `"}]}`)
	measured := []byte(`{"contracts":[{"name":"c","inputs":[{"name":"x","type":"string","default":"` + big + `"}]}]}`)
	best := func(raw []byte) time.Duration {
		var d time.Duration
		for i := 0; i < 3; i++ {
			start := time.Now()
			f, err := ast.UnmarshalFile(raw)
			if err != nil {
				t.Fatal(err)
			}
			if len(f.Contracts) != 1 {
				t.Fatal("the contract was not read")
			}
			if run := time.Since(start); i == 0 || run < d {
				d = run
			}
		}
		return d
	}
	base, d := best(control), best(measured)
	if d > 10*base {
		t.Fatalf("16 MiB of JSON took %s to read against a %s control: the nil walk visits the bytes", d, base)
	}
	f, err := ast.UnmarshalFile(measured)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(f.Contracts[0].Inputs[0].Default); got != len(big)+2 {
		t.Fatalf("the default came back %d bytes long, want %d", got, len(big)+2)
	}
}
