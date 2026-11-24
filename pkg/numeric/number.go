// Package numeric reads materialized numbers without rounding their integer
// domain or order. It does not select an expression's arithmetic domain.
// Decimal arithmetic is not implemented: Float64 is an explicit IEEE conversion.
package numeric

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"strconv"
	"strings"
)

var (
	ErrNotNumber     = errors.New("numeric: unsupported numeric type")
	ErrInvalidNumber = errors.New("numeric: invalid or non-finite number")
)

// Number is an immutable mathematical value. A native float denotes its actual
// binary value, including precision already lost before Read. The zero value
// denotes zero. Copies may share immutable storage and are safe for concurrent
// readers. Read never calls user-provided methods or dereferences pointers.
type Number struct {
	digits      string
	point       big.Int // value = sign * digits * 10^(point-len(digits))
	negative    bool
	source      string
	nativeFloat bool
}

// Read accepts built-in numeric types and json.Number. Storage grows with the
// exponent's text, never with its numeric value. Callers with a work budget must
// charge a json.Number's byte length before this reading. Context cancellation
// is checked around standard-library operations, which are not preemptible.
func Read(ctx context.Context, value any) (Number, error) {
	if err := ctx.Err(); err != nil {
		return Number{}, err
	}
	var text string
	switch v := value.(type) {
	case json.Number:
		text = string(v)
	case int:
		text = strconv.FormatInt(int64(v), 10)
	case int8:
		text = strconv.FormatInt(int64(v), 10)
	case int16:
		text = strconv.FormatInt(int64(v), 10)
	case int32:
		text = strconv.FormatInt(int64(v), 10)
	case int64:
		text = strconv.FormatInt(v, 10)
	case uint:
		text = strconv.FormatUint(uint64(v), 10)
	case uint8:
		text = strconv.FormatUint(uint64(v), 10)
	case uint16:
		text = strconv.FormatUint(uint64(v), 10)
	case uint32:
		text = strconv.FormatUint(uint64(v), 10)
	case uint64:
		text = strconv.FormatUint(v, 10)
	case uintptr:
		text = strconv.FormatUint(uint64(v), 10)
	case float32:
		return readFloat(ctx, float64(v))
	case float64:
		return readFloat(ctx, v)
	default:
		return Number{}, ErrNotNumber
	}
	return readDecimal(ctx, text)
}

func readFloat(ctx context.Context, value float64) (Number, error) {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return Number{}, ErrInvalidNumber
	}
	rat := new(big.Rat).SetFloat64(value)
	// The reduced denominator of a binary float is a power of two. At most
	// 1074 decimal places suffice; this never expands an input JSON exponent.
	text := rat.FloatString(int(rat.Denom().TrailingZeroBits()))
	n, err := readDecimal(ctx, text)
	if err != nil {
		return Number{}, err
	}
	n.source = strconv.FormatFloat(value, 'g', -1, 64)
	n.nativeFloat = true
	return n, nil
}

func readDecimal(ctx context.Context, text string) (Number, error) {
	if len(text) == 0 || (text[0] != '-' && (text[0] < '0' || text[0] > '9')) || text[len(text)-1] < '0' || text[len(text)-1] > '9' || !json.Valid([]byte(text)) {
		return Number{}, ErrInvalidNumber
	}
	if err := ctx.Err(); err != nil {
		return Number{}, err
	}
	n := Number{source: text}
	if text[0] == '-' {
		n.negative = true
		text = text[1:]
	}
	coefficient := text
	if i := strings.IndexAny(text, "eE"); i >= 0 {
		coefficient = text[:i]
		if _, ok := n.point.SetString(text[i+1:], 10); !ok {
			return Number{}, ErrInvalidNumber
		}
	}
	if err := ctx.Err(); err != nil {
		return Number{}, err
	}
	integerDigits := len(coefficient)
	if i := strings.IndexByte(coefficient, '.'); i >= 0 {
		integerDigits = i
		coefficient = coefficient[:i] + coefficient[i+1:]
	}
	trimmed := strings.TrimLeft(coefficient, "0")
	if err := ctx.Err(); err != nil {
		return Number{}, err
	}
	if trimmed == "" {
		return Number{source: n.source}, nil
	}
	leading := len(coefficient) - len(trimmed)
	n.point.Add(&n.point, big.NewInt(int64(integerDigits-leading)))
	n.digits = strings.TrimRight(trimmed, "0")
	if err := ctx.Err(); err != nil {
		return Number{}, err
	}
	return n, nil
}

