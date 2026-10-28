package model

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// TestChainHooksForwardsEveryField is the structural guard behind
// ChainHooks: it walks EventHooks by reflection, installs a recorder on
// BOTH halves of one field at a time, fires the chained result, and
// requires exactly two calls in order (a then b).
//
// Field-by-field composition is the kind of code that silently stops
// covering the struct it composes the moment a field is added — a hook a
// caller registered then never fires, with no build error and no test
// failure anywhere. Enumerating the fields from the type itself is what
// makes that omission impossible: a new EventHooks field fails here until
// ChainHooks forwards it.
// zeroResults is what a reflected recorder returns: one zero value per
// result the field declares. Most hooks return nothing, but a hook whose
// verdict reaches the node returns an error (OnToolNodeDeliverables) —
// and reflect.MakeFunc panics on a result count that does not match, so
// the walk must build the list from the field's own type rather than
// assume the void shape.
func zeroResults(typ reflect.Type) []reflect.Value {
	out := make([]reflect.Value, typ.NumOut())
	for i := range out {
		out[i] = reflect.Zero(typ.Out(i))
	}
	return out
}

func TestChainHooksForwardsEveryField(t *testing.T) {
	typ := reflect.TypeOf(EventHooks{})
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		t.Run(field.Name, func(t *testing.T) {
			if field.Type.Kind() != reflect.Func {
				t.Fatalf("EventHooks.%s is a %s, not a func — extend this guard "+
					"so the new field's composition is covered too",
					field.Name, field.Type.Kind())
			}
			var order []string
			recorder := func(side string) reflect.Value {
				return reflect.MakeFunc(field.Type, func([]reflect.Value) []reflect.Value {
					order = append(order, side)
					return zeroResults(field.Type)
				})
			}
			a := reflect.New(typ).Elem()
			a.Field(i).Set(recorder("a"))
			b := reflect.New(typ).Elem()
			b.Field(i).Set(recorder("b"))

			chained := ChainHooks(a.Interface().(EventHooks), b.Interface().(EventHooks))
			fn := reflect.ValueOf(chained).Field(i)
			if fn.IsNil() {
				t.Fatalf("ChainHooks dropped EventHooks.%s: both halves registered a "+
					"callback and the composed hooks carry none", field.Name)
			}
			args := make([]reflect.Value, field.Type.NumIn())
			for j := range args {
				args[j] = reflect.Zero(field.Type.In(j))
			}
			fn.Call(args)

			if len(order) != 2 || order[0] != "a" || order[1] != "b" {
				t.Errorf("EventHooks.%s fired %v, want [a b]", field.Name, order)
			}
		})
	}
}

// TestChainHooksJoinsTheDeliverablesVerdict covers what the reflection
// walks above cannot see: OnToolNodeDeliverables is the one hook whose
// RESULT matters, and composing two of them must not swallow it.
//
// Both sides run even when the first lost a required file — the second
// registrant's own files still have to be handed over — and the verdict
// of either reaches the caller.
func TestChainHooksJoinsTheDeliverablesVerdict(t *testing.T) {
	lost := errors.New("report.md was not delivered")

	cases := []struct {
		name          string
		aErr, bErr    error
		wantRan       []string
		wantNonNilErr bool
	}{
		{"the first side loses one", lost, nil, []string{"a", "b"}, true},
		{"the second side loses one", nil, lost, []string{"a", "b"}, true},
		{"both deliver", nil, nil, []string{"a", "b"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var ran []string
			side := func(name string, err error) func(string, string) error {
				return func(string, string) error {
					ran = append(ran, name)
					return err
				}
			}
			chained := ChainHooks(
				EventHooks{OnToolNodeDeliverables: side("a", tc.aErr)},
				EventHooks{OnToolNodeDeliverables: side("b", tc.bErr)},
			)
			err := chained.OnToolNodeDeliverables("node", "output")

			if strings.Join(ran, ",") != strings.Join(tc.wantRan, ",") {
				t.Errorf("sides that ran = %v, want %v — a lost file must not "+
					"cancel the other registrant's delivery", ran, tc.wantRan)
			}
			if (err != nil) != tc.wantNonNilErr {
				t.Fatalf("composed verdict = %v, want non-nil = %v", err, tc.wantNonNilErr)
			}
			if tc.wantNonNilErr && !errors.Is(err, lost) {
				t.Errorf("composed verdict %v does not carry the loss it was handed", err)
			}
		})
	}
}

// TestChainHooksKeepsTheLoneSideOfEveryField pins the other half of the
// contract: when only one side registered a callback, the composed hooks
// carry it. A field ChainHooks forgets is invisible to the both-sides
// walk above whenever the omission is a whole-field drop, so this asserts
// the single-side path per field as well.
func TestChainHooksKeepsTheLoneSideOfEveryField(t *testing.T) {
	typ := reflect.TypeOf(EventHooks{})
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if field.Type.Kind() != reflect.Func {
			t.Fatalf("EventHooks.%s is a %s, not a func — extend this guard",
				field.Name, field.Type.Kind())
		}
		for _, side := range []string{"a", "b"} {
			t.Run(field.Name+"/"+side, func(t *testing.T) {
				calls := 0
				hooks := reflect.New(typ).Elem()
				hooks.Field(i).Set(reflect.MakeFunc(field.Type, func([]reflect.Value) []reflect.Value {
					calls++
					return zeroResults(field.Type)
				}))
				var chained EventHooks
				if side == "a" {
					chained = ChainHooks(hooks.Interface().(EventHooks), EventHooks{})
				} else {
					chained = ChainHooks(EventHooks{}, hooks.Interface().(EventHooks))
				}
				fn := reflect.ValueOf(chained).Field(i)
				if fn.IsNil() {
					t.Fatalf("ChainHooks dropped EventHooks.%s registered on side %s",
						field.Name, side)
				}
				args := make([]reflect.Value, field.Type.NumIn())
				for j := range args {
					args[j] = reflect.Zero(field.Type.In(j))
				}
				fn.Call(args)
				if calls != 1 {
					t.Errorf("EventHooks.%s (side %s) fired %d times, want 1", field.Name, side, calls)
				}
			})
		}
	}
}
