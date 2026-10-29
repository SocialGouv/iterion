package secretguard

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/secretsink"
)

// A placeholder that arrives through DATA (a model's output an edge carried
// into a tool body) is the agent sink's: DefuseUngranted rewrites, in such
// a text, the placeholder of every secret kept off the agent into its
// redact-only form, which the materialisers never resolve, and leaves the
// placeholder of a secret that grants the agent — or names no sinks — as
// written.
func TestDefuseUngrantedRewritesThePlaceholderOfASecretKeptOffTheSink(t *testing.T) {
	g := newTestGuard(t,
		Secret{Name: "pw", Value: "pw-value-7f3a9c2e1b", Sinks: []string{secretsink.Command}},
		Secret{Name: "ok", Value: "ok-value-7f3a9c2e1b", Sinks: []string{secretsink.Agent}},
		Secret{Name: "all", Value: "all-value-7f3a9c2e1b"},
	)
	pw, ok, all := defaultPlaceholder("pw"), defaultPlaceholder("ok"), defaultPlaceholder("all")
	in := "printf '%s %s %s' " + pw + " " + ok + " " + all
	got := g.DefuseUngranted(secretsink.Agent, in)
	if strings.Contains(got, pw) {
		t.Fatalf("the command-only secret's placeholder survived in model-carried data: %q", got)
	}
	if !strings.Contains(got, redactOnlyPlaceholder(pw)) {
		t.Fatalf("the defused placeholder is not the redact-only form: %q", got)
	}
	if !strings.Contains(got, ok) || !strings.Contains(got, all) {
		t.Fatalf("a placeholder the agent may resolve was defused: %q", got)
	}
	// Defused, the token resolves nowhere — in the authored materialisers
	// that would otherwise have resolved it under the command grant.
	if out, env := g.MaterializeAuthoredShellEnv("echo '" + redactOnlyPlaceholder(pw) + "'"); strings.Contains(out, "pw-value") || len(env) != 0 {
		t.Fatalf("the defused form resolved: %q %v", out, env)
	}
	if got := g.MaterializeShell(redactOnlyPlaceholder(pw)); strings.Contains(got, "pw-value") {
		t.Fatalf("the defused form resolved in a script body: %q", got)
	}
	var none *Guard
	if none.DefuseUngranted(secretsink.Agent, pw) != pw {
		t.Fatal("a nil guard rewrote the text")
	}
}

// ResolveSecretRefAt renders a value secret's reference only at a sink its
// declaration grants: a prompt-like surface the compiler never walked (a
// preset's prompt, an images: path) cannot hand a model a placeholder — or,
// with placeholders off, a value — the declaration keeps off prompts. A file
// secret renders its path whatever the sink.
func TestResolveSecretRefAtRendersOnlyAtAGrantedSink(t *testing.T) {
	g := newTestGuard(t,
		Secret{Name: "cmd", Value: "cmd-value-7f3a9c2e1b", Sinks: []string{secretsink.Command}},
		Secret{Name: "pr", Value: "pr-value-7f3a9c2e1b", Sinks: []string{secretsink.Prompt, secretsink.Egress}},
		Secret{Name: "all", Value: "all-value-7f3a9c2e1b"},
		Secret{Name: "kube", Value: "kube-value-7f3a9c2e1b", FilePath: "/run/iterion/secrets/kube", Sinks: []string{secretsink.File}},
	)
	if got := g.ResolveSecretRefAt(secretsink.Prompt, "cmd"); got != "" {
		t.Fatalf("a command-only secret rendered at the prompt sink: %q", got)
	}
	if got := g.ResolveSecretRefAt(secretsink.Prompt, "pr"); got != defaultPlaceholder("pr") {
		t.Fatalf("a prompt-granted secret did not render its placeholder: %q", got)
	}
	if got := g.ResolveSecretRefAt(secretsink.Prompt, "all"); got != defaultPlaceholder("all") {
		t.Fatalf("a secret with no sinks did not render at the prompt sink: %q", got)
	}
	if got := g.ResolveSecretRefAt(secretsink.Prompt, "kube"); got != "/run/iterion/secrets/kube" {
		t.Fatalf("a file secret did not render its path: %q", got)
	}
	if got := g.ResolveSecretRefAt(secretsink.Prompt, "undeclared"); got != "" {
		t.Fatalf("an undeclared name rendered: %q", got)
	}
	// With placeholders off, the gate is what keeps the VALUE off a prompt
	// the declaration never granted.
	off := DefaultConfig()
	off.Placeholders = false
	raw := New([]Secret{{Name: "cmd", Value: "cmd-value-7f3a9c2e1b", Sinks: []string{secretsink.Command}}}, off)
	if got := raw.ResolveSecretRefAt(secretsink.Prompt, "cmd"); got != "" {
		t.Fatalf("placeholders off: a command-only secret's VALUE rendered at the prompt sink: %q", got)
	}
	if got := raw.ResolveSecretRef("cmd"); got != "cmd-value-7f3a9c2e1b" {
		t.Fatalf("the sink-less resolver changed: %q", got)
	}
}

// A sink that never materialises leaves the text as written: the proxy,
// the prompt renderer and the file mount have their own paths.
func TestMaterializeForOnlyResolvesAtAMaterialisingSink(t *testing.T) {
	g := newTestGuard(t, Secret{Name: "tok", Value: "tok-value-7f3a9c2e1b", Sinks: []string{secretsink.Prompt, secretsink.Egress}})
	ph := defaultPlaceholder("tok")
	for _, sink := range []string{secretsink.Prompt, secretsink.File, secretsink.Egress} {
		if got := g.MaterializeFor(sink, ph); got != ph {
			t.Errorf("MaterializeFor(%s) resolved: %q", sink, got)
		}
		if g.MaterializesFor(sink) {
			t.Errorf("MaterializesFor(%s) is true for a sink that never materialises", sink)
		}
	}
	all := newTestGuard(t, Secret{Name: "tok", Value: "tok-value-7f3a9c2e1b"})
	if got := all.MaterializeFor(secretsink.Command, ph); !strings.Contains(got, "tok-value") {
		t.Errorf("MaterializeFor(command) did not resolve a secret with no sinks: %q", got)
	}
	// One derivation of the placeholder, shared with the compiler.
	for _, name := range []string{"tok", "_tok", "my-key", "", "a.b/c"} {
		if PlaceholderForName(name) != secretsink.PlaceholderFor(name) {
			t.Errorf("PlaceholderForName(%q) = %q, the leaf derives %q", name, PlaceholderForName(name), secretsink.PlaceholderFor(name))
		}
	}
}
