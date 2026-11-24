package numeric_test

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/numeric"
)

func read(t *testing.T, v any) numeric.Number {
	t.Helper()
	n, err := numeric.Read(t.Context(), v)
	if err != nil {
		t.Fatalf("Read(%T %v): %v", v, v, err)
	}
	return n
}

func TestIntegerAdmissionNeverRounds(t *testing.T) {
	for _, tc := range []struct {
		raw      string
		n        int64
		ok, frac bool
	}{
		{"9007199254740993.0", 9007199254740993, true, false},
		{"9.007199254740993e15", 9007199254740993, true, false},
		{"9223372036854775807.000", math.MaxInt64, true, false},
		{"-9223372036854775808.0", math.MinInt64, true, false},
		{"9223372036854775808", 0, false, false},
		{"-9223372036854775809", 0, false, false},
		{"1.0000000000000001", 0, false, true},
		{"1e-400", 0, false, true},
		{"1e400", 0, false, false},
		{"-0e99999999999999999999999999", 0, true, false},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			n, ok, frac := read(t, json.Number(tc.raw)).Int64()
			if n != tc.n || ok != tc.ok || frac != tc.frac {
				t.Fatalf("got %d/%v/%v, want %d/%v/%v", n, ok, frac, tc.n, tc.ok, tc.frac)
			}
		})
	}
}

func TestNativeKindsAndNonNumbers(t *testing.T) {
	for _, v := range []any{int(7), int8(7), int16(7), int32(7), int64(7), uint(7), uint8(7), uint16(7), uint32(7), uint64(7), uintptr(7), float32(7), float64(7), json.Number("7e0")} {
		n := read(t, v)
		got, ok, frac := n.Int64()
		if !ok || frac || got != 7 {
			t.Fatalf("%T became %d/%v/%v", v, got, ok, frac)
		}
	}
	for _, v := range []any{nil, "7", true, []int{7}, big.NewInt(7), struct{ N int }{7}} {
		if _, err := numeric.Read(t.Context(), v); !errors.Is(err, numeric.ErrNotNumber) {
			t.Fatalf("%T accepted: %v", v, err)
		}
	}
	for _, v := range []any{math.NaN(), math.Inf(1), float32(math.Inf(-1)), json.Number("1 "), json.Number(" 1"), json.Number("01.0"), json.Number("1/2"), json.Number("true"), json.Number("+1"), json.Number(""), json.Number("1e"), json.Number("1\n")} {
		if _, err := numeric.Read(t.Context(), v); !errors.Is(err, numeric.ErrInvalidNumber) {
			t.Fatalf("%v accepted: %v", v, err)
		}
	}
	if !read(t, float64(1e20)).NativeFloat() || read(t, json.Number("1e20")).NativeFloat() {
		t.Fatal("original arithmetic domains lost")
	}
	if _, ok, frac := read(t, uint64(math.MaxUint64)).Int64(); ok || frac {
		t.Fatal("uint64 narrowed")
	}
}

func TestExactComparisonAndEqualityKey(t *testing.T) {
	a, b, c := read(t, json.Number("0.1")), read(t, float64(0.1)), read(t, json.Number("0.10000000000000001"))
	for _, p := range [][2]numeric.Number{{a, b}, {b, c}, {a, c}} {
		cmp, err := p[0].Compare(t.Context(), p[1])
		if err != nil || cmp >= 0 {
			t.Fatalf("not strictly ordered: %d %v", cmp, err)
		}
	}
	for _, group := range [][]any{{json.Number("1"), json.Number("1.0"), json.Number("1e0"), int64(1), uint64(1), float64(1)}, {json.Number("-0.00e100"), float64(0), math.Copysign(0, -1), int(0)}, {json.Number("0.5"), float32(0.5), float64(0.5)}} {
		first := read(t, group[0])
		key, err := first.Key(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range group[1:] {
			n := read(t, v)
			got, err := n.Key(t.Context())
			if err != nil || got != key {
				t.Fatalf("equal %T %v key=%q, want %q (%v)", v, v, got, key, err)
			}
			cmp, err := first.Compare(t.Context(), n)
			if cmp != 0 || err != nil {
				t.Fatal(cmp, err)
			}
		}
	}
	x, y := read(t, int64(9007199254740993)), read(t, int64(9007199254740992))
	if cmp, _ := x.Compare(t.Context(), y); cmp != 1 {
		t.Fatal("adjacent integers collapsed")
	}
	var zero numeric.Number
	if !zero.IsZero() {
		t.Fatal("zero value")
	}
	if v, ok := zero.Float64(); v != 0 || !ok {
		t.Fatal("zero float")
	}
}

func TestRoundingBeforeRangeCheck(t *testing.T) {
	for _, tc := range []struct {
		raw          string
		floor, round int64
		fok, rok     bool
	}{
		{"9223372036854775807.9", math.MaxInt64, 0, true, false},
		{"9223372036854775807.4", math.MaxInt64, math.MaxInt64, true, true},
		{"-9223372036854775807.5", math.MinInt64, math.MinInt64, true, true},
		{"-9223372036854775808.1", 0, math.MinInt64, false, true},
		{"-9223372036854775808.5", 0, 0, false, false},
		{"0.99999999999999999", 0, 1, true, true},
		{"-0.0000000000001", -1, 0, true, true},
		{"5e-1", 0, 1, true, true}, {"-5e-1", -1, -1, true, true},
		{"1e-400", 0, 0, true, true}, {"-1e-400", -1, 0, true, true},
		{"1e400", 0, 0, false, false}, {"-0", 0, 0, true, true},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			n := read(t, json.Number(tc.raw))
			f, fo := n.FloorInt64()
			r, ro := n.RoundInt64()
			if f != tc.floor || fo != tc.fok || r != tc.round || ro != tc.rok {
				t.Fatalf("floor=%d/%v round=%d/%v", f, fo, r, ro)
			}
		})
	}
	for _, v := range []any{float64(0.5), float64(-0.5), float32(-1.25), math.SmallestNonzeroFloat64} {
		n := read(t, v)
		f, _ := n.Float64()
		floor, ok := n.FloorInt64()
		round, rok := n.RoundInt64()
		if !ok || !rok || floor != int64(math.Floor(f)) || round != int64(math.Round(f)) {
			t.Fatalf("native rounding %v", v)
		}
	}
}

