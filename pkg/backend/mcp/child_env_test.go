package mcp

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
)

const (
	// childEnvMode is declared in the server's own `env:`, so the helper
	// process receives it whatever the ambient environment does.
	childEnvMode   = "ITERION_TEST_MCP_CHILD_ENV"
	childEnvPrefix = "CHILDENV\t"
	// plantedLoader and plantedRegistry are the two names the ticket names:
	// what an npx-based server loads, and where it fetches from.
	plantedLoader   = "NODE_OPTIONS"
	plantedRegistry = "npm_config_registry"
)

// TestMCPChildEnvHelperProcess prints the environment the parent handed it,
// one `NAME=value` per line behind childEnvPrefix, and is a no-op in a normal
// test run.
func TestMCPChildEnvHelperProcess(t *testing.T) {
	if os.Getenv(childEnvMode) != "print" {
		t.Skip("helper process")
	}
	for _, kv := range os.Environ() {
		fmt.Println(childEnvPrefix + kv)
	}
}

// TestAStdioServerDoesNotInheritWhatAProjectDotEnvPlanted drives the REAL
// child: buildTransport composes its environment, exec delivers it, and the
// child prints what it actually received.
func TestAStdioServerDoesNotInheritWhatAProjectDotEnvPlanted(t *testing.T) {
	envtrust.ResetForTest()
	t.Cleanup(envtrust.ResetForTest)
	t.Setenv(envtrust.EnvPlantedNames, "")
	t.Setenv(plantedLoader, "--require /repo/under/review/pwn.js")
	t.Setenv(plantedRegistry, "https://registry.invalid")
	envtrust.MarkPlanted(plantedLoader, plantedRegistry)

	child := childEnvOfRealServer(t, map[string]string{
		childEnvMode:             "print",
		"MCP_CHILD_ENV_DECLARED": "declared-by-the-operator",
	})

	for _, name := range []string{plantedLoader, plantedRegistry} {
		if v, ok := child[name]; ok {
			t.Errorf("the child inherited %s=%q, planted by a project .env", name, v)
		}
	}
	if got := child["MCP_CHILD_ENV_DECLARED"]; got != "declared-by-the-operator" {
		t.Errorf("declared env value: got %q, want the operator's", got)
	}
	// The npx precondition: a node process without these does not start.
	for _, name := range []string{"PATH", "HOME"} {
		if _, ok := child[name]; !ok {
			t.Errorf("the child lost %s; an npx-based operator server cannot start", name)
		}
	}
}

// TestEveryStdioChildGetsAnExplicitEnvironment covers the other half of the
// class: with an empty `env:` the old code left cmd.Env nil, and Go hands a
// nil-Env child the whole parent environment — the same leak by a different
// path.
func TestEveryStdioChildGetsAnExplicitEnvironment(t *testing.T) {
	envtrust.ResetForTest()
	t.Cleanup(envtrust.ResetForTest)
	t.Setenv(envtrust.EnvPlantedNames, "")
	t.Setenv(plantedLoader, "--require /repo/under/review/pwn.js")
	envtrust.MarkPlanted(plantedLoader)

	client := newSDKClient(&ServerConfig{
		Name: "no-declared-env", Transport: TransportStdio, Command: os.Args[0],
	}, clientInfo{Name: "test", Version: "1"}, nil, nil)
	transport, err := client.buildTransport()
	if err != nil {
		t.Fatalf("buildTransport: %v", err)
	}
	cmd := transport.(*diagnosticCommandTransport).Command
	if cmd.Env == nil {
		t.Fatal("cmd.Env is nil: exec inherits the whole parent environment")
	}
	for _, kv := range cmd.Env {
		if strings.HasPrefix(kv, plantedLoader+"=") {
			t.Errorf("a server with no declared env still received %q", kv)
		}
	}
}

func TestTheAmbientEnvironmentNeedsTheOperatorToNameIt(t *testing.T) {
	const ambient = "MCP_CHILD_ENV_AMBIENT"
	for _, tc := range []struct {
		name        string
		knob        string
		knobPlanted bool
		wantAmbient bool
		wantInfo    string
		wantWarning string
	}{
		{name: "unnamed is dropped"},
		{name: "named passes", knob: ambient, wantAmbient: true, wantInfo: ambient},
		{name: "all restores inheritance", knob: "all", wantAmbient: true, wantWarning: "whole environment"},
		{
			// Honoured wherever it appears: reading it as the name of a
			// variable called `all` gives less than the operator asked for.
			name: "all inside a list still means all", knob: "all,SOMETHING_ELSE",
			wantAmbient: true, wantWarning: "whole environment",
		},
		{
			// The knob is the OPERATOR's: read live, a `.env` in the
			// repository under review would widen its own reach.
			name: "a planted knob widens nothing", knob: ambient, knobPlanted: true,
		},
		{
			name: "a token that is not a name is said, not swallowed",
			knob: "NODE_OPTIONS=--require /x", wantWarning: "not variable names",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			envtrust.ResetForTest()
			t.Cleanup(envtrust.ResetForTest)
			t.Setenv(envtrust.EnvPlantedNames, "")
			t.Setenv(ambient, "from-the-operators-shell")
			if tc.knob != "" {
				t.Setenv(EnvServerEnvPassthrough, tc.knob)
			}
			if tc.knobPlanted {
				envtrust.MarkPlanted(EnvServerEnvPassthrough)
			}

			dec := stdioChildEnv(nil)
			env := envMapOf(dec.Env)
			if _, ok := env[ambient]; ok != tc.wantAmbient {
				t.Errorf("%s present=%v, want %v", ambient, ok, tc.wantAmbient)
			}
			if tc.wantInfo != "" && !strings.Contains(dec.Info, tc.wantInfo) {
				t.Errorf("info %q does not name %s", dec.Info, tc.wantInfo)
			}
			if tc.wantInfo == "" && dec.Info != "" {
				t.Errorf("unexpected info %q", dec.Info)
			}
			if tc.wantWarning == "" && dec.Warning != "" {
				t.Errorf("unexpected warning %q", dec.Warning)
			}
			if tc.wantWarning != "" && !strings.Contains(dec.Warning, tc.wantWarning) {
				t.Errorf("warning %q does not say %q", dec.Warning, tc.wantWarning)
			}
		})
	}
}

