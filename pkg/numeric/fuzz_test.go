package numeric_test

import (
	"encoding/json"
	"math/big"
	"strconv"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/numeric"
)

func FuzzNumberNeverLosesIntegerClassification(f *testing.F) {
	for _, s := range []string{"0", "-0.0", "9223372036854775807.9", "9007199254740993.0", "1e400", "1e-400", "1e999999999999999999999", "1.000000000000000001", "01", "-9223372036854775808", "0.1"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		if len(raw) > 256 {
			t.Skip("input exceeds the bounded independent rational oracle corpus")
		}
		n, err := numeric.Read(t.Context(), json.Number(raw))
		if err != nil {
			return
		}
		if order, err := n.Compare(t.Context(), n); err != nil || order != 0 {
			t.Fatal("self comparison", order, err)
		}
		if _, err := n.Key(t.Context()); err != nil {
			t.Fatal(err)
		}
		n.Float64()
		n.FloorInt64()
		n.RoundInt64()
		// Bound only the independent rational oracle, never production parsing:
		// big.Rat materializes powers of ten for the exponent's VALUE.
		if e := strings.IndexAny(raw, "eE"); e >= 0 {
			v, err := strconv.Atoi(raw[e+1:])
			if err != nil || v < -400 || v > 400 {
				return
			}
		}
		r, ok := new(big.Rat).SetString(raw)
		if !ok {
			t.Fatal("accepted non-rational JSON number")
		}
		v, fit, fractional := n.Int64()
		wantFit := r.IsInt() && r.Num().IsInt64()
		if fit != wantFit || fractional == r.IsInt() || (fit && v != r.Num().Int64()) {
			t.Fatalf("wrong integer classification %q: %d/%v/%v", raw, v, fit, fractional)
		}
	})
}
