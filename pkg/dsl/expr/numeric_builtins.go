package expr

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/numeric"
)

var exactNumericBuiltins = map[string]func(*evalState, []any) (any, error){
	"contains": exactContains, "unique": exactUnique, "sort": exactSort,
	"sum": exactSum, "min": exactMin, "max": exactMax,
	"floor": exactFloor, "round": exactRound, "join": exactJoin,
}

func exactContains(st *evalState, args []any) (any, error) {
	if args[0] == nil {
		return false, nil
	}
	arr, ok := args[0].([]any)
	if !ok {
		return nil, fmt.Errorf("expr: contains() expects array as first argument, got %T", args[0])
	}
	target, err := st.collectionKey(args[1])
	if err != nil {
		return nil, err
	}
	for _, v := range arr {
		key, err := st.collectionKey(v)
		if err != nil {
			return nil, err
		}
		if key == target {
			return true, nil
		}
	}
	return false, nil
}

func exactUnique(st *evalState, args []any) (any, error) {
	if args[0] == nil {
		return []any{}, nil
	}
	arr, ok := args[0].([]any)
	if !ok {
		return nil, fmt.Errorf("expr: unique() expects array, got %T", args[0])
	}
	if err := st.debit(len(arr)); err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(arr))
	out := make([]any, 0, len(arr))
	for _, v := range arr {
		key, err := st.collectionKey(v)
		if err != nil {
			return nil, err
		}
		if !seen[key] {
			seen[key] = true
			out = append(out, v)
		}
	}
	return out, nil
}

// collectionKey preserves the collection helpers' existing nonnumeric shape
// identity, while all materialized numbers share a mathematical namespace.
func (st *evalState) collectionKey(v any) (string, error) {
	n, isNumber, err := st.readNumber(v)
	if err != nil {
		return "", err
	}
	if isNumber {
		// Reserve key construction before allocating it. A native binary float
		// expands to at most 1074 fractional decimal places; other native values
		// need at most 20 digits. JSON exponent text never expands to its power.
		reserve := 32
		switch value := v.(type) {
		case json.Number:
			reserve = len(value) + 24
		case float32, float64:
			reserve = 1100
		}
		if err := st.debit(reserve); err != nil {
			return "", err
		}
		key, err := n.Key(st.numberContext())
		return "n:" + key, err
	}
	key, err := st.display(v)
	if err != nil {
		return "", err
	}
	prefix := fmt.Sprintf("%T:", v)
	if err := st.debit(len(prefix)); err != nil {
		return "", err
	}
	return prefix + key, nil
}

func exactSort(st *evalState, args []any) (any, error) {
	arr, err := st.elements(args[0])
	if err != nil {
		return nil, fmt.Errorf("expr: sort() expects array: %w", err)
	}
	type entry struct {
		value  any
		number numeric.Number
		text   string
	}
	entries := make([]entry, len(arr))
	allNumbers := true
	for i, v := range arr {
		n, ok, err := st.readNumber(v)
		if err != nil {
			return nil, err
		}
		entries[i] = entry{value: v, number: n}
		allNumbers = allNumbers && ok
	}
	if !allNumbers {
		for i := range entries {
			text, err := st.display(entries[i].value)
			if err != nil {
				return nil, err
			}
			entries[i].text = text
		}
	}
	// Sorting cannot return an error from its comparator. Once a cancellation
	// or debit fails, stop inspecting values and return that error, never output.
	var compareErr error
	sort.SliceStable(entries, func(i, j int) bool {
		if compareErr != nil {
			return false
		}
		if compareErr = st.debit(1); compareErr != nil {
			return false
		}
		if allNumbers {
			// Charge both original decimal lengths for every comparison;
			// pre-parsed numbers keep sorting free of repeated normalization.
			for _, v := range []any{entries[i].value, entries[j].value} {
				if compareErr = st.chargeNumberBytes(v); compareErr != nil {
					return false
				}
			}
			var cmp int
			cmp, compareErr = entries[i].number.Compare(st.numberContext(), entries[j].number)
			return cmp < 0
		}
		if compareErr = st.debit(len(entries[i].text)); compareErr != nil {
			return false
		}
		if compareErr = st.debit(len(entries[j].text)); compareErr != nil {
			return false
		}
		if compareErr = st.numberContext().Err(); compareErr != nil {
			return false
		}
		return entries[i].text < entries[j].text
	})
	if compareErr != nil {
		return nil, compareErr
	}
	out := make([]any, len(entries))
	for i, e := range entries {
		out[i] = e.value
	}
	return out, nil
}