// TestAnAllowlistedNameIsReadAsInherited pins the provenance of the allowlist
// itself: PATH chooses which executable a child's own subprocesses are, so a
// planted value must reach no child — it reads as unset, and the name is
// absent rather than repository-chosen.
func TestAnAllowlistedNameIsReadAsInherited(t *testing.T) {
	envtrust.ResetForTest()
	t.Cleanup(envtrust.ResetForTest)
	t.Setenv(envtrust.EnvPlantedNames, "")
	t.Setenv("PATH", "/repo/under/review/bin")
	t.Setenv("HOME", "/home/operator")
	envtrust.MarkPlanted("PATH")

	env := envMapOf(stdioChildEnv(nil).Env)
	if v, ok := env["PATH"]; ok {
		t.Errorf("the child received a planted PATH=%q", v)
	}
	if got := env["HOME"]; got != "/home/operator" {
		t.Errorf("HOME: got %q, want the operator's", got)
	}
}

// TestADeclaredValueWinsOverTheAllowlist keeps the precedence the server's
// own `env:` had: it is the operator's statement about this server.
func TestADeclaredValueWinsOverTheAllowlist(t *testing.T) {
	envtrust.ResetForTest()
	t.Cleanup(envtrust.ResetForTest)
	t.Setenv(envtrust.EnvPlantedNames, "")
	t.Setenv("TMPDIR", "/tmp/launcher")

	env := envMapOf(stdioChildEnv(map[string]string{"TMPDIR": "/tmp/declared"}).Env)
	if got := env["TMPDIR"]; got != "/tmp/declared" {
		t.Errorf("TMPDIR: got %q, want the declared value", got)
	}
}

// TestThePassthroughNoticeSurvivesANilLogger exercises the log path the
// operator's own widening takes, on the client every test and every host that
// builds one directly has: no logger at all.
func TestThePassthroughNoticeSurvivesANilLogger(t *testing.T) {
	const ambient = "MCP_CHILD_ENV_AMBIENT"
	envtrust.ResetForTest()
	t.Cleanup(envtrust.ResetForTest)
	t.Setenv(envtrust.EnvPlantedNames, "")
	t.Setenv(ambient, "from-the-operators-shell")
	t.Setenv(EnvServerEnvPassthrough, ambient+",NODE_OPTIONS=--require /x")

	client := newSDKClient(&ServerConfig{
		Name: "nil-logger", Transport: TransportStdio, Command: os.Args[0],
	}, clientInfo{Name: "test", Version: "1"}, nil, nil)
	transport, err := client.buildTransport()
	if err != nil {
		t.Fatalf("buildTransport: %v", err)
	}
	cmd := transport.(*diagnosticCommandTransport).Command
	if got := envMapOf(cmd.Env)[ambient]; got != "from-the-operators-shell" {
		t.Errorf("%s: got %q, want the operator's value", ambient, got)
	}
}

// childEnvOfRealServer starts the helper process through buildTransport and
// returns the environment the child itself reports.
func childEnvOfRealServer(t *testing.T, declared map[string]string) map[string]string {
	t.Helper()
	client := newSDKClient(&ServerConfig{
		Name: "child-env-test", Transport: TransportStdio, Command: os.Args[0],
		Args: []string{"-test.run=^TestMCPChildEnvHelperProcess$"},
		Env:  declared,
	}, clientInfo{Name: "test", Version: "1"}, nil, nil)
	transport, err := client.buildTransport()
	if err != nil {
		t.Fatalf("buildTransport: %v", err)
	}
	cmd := transport.(*diagnosticCommandTransport).Command
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helper process: %v (output %q)", err, out)
	}
	child := map[string]string{}
	var printed int
	for _, line := range strings.Split(string(out), "\n") {
		kv, ok := strings.CutPrefix(strings.TrimSuffix(line, "\r"), childEnvPrefix)
		if !ok {
			continue
		}
		printed++
		if name, value, cut := strings.Cut(kv, "="); cut {
			child[name] = value
		}
	}
	if printed == 0 {
		t.Fatalf("the helper process printed no environment: %q", out)
	}
	return child
}

func envMapOf(env []string) map[string]string {
	out := map[string]string{}
	for _, kv := range env {
		if name, value, ok := strings.Cut(kv, "="); ok {
			out[name] = value
		}
	}
	return out
}
