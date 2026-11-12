package scripthost

import (
	"reflect"
	"testing"
	"time"
)

// TestEveryBoundIsDefaultedAndCeilinged walks [Limits] by REFLECTION rather
// than by a list someone keeps up to date.
//
// [Limits.Clamp] returns an explicit literal of every field, so a field added
// to the struct and forgotten there is not a compile error: the literal simply
// leaves it at the zero value, and a bound of zero is a bound of nothing or a
// bound of everything depending on which side reads it. The slice that added
// the first new fields (ToolCalls, ToolResultBytes) is the one that owes this
// guard, because by then the shape of that mistake was known and nothing
// caught it.
//
// Three properties per field, each the mirror of one way the literal can be
// wrong:
//
//   - unset in, DEFAULT out. Catches a field missing from Clamp entirely: its
//     zero would survive.
//   - over the ceiling in, CEILING out, and clamped reported. Catches a field
//     whose clamp forgot its ceiling, and a reduction applied in silence,
//     which philosophy 1 forbids.
//   - under the ceiling in, unchanged out, and clamped NOT reported. Catches a
//     clamp that pins a field to the default and ignores the operator.
func TestEveryBoundIsDefaultedAndCeilinged(t *testing.T) {
	t.Parallel()

	typ := reflect.TypeOf(Limits{})
	if typ.NumField() == 0 {
		t.Fatal("Limits has no fields: this guard would pass by vacuity")
	}
	def, ceil := DefaultLimits(), Ceiling()

	for i := range typ.NumField() {
		f := typ.Field(i)
		t.Run(f.Name, func(t *testing.T) {
			defVal := reflect.ValueOf(def).Field(i)
			ceilVal := reflect.ValueOf(ceil).Field(i)

			if isZero(defVal) {
				t.Fatalf("DefaultLimits leaves %s at zero: a script would run with no bound, or with a bound of nothing", f.Name)
			}
			if isZero(ceilVal) {
				t.Fatalf("Ceiling leaves %s at zero: a tenant's value would have nothing above it", f.Name)
			}
			if cmp(ceilVal, defVal) < 0 {
				t.Fatalf("Ceiling's %s (%v) is BELOW the default (%v): the default is already over the platform bound",
					f.Name, ceilVal, defVal)
			}

			// 1. Unset in, default out.
			got, clamped := Limits{}.Clamp()
			gotVal := reflect.ValueOf(got).Field(i)
			if cmp(gotVal, defVal) != 0 {
				t.Errorf("an unset %s clamped to %v, want the default %v — the field is missing from Clamp's literal",
					f.Name, gotVal, defVal)
			}
			if clamped {
				t.Errorf("clamping an entirely unset Limits reported a reduction: nothing was asked for, so nothing was reduced")
			}

			// 2. Over the ceiling in, ceiling out, reduction reported.
			over := Limits{}
			setField(reflect.ValueOf(&over).Elem().Field(i), ceilVal, 2)
			got, clamped = over.Clamp()
			gotVal = reflect.ValueOf(got).Field(i)
			if cmp(gotVal, ceilVal) != 0 {
				t.Errorf("twice the ceiling for %s clamped to %v, want the ceiling %v", f.Name, gotVal, ceilVal)
			}
			if !clamped {
				t.Errorf("%s was reduced to its ceiling and Clamp reported no reduction: a clamp is a replacement of the operator's choice and must be reported",
					f.Name)
			}

			// 3. Under the ceiling in, honoured, no reduction reported.
			under := Limits{}
			setField(reflect.ValueOf(&under).Elem().Field(i), ceilVal, 0) // ceiling-1
			got, clamped = under.Clamp()
			gotVal = reflect.ValueOf(got).Field(i)
			if cmp(gotVal, reflect.ValueOf(under).Field(i)) != 0 {
				t.Errorf("%s asked for %v under its ceiling and came back %v: Clamp overrode a choice it should have honoured",
					f.Name, reflect.ValueOf(under).Field(i), gotVal)
			}
			if clamped {
				t.Errorf("%s under its ceiling reported a reduction", f.Name)
			}
		})
	}
}

// setField writes a value derived from the ceiling into field v: mul=2 for
// twice the ceiling, mul=0 for one below it. It handles the two kinds Limits
// uses and FAILS LOUDLY on a third, so a field of a new kind cannot slip
// through this guard by being silently skipped.
func setField(v, ceiling reflect.Value, mul int64) {
	switch v.Kind() {
	case reflect.Int64:
		c := ceiling.Int()
		if mul == 0 {
			v.SetInt(c - 1)
			return
		}
		v.SetInt(c * mul)
	default:
		panic("scripthost: Limits gained a field of kind " + v.Kind().String() +
			", which TestEveryBoundIsDefaultedAndCeilinged does not know how to drive — teach it rather than skipping the field")
	}
}

func isZero(v reflect.Value) bool { return v.Int() == 0 }

func cmp(a, b reflect.Value) int {
	x, y := a.Int(), b.Int()
	switch {
	case x < y:
		return -1
	case x > y:
		return 1
	}
	return 0
}

// TestTheDurationBoundsStillEncodeNanoseconds pins the unit of the two
// duration fields against their wire names, which is a mistake this package
// has already made: `wall_ms` on a time.Duration meant a hand-written request
// asking for 30 s was enforced at 30 µs.
func TestTheDurationBoundsStillEncodeNanoseconds(t *testing.T) {
	t.Parallel()

	typ := reflect.TypeOf(Limits{})
	durType := reflect.TypeOf(time.Duration(0))
	for i := range typ.NumField() {
		f := typ.Field(i)
		if f.Type != durType {
			continue
		}
		tag := f.Tag.Get("json")
		if tag != wireName(f.Name) {
			t.Errorf("%s is a time.Duration tagged %q: a Duration encodes NANOSECONDS, so the name must say _ns (want %q)",
				f.Name, tag, wireName(f.Name))
		}
	}
}

// wireName is the only spelling a duration bound may carry on the wire.
func wireName(field string) string {
	switch field {
	case "Wall":
		return "wall_ns"
	case "CPU":
		return "cpu_ns"
	}
	return "UNKNOWN_DURATION_FIELD_" + field
}