func TestExplicitFloatConversionKeepsItsDomain(t *testing.T) {
	if _, ok := read(t, json.Number("1e400")).Float64(); ok {
		t.Fatal("overflow accepted")
	}
	n := read(t, json.Number("-1e-400"))
	f, ok := n.Float64()
	if !ok || f != 0 || !math.Signbit(f) || n.IsZero() {
		t.Fatal("underflow leaked into exact zero or lost its sign")
	}
	for _, v := range []float64{math.Copysign(0, -1), math.SmallestNonzeroFloat64, math.MaxFloat64, 0.1} {
		n := read(t, v)
		f, ok := n.Float64()
		if !ok || math.Float64bits(v) != math.Float64bits(f) {
			t.Fatal("float bits changed", v, f)
		}
	}
}

func TestBoundedRationalOracle(t *testing.T) {
	rng := rand.New(rand.NewPCG(31, 13))
	generate := func() string {
		s := strconv.FormatUint(rng.Uint64(), 10)
		cut := 1 + rng.IntN(len(s))
		if cut < len(s) {
			s = s[:cut] + "." + s[cut:]
		}
		if rng.IntN(2) == 0 {
			s = "-" + s
		}
		return s + "e" + strconv.Itoa(rng.IntN(801)-400)
	}
	for i := 0; i < 3000; i++ {
		x, y := generate(), generate()
		a, b := read(t, json.Number(x)), read(t, json.Number(y))
		ar, _ := new(big.Rat).SetString(x)
		br, _ := new(big.Rat).SetString(y)
		got, err := a.Compare(t.Context(), b)
		if err != nil || got != ar.Cmp(br) {
			t.Fatalf("order %s / %s: %d %v", x, y, got, err)
		}
		n, ok, frac := a.Int64()
		if ok != (ar.IsInt() && ar.Num().IsInt64()) || frac == ar.IsInt() || (ok && n != ar.Num().Int64()) {
			t.Fatalf("integer %s: %d/%v/%v", x, n, ok, frac)
		}
		f := math.Float64frombits(rng.Uint64())
		if math.IsNaN(f) || math.IsInf(f, 0) {
			continue
		}
		fn := read(t, f)
		got, err = fn.Compare(t.Context(), a)
		if err != nil || got != new(big.Rat).SetFloat64(f).Cmp(ar) {
			t.Fatalf("float order %v / %s", f, x)
		}
	}
}

func TestHugeExponentNeverExpandsItsValue(t *testing.T) {
	exp := strings.Repeat("9", 20000)
	for _, p := range [][3]string{{"1e" + exp, "2e" + exp, "-1"}, {"1e-" + exp, "0", "1"}, {"-1e-" + exp, "0", "-1"}, {"0e" + exp, "0e-" + exp, "0"}} {
		a, b := read(t, json.Number(p[0])), read(t, json.Number(p[1]))
		cmp, err := a.Compare(t.Context(), b)
		want, _ := strconv.Atoi(p[2])
		if err != nil || cmp != want {
			t.Fatal("huge exponent compare", cmp, err)
		}
		key, err := a.Key(t.Context())
		if err != nil || len(key) > len(p[0])+10 {
			t.Fatal("key expanded exponent")
		}
		_, _, _ = a.Int64()
		_, _ = a.RoundInt64()
		_, _ = a.FloorInt64()
	}
}

func TestCancellationAndReadOnlyCopies(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := numeric.Read(ctx, json.Number("1")); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	a := read(t, json.Number("1.00e400"))
	before, _ := a.Key(t.Context())
	copy := a
	if _, err := a.Compare(ctx, copy); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := a.Key(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			t.Parallel()
			for j := 0; j < 50; j++ {
				copy.Int64()
				copy.FloorInt64()
				copy.RoundInt64()
				got, err := copy.Key(t.Context())
				if err != nil || got != before {
					t.Fatal("copy mutated", err)
				}
				if cmp, err := a.Compare(t.Context(), copy); cmp != 0 || err != nil {
					t.Fatal(cmp, err)
				}
			}
		})
	}
}

// cancelDuringScan models cancellation after the preceding check has read its
// answer. The next check must observe the real context's cancellation. This
// exercises the work boundary without a scheduler race or a wall-clock sleep.
type cancelDuringScan struct {
	context.Context
	cancel context.CancelFunc
	checks int
}

func (c *cancelDuringScan) Err() error {
	err := c.Context.Err()
	c.checks++
	if c.checks == 3 {
		c.cancel()
	}
	return err
}
func TestCancellationAfterCoefficientScan(t *testing.T) {
	for _, last := range []string{"0", "1"} {
		t.Run(last, func(t *testing.T) {
			base, cancel := context.WithCancel(t.Context())
			defer cancel()
			ctx := &cancelDuringScan{Context: base, cancel: cancel}
			_, err := numeric.Read(ctx, json.Number("0."+strings.Repeat("0", 1024)+last))
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation lost after coefficient scan: %v", err)
			}
		})
	}
}
