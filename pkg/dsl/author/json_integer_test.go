package author

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)

func TestJSONIntegerWriterRefusesOverflowByValue(t *testing.T) {
	for _, number := range []string{"9223372036854775808", "18446744073709551615", "18446744073709551616", "99999999999999999999", "-9223372036854775809"} {
		for _, wrap := range []string{"%s", "[%s]", `{"nested":[%s]}`} {
			for _, site := range []string{"input", "output", "criterion"} {
				t.Run(number+"/"+wrap+"/"+site, func(t *testing.T) {
					raw := json.RawMessage(strings.ReplaceAll(wrap, "%s", number))
					c := &ast.ContractDecl{Name: "c"}
					f := &ast.File{Contracts: []*ast.ContractDecl{c}}
					switch site {
					case "input":
						c.Inputs = []*ast.PortDecl{{Name: "value", Type: "json", Default: raw}}
					case "output":
						c.Outputs = []*ast.PortDecl{{Name: "value", Type: "json", Default: raw}}
					case "criterion":
						c.Criteria = []*ast.CriterionDecl{{Name: "check", Kind: "schema", Params: raw}}
					}
					out, err := Write(f)
					if err == nil || !strings.Contains(err.Error(), number) || !strings.Contains(err.Error(), "int64") {
						t.Fatalf("got output %s, error %v; want refusal naming number and int64", out, err)
					}
					if len(out) != 0 {
						t.Fatal("refused conversion returned partial YAML")
					}
				})
			}
		}
	}
}

func TestJSONIntegerWriterKeepsInt64Bounds(t *testing.T) {
	for _, number := range []string{"0", "9223372036854775807", "-9223372036854775808"} {
		t.Run(number, func(t *testing.T) {
			w := &writer{}
			n := w.jsonValueNode(json.Number(number))
			if w.err != nil {
				t.Fatal(w.err)
			}
			got, ok := intOf(n)
			want, err := json.Number(number).Int64()
			if err != nil || !ok || got != want {
				t.Fatalf("got %d/%v, want %d (%v)", got, ok, want, err)
			}
		})
	}
}

// Connector action params are strings, unlike contract criterion params.
// A string that resembles an oversized integer must stay accepted.
func TestJSONIntegerWriterLeavesActionStringsAlone(t *testing.T) {
	value := "99999999999999999999"
	f := &ast.File{Tools: []*ast.ToolNodeDecl{{Name: "act", Action: "sample.call", Params: []ast.ActionParam{{Key: "value", Value: value}}}}}
	out, err := Write(f)
	if err != nil {
		t.Fatal(err)
	}
	back := Parse("action.bot.yaml", out)
	if hasParseErrors(back.Diagnostics) {
		t.Fatal(back.Diagnostics)
	}
	if got := back.File.Tools[0].Params[0].Value; got != value {
		t.Fatalf("got %q, want %q", got, value)
	}
}
