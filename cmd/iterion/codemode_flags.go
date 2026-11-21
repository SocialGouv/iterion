package main

import (
	"time"

	"github.com/spf13/pflag"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// codemodeFlagSet carries the run-level codemode override flags (CM-5e,
// #3345), shared by run, resume and the remote launch: the mode, and one
// flag per bound the launch may raise — those the `codemode:` block
// declares, with its orthography (durations parse as Go durations, sizes and
// counts are raw integers). A zero/empty flag means "this launch did not
// choose" and the authored value stands; every value still passes
// scripthost.Clamp at spawn (ceiling, memory floor, OnClamp report), so a
// flag can warn but never bypass a bound by asking.
type codemodeFlagSet struct {
	codemode            string
	wall                time.Duration
	cpu                 time.Duration
	memory              int64
	scriptBytes         int64
	resultBytes         int64
	toolCalls           int64
	concurrentToolCalls int64
	toolResultBytes     int64
}

func (c *codemodeFlagSet) register(f *pflag.FlagSet) {
	f.StringVar(&c.codemode, "codemode", "", "Run-level codemode override (ADR-165): \"on\" hands agent/judge nodes on claw one extra tool whose input is a program, \"off\" keeps it away even where the author declared it. Empty inherits the workflow/node codemode: DSL — there is deliberately no environment rung (ADR-132 §1). A backend that serves no codemode tool refuses at dispatch; the cost preview screens that before the run.")
	f.DurationVar(&c.wall, "codemode-wall", 0, "Raise the codemode wall-clock bound for this run, e.g. 60s, 5m (0 = inherit the authored bound). Still capped by the platform ceiling; the clamp report says so when it bites.")
	f.DurationVar(&c.cpu, "codemode-cpu", 0, "Raise the codemode CPU-seconds bound for this run (0 = inherit; rounded up to whole seconds by the kernel's unit)")
	f.Int64Var(&c.memory, "codemode-memory", 0, "Raise the codemode memory bound for this run, in bytes (0 = inherit; the 64 MiB floor still applies below it)")
	f.Int64Var(&c.scriptBytes, "codemode-script-bytes", 0, "Raise the codemode script-size bound for this run, in bytes (0 = inherit)")
	f.Int64Var(&c.resultBytes, "codemode-result-bytes", 0, "Raise the codemode result-size bound for this run, in bytes (0 = inherit)")
	f.Int64Var(&c.toolCalls, "codemode-tool-calls", 0, "Raise the codemode tool-call budget for this run (0 = inherit). This is the bound the model is told to ask the operator to raise when a script spends it.")
	f.Int64Var(&c.concurrentToolCalls, "codemode-concurrent-tool-calls", 0, "Override the number of codemode tool calls in flight per program (0 = inherit; default 1, platform ceiling 4)")
	f.Int64Var(&c.toolResultBytes, "codemode-tool-result-bytes", 0, "Raise the per-tool-result bound handed to codemode programs for this run, in bytes (0 = inherit)")
}

// negativeField names the first flag set below zero, or "". A negative bound
// is a typo, not a choice: the resolver's pick rule would silently read it
// as unset, so the CLI refuses it where it validates the mode instead of
// silently inverting an operator input into an inherit.
func (c *codemodeFlagSet) negativeField() string {
	for _, f := range []struct {
		name string
		v    int64
	}{
		{"--codemode-tool-calls", c.toolCalls},
		{"--codemode-concurrent-tool-calls", c.concurrentToolCalls},
		{"--codemode-memory", c.memory},
		{"--codemode-script-bytes", c.scriptBytes},
		{"--codemode-result-bytes", c.resultBytes},
		{"--codemode-tool-result-bytes", c.toolResultBytes},
	} {
		if f.v < 0 {
			return f.name
		}
	}
	if c.wall < 0 {
		return "--codemode-wall"
	}
	if c.cpu < 0 {
		return "--codemode-cpu"
	}
	return ""
}

// bounds projects the flags onto the resolver's rung type. All-zero is nil:
// the launch expressed nothing, and a zero struct must not travel as a
// declaration.
func (c *codemodeFlagSet) bounds() *ir.CodemodeBounds {
	if c.wall <= 0 && c.cpu <= 0 && c.memory <= 0 && c.scriptBytes <= 0 &&
		c.resultBytes <= 0 && c.toolCalls <= 0 && c.concurrentToolCalls <= 0 && c.toolResultBytes <= 0 {
		return nil
	}
	return &ir.CodemodeBounds{
		Wall:                c.wall,
		CPU:                 c.cpu,
		Memory:              c.memory,
		ScriptBytes:         c.scriptBytes,
		ResultBytes:         c.resultBytes,
		ToolCalls:           c.toolCalls,
		ConcurrentToolCalls: c.concurrentToolCalls,
		ToolResultBytes:     c.toolResultBytes,
	}
}