func exactSum(st *evalState, args []any) (any, error) {
	arr, err := st.elements(args[0])
	if err != nil {
		return nil, fmt.Errorf("expr: sum() expects array: %w", err)
	}
	var totalInt int64
	var totalFloat float64
	floating := false
	for _, v := range arr {
		i, f, isFloat, err := st.arithmeticOperand(v)
		if err != nil {
			return nil, err
		}
		if !floating && !isFloat {
			n, ok := addCheckedInt64(totalInt, i)
			if !ok {
				return nil, fmt.Errorf("expr: sum() integer overflow")
			}
			totalInt = n
			continue
		}
		if !floating {
			totalFloat = float64(totalInt)
			floating = true
		}
		if !isFloat {
			f = float64(i)
		}
		totalFloat += f
		if math.IsNaN(totalFloat) || math.IsInf(totalFloat, 0) {
			return nil, fmt.Errorf("expr: sum() float overflow")
		}
	}
	if floating {
		return totalFloat, nil
	}
	return totalInt, nil
}

func exactMin(st *evalState, args []any) (any, error) { return exactMinMax(st, args, false) }
func exactMax(st *evalState, args []any) (any, error) { return exactMinMax(st, args, true) }
func exactMinMax(st *evalState, args []any, wantMax bool) (any, error) {
	var best any
	var bestN numeric.Number
	found := false
	for _, arg := range args {
		var arr []any
		rv := reflect.ValueOf(arg)
		if arg == nil || rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array {
			var err error
			arr, err = st.elements(arg)
			if err != nil {
				return nil, err
			}
		} else {
			if len(args) == 1 {
				return nil, fmt.Errorf("expr: min/max expects array")
			}
			arr = []any{arg}
		}
		for _, v := range arr {
			n, ok, err := st.readNumber(v)
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, fmt.Errorf("expr: min/max expects numeric elements, got %T", v)
			}
			if !found {
				best, bestN, found = v, n, true
				continue
			}
			if err := st.chargeNumberBytes(best); err != nil {
				return nil, err
			}
			cmp, err := n.Compare(st.numberContext(), bestN)
			if err != nil {
				return nil, err
			}
			if (wantMax && cmp > 0) || (!wantMax && cmp < 0) {
				best, bestN = v, n
			}
		}
	}
	return best, nil
}

func exactFloor(st *evalState, args []any) (any, error) { return exactRoundInt(st, args, false) }
func exactRound(st *evalState, args []any) (any, error) { return exactRoundInt(st, args, true) }
func exactRoundInt(st *evalState, args []any, nearest bool) (any, error) {
	n, ok, err := st.readNumber(args[0])
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("expr: floor/round expects a number, got %T", args[0])
	}
	var i int64
	if nearest {
		i, ok = n.RoundInt64()
	} else {
		i, ok = n.FloorInt64()
	}
	if !ok {
		return nil, fmt.Errorf("expr: rounded number is outside int64 domain")
	}
	return i, nil
}

func exactJoin(st *evalState, args []any) (any, error) {
	sep, ok := args[1].(string)
	if !ok {
		return nil, fmt.Errorf("expr: join() separator must be a string")
	}
	if args[0] == nil {
		return "", nil
	}
	arr, ok := args[0].([]any)
	if !ok {
		return nil, fmt.Errorf("expr: join() expects array")
	}
	var out strings.Builder
	for i, v := range arr {
		if i > 0 {
			if err := st.debit(len(sep)); err != nil {
				return nil, err
			}
			out.WriteString(sep)
		}
		text, err := st.display(v)
		if err != nil {
			return nil, err
		}
		out.WriteString(text)
	}
	return out.String(), nil
}
