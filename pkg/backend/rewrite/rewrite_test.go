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
	// Precedence: override > node > workflow > env.
	if got := Resolve("", "off", "on", ""); got != Off {
		t.Errorf("node off must win over workflow on, got %v", got)
	}
	if got := Resolve("ultra", "off", "", ""); got != Ultra {
		t.Errorf("override must win, got %v", got)
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
