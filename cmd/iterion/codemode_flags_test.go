package main

import (
	"testing"
	"time"

	"github.com/spf13/pflag"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// The CLI half of the run-level codemode override (#3345): the flags project
// onto the resolver's rung type, and an untouched set projects NIL — a launch
// that expressed nothing must not travel as a declaration.
func TestCodemodeFlagsProjectBoundsOrNothing(t *testing.T) {
	t.Parallel()
	var untouched codemodeFlagSet
	if b := untouched.bounds(); b != nil {
		t.Errorf("an untouched flag set projected %+v, want nil — the launch would read as having chosen", b)
	}

	c := codemodeFlagSet{toolCalls: 256, wall: 90 * time.Second}
	b := c.bounds()
	if b == nil {
		t.Fatal("a flag set with two bounds projected nil")
	}
	if b.ToolCalls != 256 || b.Wall != 90*time.Second {
		t.Errorf("bounds = %+v, want ToolCalls 256 / Wall 90s", b)
	}
	// The fields the flags left at zero stay zero: the resolver's field-by-
	// field rule reads them as "did not choose" against the authored rungs.
	if b.Memory != 0 || b.CPU != 0 || b.ToolResultBytes != 0 {
		t.Errorf("bounds = %+v, want the unset flags to stay zero", b)
	}
	if want := (&ir.CodemodeBounds{ToolCalls: 256, Wall: 90 * time.Second}); *b != *want {
		t.Errorf("bounds = %+v, want %+v", b, want)
	}

	// A negative flag is a typo, not a choice: the CLI names it instead of
	// letting the resolver silently read it as unset.
	neg := codemodeFlagSet{toolCalls: -5}
	if name := neg.negativeField(); name != "--codemode-tool-calls" {
		t.Errorf("negativeField() = %q, want --codemode-tool-calls", name)
	}
	if name := (&codemodeFlagSet{wall: -time.Second}).negativeField(); name != "--codemode-wall" {
		t.Errorf("negativeField() = %q, want --codemode-wall", name)
	}
	if name := (&codemodeFlagSet{toolCalls: 4}).negativeField(); name != "" {
		t.Errorf("a clean set named %q, want silence", name)
	}
}

func TestCodemodeConcurrencyFlagAlone(t *testing.T) {
	t.Parallel()
	var c codemodeFlagSet
	flags := pflag.NewFlagSet("codemode", pflag.ContinueOnError)
	c.register(flags)
	if err := flags.Parse([]string{"--codemode-concurrent-tool-calls=3"}); err != nil {
		t.Fatal(err)
	}
	if got := c.bounds(); got == nil || got.ConcurrentToolCalls != 3 {
		t.Errorf("bounds = %+v, want only ConcurrentToolCalls 3", got)
	}
	if err := flags.Set("codemode-concurrent-tool-calls", "0"); err != nil {
		t.Fatal(err)
	}
	if got := c.bounds(); got != nil {
		t.Errorf("bounds = %+v, want nil for explicit inherit", got)
	}
	if err := flags.Set("codemode-concurrent-tool-calls", "-1"); err != nil {
		t.Fatal(err)
	}
	if got := c.negativeField(); got != "--codemode-concurrent-tool-calls" {
		t.Errorf("negativeField = %q, want concurrency flag", got)
	}
}

func TestCodemodeStoreFlags(t *testing.T) {
	t.Parallel()
	for _, flag := range []string{"codemode-store-entries", "codemode-store-bytes"} {
		var c codemodeFlagSet
		flags := pflag.NewFlagSet("codemode", pflag.ContinueOnError)
		c.register(flags)
		if err := flags.Parse([]string{"--" + flag + "=48"}); err != nil {
			t.Fatal(err)
		}
		got := c.bounds()
		if got == nil || (flag == "codemode-store-entries" && got.StoreEntries != 48) || (flag == "codemode-store-bytes" && got.StoreBytes != 48) {
			t.Errorf("%s bounds = %+v, want the quota carried on its own", flag, got)
		}
		if err := flags.Set(flag, "-1"); err != nil {
			t.Fatal(err)
		}
		if got := c.negativeField(); got != "--"+flag {
			t.Errorf("negativeField() = %q, want --%s", got, flag)
		}
	}
}
