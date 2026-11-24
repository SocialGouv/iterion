package secretguard

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/secretsink"
)

// The two sinks the compiler cannot see are the guard's: `agent` (a
// placeholder a MODEL emits in its own tool input) and `egress` (a request
// on the wire). The authored sinks the executor names — `command`, `params`
// — are honoured here too, from the same declaration. A placeholder of a
// secret that does not grant the sink is left AS WRITTEN at that sink, and
// resolves at the sinks it grants; a secret that names no sinks resolves
// everywhere (the ALL default of ADR-146).

const sinkValue = "s3cr3t-value-7f3a9c"

func sinkGuard(t *testing.T, sinks ...string) (*Guard, string) {
	t.Helper()
	g := newTestGuard(t, Secret{Name: "tok", Value: sinkValue, Sinks: sinks})
	return g, defaultPlaceholder("tok")
}

func TestTheAgentSinkGatesWhatAModelEmits(t *testing.T) {
	t.Run("a secret kept off the agent stays a placeholder in tool input", func(t *testing.T) {
		g, ph := sinkGuard(t, secretsink.Prompt, secretsink.Egress)
		in := "curl -H 'Authorization: Bearer " + ph + "' https://api.example.com"
		if got := g.Materialize(in); got != in {
			t.Fatalf("Materialize resolved a secret whose sinks are [prompt, egress]: %q", got)
		}
		if out, env := g.MaterializeShellEnv("curl -u '" + ph + "' x"); strings.Contains(out, sinkValue) || len(env) != 0 {
			t.Fatalf("a model-proposed shell command resolved a secret kept off the agent: %q %v", out, env)
		}
		if g.MaterializesFor(secretsink.Agent) {
			t.Error("MaterializesFor(agent) is true while no secret grants the agent: a hook would be installed for nothing")
		}
	})
	t.Run("a secret granting the agent resolves", func(t *testing.T) {
		g, ph := sinkGuard(t, secretsink.Agent)
		if got := g.Materialize("x " + ph); !strings.Contains(got, sinkValue) || strings.Contains(got, ph) {
			t.Fatalf("Materialize did not resolve a secret granting agent: %q", got)
		}
		if _, env := g.MaterializeShellEnv("curl -u '" + ph + "' x"); env[ph] != sinkValue {
			t.Fatalf("a model-proposed shell command did not receive a secret granting agent by environment: %v", env)
		}
		if !g.MaterializesFor(secretsink.Agent) {
			t.Error("MaterializesFor(agent) is false while a secret grants it")
		}
	})
	t.Run("no sinks grants every sink", func(t *testing.T) {
		g, ph := sinkGuard(t)
		if got := g.Materialize("x " + ph); !strings.Contains(got, sinkValue) {
			t.Fatalf("a secret with no sinks did not resolve at the agent sink: %q", got)
		}
		// Only a materialising sink ever answers true: prompt, file and
		// egress have their own paths.
		for _, sink := range secretsink.All {
			if got := g.MaterializesFor(sink); got != secretsink.Materialising(sink) {
				t.Errorf("MaterializesFor(%s) = %v for a secret that names no sinks, want %v", sink, got, secretsink.Materialising(sink))
			}
		}
	})
}

func TestTheCommandSinkGatesAnAuthoredBody(t *testing.T) {
	g, ph := sinkGuard(t, secretsink.Prompt, secretsink.Egress)
	if got := g.MaterializeShell("echo '" + ph + "'"); strings.Contains(got, sinkValue) {
		t.Fatalf("MaterializeShell (a script body) resolved a secret kept off the command sink: %q", got)
	}
	if out, env := g.MaterializeAuthoredShellEnv("echo '" + ph + "'"); strings.Contains(out, sinkValue) || len(env) != 0 {
		t.Fatalf("MaterializeAuthoredShellEnv resolved a secret kept off the command sink: %q %v", out, env)
	}

	granted, ph := sinkGuard(t, secretsink.Command)
	if got := granted.MaterializeShell("echo '" + ph + "'"); !strings.Contains(got, sinkValue) {
		t.Fatalf("MaterializeShell did not resolve a secret granting command: %q", got)
	}
	if _, env := granted.MaterializeAuthoredShellEnv("echo '" + ph + "'"); env[ph] != sinkValue {
		t.Fatalf("MaterializeAuthoredShellEnv did not hand a secret granting command to the child by environment: %v", env)
	}
	// The grant is per sink: `command` alone leaves the agent's own tool
	// input unresolved — the secret stays off the model's path.
	if got := granted.Materialize("x " + ph); got != "x "+ph {
		t.Fatalf("a command-only secret resolved at the agent sink: %q", got)
	}
}

func TestTheParamsSinkIsNamedByTheCaller(t *testing.T) {
	g, ph := sinkGuard(t, secretsink.Params)
	if got := g.MaterializeFor(secretsink.Params, "body="+ph); !strings.Contains(got, sinkValue) {
		t.Fatalf("MaterializeFor(params) did not resolve a secret granting params: %q", got)
	}
	if got := g.MaterializeFor(secretsink.Command, "body="+ph); got != "body="+ph {
		t.Fatalf("MaterializeFor(command) resolved a params-only secret: %q", got)
	}
	if got := g.Materialize("body=" + ph); got != "body="+ph {
		t.Fatalf("the agent sink resolved a params-only secret: %q", got)
	}
}

func TestTheEgressSinkGatesTheWire(t *testing.T) {
	off, ph := sinkGuard(t, secretsink.Prompt, secretsink.Agent)
	body := "token=" + ph
	if got := off.MaterializeForHost(body, "api.example.com"); got != body {
		t.Fatalf("the proxy substituted a secret kept off the wire: %q", got)
	}
	on, ph := sinkGuard(t, secretsink.Prompt, secretsink.Egress)
	if got := on.MaterializeForHost("token="+ph, "api.example.com"); !strings.Contains(got, sinkValue) {
		t.Fatalf("the proxy did not substitute a secret granting egress: %q", got)
	}
	// Content DLP is about the VALUE leaving, whatever the grants: a value
	// in a request toward an unapproved host is still refused.
	scoped := newTestGuard(t, Secret{Name: "tok", Value: sinkValue, Hosts: []string{"api.example.com"}, Sinks: []string{secretsink.Prompt}})
	if !scoped.ExfiltratesTo("x="+sinkValue, "evil.example.net") {
		t.Fatal("the DLP gate let a value out toward an unapproved host because its sinks exclude egress")
	}
}

// NamesMaterializableSecret answers the sink-agnostic question — does this
// text carry a placeholder that resolves ANYWHERE — for a caller that would
// hand the text to something that runs it with the value (a compressor).
func TestNamesMaterializableSecretIsSinkAgnostic(t *testing.T) {
	g, ph := sinkGuard(t, secretsink.Command)
	if !g.NamesMaterializableSecret("echo '" + ph + "'") {
		t.Fatal("a command-only secret's placeholder is not reported as materialisable")
	}
	if g.NamesMaterializableSecret("echo hi") {
		t.Fatal("a text with no placeholder is reported as materialisable")
	}
	var none *Guard
	if none.NamesMaterializableSecret(ph) {
		t.Fatal("a nil guard reports a materialisable secret")
	}
}
