package model

import (
	"reflect"
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
					return nil
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
					return nil
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
