package rewrite

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/plugin"
)

func TestParseModeAndResolve(t *testing.T) {
	if ParseMode("ON") != On || ParseMode("ultra") != Ultra || ParseMode("") != Off || ParseMode("true") != On {
		t.Fatal("ParseMode mapping wrong")
	}
	// Tool node: node-only opt-in; override can force-off but never force-on.
	if got := ResolveToolNode("on", ""); got != Off {
		t.Errorf("override on must NOT force-enable a tool node, got %v", got)
	}
	if got := ResolveToolNode("off", "on"); got != Off {
		t.Errorf("override off must kill the node, got %v", got)
	}
	if got := ResolveToolNode("", "ultra"); got != Ultra {
		t.Errorf("tool node honours its own field, got %v", got)
	}
}

// fakeRewriter writes an executable script that echoes a rewritten command and
// returns a spec pointing at it.
func fakeRewriter(t *testing.T, body string, applyExit []int, modes map[string]plugin.ModeSpec) plugin.RewriterSpec {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "fakerw")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("write fake: %v", err)
	}
	return plugin.RewriterSpec{
		ID:     "fake",
		Locate: plugin.LocateSpec{Paths: []string{bin}},
		Invoke: plugin.InvokeSpec{Argv: []string{"rewrite", "{{command}}"}, ApplyExitCodes: applyExit, Modes: modes},
	}
}

func TestResolveWithDefault(t *testing.T) {
	// All unset → the default (opt-out On for LLM nodes when chain available).
	if got := ResolveWithDefault("", "", "", "", On); got != On {
		t.Errorf("all unset should fall back to default On, got %v", got)
	}
	if got := ResolveWithDefault("", "", "", "", Off); got != Off {
		t.Errorf("all unset with Off default → Off, got %v", got)
	}
	// An explicit off at ANY level beats the On default (global + per-run kill).
	if got := ResolveWithDefault("off", "", "", "", On); got != Off {
		t.Errorf("run override off must win over On default, got %v", got)
	}
	if got := ResolveWithDefault("", "", "", "off", On); got != Off {
		t.Errorf("ITERION_COMPRESS=off must win over On default, got %v", got)
	}
	if got := ResolveWithDefault("", "off", "", "", On); got != Off {
		t.Errorf("node off must win over On default, got %v", got)
	}
	// Explicit ultra anywhere is honoured.
	if got := ResolveWithDefault("", "", "ultra", "", On); got != Ultra {
		t.Errorf("workflow ultra honoured, got %v", got)
	}
}

func TestResolveWithDefaultSourced(t *testing.T) {
	cases := []struct {
		name                              string
		override, node, workflow, envDflt string
		def                               Mode
		wantMode                          Mode
		wantSource                        string
	}{
		{"all unset falls to default", "", "", "", "", On, On, "default"},
		{"override wins over everything", "off", "on", "ultra", "on", On, Off, "run_override"},
		{"node wins below override", "", "ultra", "on", "off", Off, Ultra, "node"},
		{"workflow wins below node", "", "", "on", "off", Off, On, "workflow"},
		{"env wins below workflow", "", "", "", "ultra", Off, Ultra, "env"},
		{"whitespace-only level is unset", "  ", "\t", "", "on", Off, On, "env"},
		{"unparsable non-empty level still wins (as off)", "", "banana", "ultra", "", On, Off, "node"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mode, source := ResolveWithDefaultSourced(tc.override, tc.node, tc.workflow, tc.envDflt, tc.def)
			if mode != tc.wantMode || source != tc.wantSource {
				t.Fatalf("got (%v, %q), want (%v, %q)", mode, source, tc.wantMode, tc.wantSource)
			}
		})
	}
}

func TestChainRewriteApplies(t *testing.T) {
	// Script: print "fake <the command arg>" (arg $2 is the {{command}}).
	spec := fakeRewriter(t, `printf 'fake %s' "$2"`, []int{0}, nil)
	chain := NewChain([]plugin.RewriterSpec{spec})
	if !chain.Available() {
		t.Fatal("chain should be available (fake binary exists)")
	}
	got, changed := chain.Rewrite(context.Background(), On, "git status")
	if !changed || got != "fake git status" {
		t.Fatalf("rewrite = %q changed=%v, want %q", got, changed, "fake git status")
	}
	// Off is a no-op.
	if g, c := chain.Rewrite(context.Background(), Off, "git status"); c || g != "git status" {
		t.Errorf("Off must passthrough, got %q changed=%v", g, c)
	}
}

func TestChainRewriteNonApplyExitPassesThrough(t *testing.T) {
	// Script exits 1 (no equivalent) — must fall back to the original command.
	spec := fakeRewriter(t, `printf 'fake %s' "$2"; exit 1`, []int{0, 3}, nil)
	chain := NewChain([]plugin.RewriterSpec{spec})
	got, changed := chain.Rewrite(context.Background(), On, "git status")
	if changed || got != "git status" {
		t.Fatalf("exit 1 must passthrough, got %q changed=%v", got, changed)
	}
}

func TestUltraInjectFlag(t *testing.T) {
	spec := fakeRewriter(t, `printf 'rtk %s' "$2"`, []int{0},
		map[string]plugin.ModeSpec{"ultra": {InjectFlag: "--ultra-compact"}})
	chain := NewChain([]plugin.RewriterSpec{spec})
	got, changed := chain.Rewrite(context.Background(), Ultra, "git status")
	if !changed || got != "rtk --ultra-compact git status" {
		t.Fatalf("ultra inject = %q, want %q", got, "rtk --ultra-compact git status")
	}
}

func TestChainComposesTwoRewriters(t *testing.T) {
	// First prefixes "a:", second prefixes "b:" → sequential composition.
	a := fakeRewriter(t, `printf 'a:%s' "$2"`, []int{0}, nil)
	b := fakeRewriter(t, `printf 'b:%s' "$2"`, []int{0}, nil)
	chain := NewChain([]plugin.RewriterSpec{a, b})
	got, changed := chain.Rewrite(context.Background(), On, "x")
	if !changed || got != "b:a:x" {
		t.Fatalf("chain compose = %q, want %q", got, "b:a:x")
	}
}
