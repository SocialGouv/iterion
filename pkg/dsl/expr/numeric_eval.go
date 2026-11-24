package expr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/SocialGouv/iterion/pkg/numeric"
)

// NumericMode selects a whole evaluation's numeric contract. It is independent
// of the values returned by its callbacks and does not mutate the parsed AST.
type NumericMode uint8

const (
	LegacyNumeric NumericMode = iota
	ExactNumeric
)

func (st *evalState) exactNumbers() bool { return st.numericMode == ExactNumeric }

func (st *evalState) numberContext() context.Context {
	if st.cancel != nil {
		return st.cancel
	}
	return context.Background()
}

// readNumber charges the unbounded decimal representation before any reader
// validates, normalizes, hashes or compares it. Native numbers have fixed size.
func (st *evalState) readNumber(v any) (numeric.Number, bool, error) {
	if err := st.chargeNumberBytes(v); err != nil {
		return numeric.Number{}, false, err
	}
	n, err := numeric.Read(st.numberContext(), v)
	if errors.Is(err, numeric.ErrNotNumber) {
		return numeric.Number{}, false, nil
	}
	return n, err == nil, err
}

func (st *evalState) chargeNumberBytes(v any) error {
	cost := 1
	if n, ok := v.(json.Number); ok {
		cost = len(n)
	}
	return st.debit(cost)
}

// The lexer accepts leading zeroes in decimal literals. JSON does not. Keep
// their fractional digits verbatim while normalizing only that integer prefix.
func normalizeDecimalLiteral(text string) string {
	i := strings.IndexByte(text, '.')
	if i < 0 {
		return text
	}
	integer := strings.TrimLeft(text[:i], "0")
	if integer == "" {
		integer = "0"
	}
	return integer + text[i:]
}

func (st *evalState) decimalLiteral(text string) (any, error) {
	v := json.Number(text)
	if _, _, err := st.readNumber(v); err != nil {
		return nil, err
	}
	return v, nil
}

func (st *evalState) boolean(v any) (bool, error) {
	if !st.exactNumbers() {
		return truthy(v), nil
	}
	n, ok, err := st.readNumber(v)
	if err != nil {
		return false, err
	}
	if ok {
		return !n.IsZero(), nil
	}
	if s, ok := v.(string); ok {
		if err := st.debit(len(s)); err != nil {
			return false, err
		}
	}
	return truthy(v), nil
}

func (st *evalState) integer(v any) (int64, bool, error) {
	if !st.exactNumbers() {
		n, ok := toInt(v)
		return n, ok, nil
	}
	n, ok, err := st.readNumber(v)
	if err != nil || !ok {
		return 0, false, err
	}
	i, fits, _ := n.Int64()
	return i, fits, nil
}

func (st *evalState) exactEqual(a, b any) (bool, error) {
	an, anOK, err := st.readNumber(a)
	if err != nil {
		return false, err
	}
	bn, bnOK, err := st.readNumber(b)
	if err != nil {
		return false, err
	}
	if anOK || bnOK {
		if !anOK || !bnOK {
			return false, nil
		}
		cmp, err := an.Compare(st.numberContext(), bn)
		return cmp == 0, err
	}
	if as, ok := a.(string); ok {
		if bs, ok := b.(string); ok {
			if err := st.debit(len(as)); err != nil {
				return false, err
			}
			if err := st.debit(len(bs)); err != nil {
				return false, err
			}
		}
	}
	return equals(a, b), nil
}

func (st *evalState) exactBinary(op string, a, b any) (any, error) {
	if op == "==" || op == "!=" {
		eq, err := st.exactEqual(a, b)
		if op == "!=" {
			eq = !eq
		}
		return eq, err
	}
	if op == "<" || op == "<=" || op == ">" || op == ">=" {
		an, aOK, err := st.readNumber(a)
		if err != nil {
			return nil, err
		}
		bn, bOK, err := st.readNumber(b)
		if err != nil {
			return nil, err
		}
		if aOK && bOK {
			cmp, err := an.Compare(st.numberContext(), bn)
			if err != nil {
				return nil, err
			}
			switch op {
			case "<":
				return cmp < 0, nil
			case "<=":
				return cmp <= 0, nil
			case ">":
				return cmp > 0, nil
			default:
				return cmp >= 0, nil
			}
		}
		as, aOK := a.(string)
		bs, bOK := b.(string)
		if !aOK || !bOK {
			return nil, fmt.Errorf("expr: cannot compare %T %s %T", a, op, b)
		}
		if err := st.debit(len(as)); err != nil {
			return nil, err
		}
		if err := st.debit(len(bs)); err != nil {
			return nil, err
		}
		return compare(op, as, bs)
	}
	if op == "+" {
		_, aString := a.(string)
		_, bString := b.(string)
		if aString || bString {
			as, err := st.scalarText(a)
			if err != nil {
				return nil, err
			}
			bs, err := st.scalarText(b)
			if err != nil {
				return nil, err
			}
			return as + bs, nil
		}
	}
	return st.numberArithmetic(op, a, b)
}

// arithmeticOperand chooses the domain before conversion. A decimal fraction
// stays fractional even if IEEE rounding later makes it an integral float.
func (st *evalState) arithmeticOperand(v any) (int64, float64, bool, error) {
	n, ok, err := st.readNumber(v)
	if err != nil {
		return 0, 0, false, err
	}
	if !ok {
		return 0, 0, false, fmt.Errorf("expr: expected numeric operand, got %T", v)
	}
	i, fits, fractional := n.Int64()
	if fits {
		return i, 0, false, nil
	}
	if !fractional && !n.NativeFloat() {
		return 0, 0, false, fmt.Errorf("expr: exact integer is outside int64 arithmetic domain")
	}
	f, finite := n.Float64()
	if !finite {
		return 0, 0, false, fmt.Errorf("expr: number is outside finite IEEE arithmetic domain")
	}
	return 0, f, true, nil
}

func (st *evalState) numberArithmetic(op string, a, b any) (any, error) {
	ai, af, aFloat, err := st.arithmeticOperand(a)
	if err != nil {
		return nil, err
	}
	bi, bf, bFloat, err := st.arithmeticOperand(b)
	if err != nil {
		return nil, err
	}
	if !aFloat && !bFloat {
		return arith(op, ai, bi)
	}
	if !aFloat {
		af = float64(ai)
	}
	if !bFloat {
		bf = float64(bi)
	}
	var result float64
	switch op {
	case "+":
		result = af + bf
	case "-":
		result = af - bf
	case "*":
		result = af * bf
	case "/":
		if bf == 0 {
			return nil, fmt.Errorf("expr: float division by zero")
		}
		result = af / bf
	default:
		return nil, fmt.Errorf("expr: %s requires integer operands", op)
	}
	if math.IsNaN(result) || math.IsInf(result, 0) {
		return nil, fmt.Errorf("expr: float arithmetic overflow")
	}
	return result, nil
}

func (st *evalState) negateNumber(v any) (any, error) {
	i, f, floating, err := st.arithmeticOperand(v)
	if err != nil {
		return nil, err
	}
	if floating {
		return -f, nil
	}
	if i == math.MinInt64 {
		return nil, fmt.Errorf("expr: integer negation overflow")
	}
	return -i, nil
}
