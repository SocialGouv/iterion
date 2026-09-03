package model

import (
	"reflect"
	"testing"
)

// ChainHooks is written by hand, one line per callback, so a callback
// added to EventHooks without its matching line is silently DROPPED
// wherever the composition is used (pkg/runview/executor.go replaces the
// store hooks with it when ExtraHooks is configured). That failure
// compiles, passes every per-callback test, and disables the feature —
// three callbacks had accumulated that way. This test closes the CLASS:
// it reflects over the struct rather than naming callbacks, so the next
// one is caught the day it is added.
func TestChainHooksComposesEveryCallback(t *testing.T) {
	typ := reflect.TypeOf(EventHooks{})

	// Build two EventHooks with every func field set to a recorder.
	var calledA, calledB []string
	makeHooks := func(log *[]string) EventHooks {
		var h EventHooks
		v := reflect.ValueOf(&h).Elem()
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			if f.Type.Kind() != reflect.Func {
				t.Fatalf("EventHooks.%s is a %s, not a func — this test assumes an all-callback struct", f.Name, f.Type.Kind())
			}
			name := f.Name
			v.Field(i).Set(reflect.MakeFunc(f.Type, func([]reflect.Value) []reflect.Value {
				*log = append(*log, name)
				return make([]reflect.Value, f.Type.NumOut())
			}))
		}
		return h
	}
	a := makeHooks(&calledA)
	b := makeHooks(&calledB)

	chained := ChainHooks(a, b)
	cv := reflect.ValueOf(chained)
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if cv.Field(i).IsNil() {
			t.Errorf("ChainHooks dropped %s: both sides set it, the composition is nil — add a chainCbN line for it", f.Name)
			continue
		}
		// Both sides must actually run, not just one survive.
		args := make([]reflect.Value, f.Type.NumIn())
		for j := range args {
			args[j] = reflect.New(f.Type.In(j)).Elem()
		}
		cv.Field(i).Call(args)
	}
	if len(calledA) != typ.NumField() || len(calledB) != typ.NumField() {
		t.Errorf("chained callbacks invoked a=%d b=%d of %d — both sides must run for every callback (a: %v, b: %v)",
			len(calledA), len(calledB), typ.NumField(), calledA, calledB)
	}
}