// NativeFloat reports the original numeric domain. In particular, a finite
// native float outside int64 is still a float, not an overflowing exact integer.
func (n Number) NativeFloat() bool { return n.nativeFloat }

func (n Number) IsZero() bool { return n.digits == "" }

// Int64 distinguishes a fractional value from a non-fractional overflow. It
// never converts through Float64, even for a decimal spelling such as 1.00e3.
func (n Number) Int64() (value int64, ok bool, fractional bool) {
	if n.IsZero() {
		return 0, true, false
	}
	if n.point.Cmp(big.NewInt(int64(len(n.digits)))) < 0 {
		return 0, false, true
	}
	if n.point.Cmp(big.NewInt(19)) > 0 {
		return 0, false, false
	}
	text := n.digits + strings.Repeat("0", int(n.point.Int64())-len(n.digits))
	if n.negative {
		text = "-" + text
	}
	value, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, false, false
	}
	return value, true, false
}

// Float64 explicitly rounds into the finite IEEE domain. Underflow to signed
// zero follows strconv.ParseFloat's established behavior; it must not be used
// to decide integer admission, ordering, truthiness, or exact floor/round.
func (n Number) Float64() (float64, bool) {
	if n.source == "" {
		return 0, true
	}
	f, err := strconv.ParseFloat(n.source, 64)
	return f, err == nil && !math.IsNaN(f) && !math.IsInf(f, 0)
}

// Compare orders mathematical values exactly. It is transitive across native
// integer, finite binary float, and decimal JSON representations.
func (n Number) Compare(ctx context.Context, other Number) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if n.IsZero() && other.IsZero() {
		return 0, nil
	}
	if n.negative != other.negative {
		if n.negative {
			return -1, nil
		}
		return 1, nil
	}
	sign := 1
	if n.negative {
		sign = -1
	}
	if n.IsZero() {
		return -sign, nil
	}
	if other.IsZero() {
		return sign, nil
	}
	if cmp := n.point.Cmp(&other.point); cmp != 0 {
		return sign * cmp, nil
	}
	for i := 0; i < max(len(n.digits), len(other.digits)); i++ {
		if i%256 == 0 {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
		}
		a, b := byte('0'), byte('0')
		if i < len(n.digits) {
			a = n.digits[i]
		}
		if i < len(other.digits) {
			b = other.digits[i]
		}
		if a < b {
			return -sign, nil
		}
		if a > b {
			return sign, nil
		}
	}
	return 0, nil
}

// Key is a mathematical equality key, not a wire format or a preserved
// lexeme. Equal numbers share a key; callers retain their original value.
// Keys belong in a separate numeric namespace from user strings.
func (n Number) Key(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if n.IsZero() {
		return "0", nil
	}
	sign := "+"
	if n.negative {
		sign = "-"
	}
	key := sign + n.digits + "@" + n.point.String()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return key, nil
}

// FloorInt64 rounds toward negative infinity before checking the int64 domain.
func (n Number) FloorInt64() (int64, bool) { return n.roundedInt64(false) }

// RoundInt64 rounds to the nearest integer, with ties away from zero.
func (n Number) RoundInt64() (int64, bool) { return n.roundedInt64(true) }

func (n Number) roundedInt64(nearest bool) (int64, bool) {
	if value, ok, fractional := n.Int64(); ok || !fractional {
		return value, ok
	}
	if n.point.Cmp(big.NewInt(19)) > 0 {
		return 0, false
	}
	var magnitude uint64
	p := 0
	if n.point.Sign() > 0 {
		p = int(n.point.Int64())
		var err error
		magnitude, err = strconv.ParseUint(n.digits[:p], 10, 64)
		if err != nil {
			return 0, false
		}
	}
	up := n.negative
	if nearest {
		up = n.point.Sign() >= 0 && n.digits[p] >= '5'
	}
	if up {
		if magnitude == math.MaxUint64 {
			return 0, false
		}
		magnitude++
	}
	limit := uint64(math.MaxInt64)
	if n.negative {
		limit++
	}
	if magnitude > limit {
		return 0, false
	}
	if n.negative {
		if magnitude == uint64(1)<<63 {
			return math.MinInt64, true
		}
		return -int64(magnitude), true
	}
	return int64(magnitude), true
}
