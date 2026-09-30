package runview

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
	"github.com/SocialGouv/iterion/pkg/backend/mcp"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

const (
	envCanaryPlain    = "ITERION_TEST_MCP_CANARY_PLAIN"
	envCanaryOverlay  = "ITERION_TEST_MCP_CANARY_OVERLAY"
	canaryPlainValue  = "canary-plain"
	canaryOverlayVal  = "canary-overlay"
	expansionTestHost = "https://example.invalid/"
)

// The expansion of an MCP server's config answers `${VAR}` from the LAUNCHER's
// environment: an operator's shell, or the cloud runner pod holding the
// platform's credentials. Under an ACTIVE sandbox the expanded value then
// travels into the container as the CLI backends' MCP config — so for a
// server whose definition comes from the workflow's own source tree, that is
// a read of the launcher's environment on behalf of the tree under review.
//
// Two axes, because both were wrong once: the ORIGIN (an operator's plugin
// may read it, a repository's `.mcp.json` may not) and the SANDBOX (a run with
// no sandbox already executes the workflow's own tool nodes beside the
// launcher with its whole environment, so suppressing the expansion there
// protects nothing and costs the author their variable).
//
// Within each, three fields × three reference forms × two sources (the
// process environment, and the platform overlay the cloud installs). Each of
// those axes has been the one that carried the value.
func TestOnlyOperatorServersExpandAgainstTheLauncherEnvironment(t *testing.T) {
	t.Setenv(envCanaryPlain, canaryPlainValue)
	ir.SetEnvOverlay(func(name string) (string, bool) {
		if name == envCanaryOverlay {
			return canaryOverlayVal, true
		}
		return "", false
	})
	t.Cleanup(func() { ir.SetEnvOverlay(nil) })

	for _, policy := range []mcp.StartPolicy{mcp.StartOperatorServersOnly, mcp.StartPolicyUnknown} {
		for _, tc := range []struct {
			origin   mcp.Origin
			expanded bool
		}{
			{mcp.OriginPlugin, true},
			{mcp.OriginProject, false},
			{mcp.OriginWorkflow, false},
			{mcp.OriginUnknown, false},
		} {
			t.Run(policy.String()+"/"+tc.origin.String(), func(t *testing.T) {
				cfg := buildCatalogForTest(t, tc.origin, policy)["s"]
				assertCanary(t, "command", cfg.Command, canaryPlainValue, tc.expanded)
				assertCanary(t, "args[0] (braced)", cfg.Args[0], canaryPlainValue, tc.expanded)
				assertCanary(t, "args[1] (bare)", cfg.Args[1], canaryPlainValue, tc.expanded)
				assertCanary(t, "args[2] (overlay)", cfg.Args[2], canaryOverlayVal, tc.expanded)
				assertCanary(t, "url", cfg.URL, canaryPlainValue, tc.expanded)
				// A default keeps working on every origin: the restriction is
				// "do not read this process's environment", not "do not expand".
				if got := cfg.Args[3]; got != "--default=fallback" {
					t.Errorf("args[3]: ${X:-fallback} should resolve to its default, got %q", got)
				}
			})
		}
	}

	// A run the launch surface KNOWS is unsandboxed: everything expands, as it
	// always did. Suppressing it here was a scope error that cost an author
	// their `${VAR}` on `--sandbox none`, and failed as an opaque protocol
	// error rather than saying so.
	t.Run("a run known to be unsandboxed expands every origin", func(t *testing.T) {
		for _, origin := range []mcp.Origin{mcp.OriginProject, mcp.OriginWorkflow, mcp.OriginUnknown} {
			cfg := buildCatalogForTest(t, origin, mcp.StartAllServers)["s"]
			assertCanary(t, origin.String()+" command", cfg.Command, canaryPlainValue, true)
			if cfg.StartErr != nil {
				t.Errorf("%s: nothing was dropped, so nothing is unusable: %v", origin, cfg.StartErr)
			}
		}
	})
}

// A dropped reference can leave the config unusable — an empty stdio command.
// The operator met that as "stdio initialization failed
// (reason=protocol_or_startup_failure, raw diagnostics withheld)", which names
// neither the variable nor the rule. The reason is known here, so it is said
// here.
func TestAConfigEmptiedByTheSuppressionSaysWhy(t *testing.T) {
	t.Setenv(envCanaryPlain, canaryPlainValue)
	cfg := buildCatalogForTest(t, mcp.OriginProject, mcp.StartOperatorServersOnly)["s"]
	if cfg.StartErr == nil {
		t.Skip("this fixture's command does not empty out; see the dedicated case below")
	}
	for _, want := range []string{envCanaryPlain, mcp.EnvExpandUntrustedEnv} {
		if !strings.Contains(cfg.StartErr.Error(), want) {
			t.Errorf("the reason must name %q: %v", want, cfg.StartErr)
		}
	}
}

// And the same, on a config whose command is NOTHING BUT a reference.
func TestAnEmptiedStdioCommandIsRefusedWithItsReason(t *testing.T) {
	t.Setenv(envCanaryPlain, canaryPlainValue)
	wf := &ir.Workflow{ResolvedMCPServers: map[string]*ir.MCPServer{
		"s": {
			Name: "s", Origin: string(mcp.OriginProject), Transport: ir.MCPTransportStdio,
			Command: "${" + envCanaryPlain + "}",
		},
	}}
	manager, _, err := buildMCPManager(wf, t.TempDir(), iterlog.Nop(), mcp.StartOperatorServersOnly)
	if err != nil {
		t.Fatalf("buildMCPManager: %v", err)
	}
	cfg, ok := manager.ServerConfig("s")
	if !ok {
		t.Fatal("the server stays in the catalog")
	}
	if cfg.StartErr == nil {
		t.Fatal("a stdio server whose whole command was a dropped reference cannot start; it must say so")
	}
	if !strings.Contains(cfg.StartErr.Error(), envCanaryPlain) {
		t.Errorf("the reason must name the variable: %v", cfg.StartErr)
	}
	if strings.Contains(cfg.StartErr.Error(), canaryPlainValue) {
		t.Error("by NAME, never by value — the value is what was withheld")
	}
}

// The restriction is load-bearing, not absolute: an operator running their own
// repository locally may legitimately keep a token in their shell. The escape
// hatch is greppable and documented, and it restores the previous behaviour
// exactly.
func TestTheOperatorCanOptBackIntoLauncherEnvExpansion(t *testing.T) {
	t.Setenv(envCanaryPlain, canaryPlainValue)
	t.Setenv(mcp.EnvExpandUntrustedEnv, "true")

	cfg := buildCatalogForTest(t, mcp.OriginProject, mcp.StartOperatorServersOnly)["s"]
	assertCanary(t, "command", cfg.Command, canaryPlainValue, true)
}

func buildCatalogForTest(t *testing.T, origin mcp.Origin, policy mcp.StartPolicy) map[string]*mcp.ServerConfig {
	t.Helper()
	wf := &ir.Workflow{
		ResolvedMCPServers: map[string]*ir.MCPServer{
			"s": {
				Name:      "s",
				Origin:    string(origin),
				Transport: ir.MCPTransportStdio,
				Command:   "/bin/${" + envCanaryPlain + "}",
				Args: []string{
					"--braced=${" + envCanaryPlain + "}",
					"--bare=$" + envCanaryPlain,
					"--overlay=${" + envCanaryOverlay + "}",
					"--default=${ITERION_TEST_MCP_CANARY_UNSET:-fallback}",
				},
				URL: expansionTestHost + "${" + envCanaryPlain + "}",
			},
		},
	}
	manager, _, err := buildMCPManager(wf, t.TempDir(), iterlog.Nop(), policy)
	if err != nil {
		t.Fatalf("buildMCPManager: %v", err)
	}
	cfg, ok := manager.ServerConfig("s")
	if !ok {
		t.Fatal("the server is missing from the manager's catalog")
	}
	return map[string]*mcp.ServerConfig{"s": cfg}
}

// assertCanary checks whether a field carries the launcher's value, naming the
// FIELD and never printing the environment's contents: a test that prints what
// it read from the environment leaks it into CI output the first time someone
// points it at a real variable.
func assertCanary(t *testing.T, field, got, canary string, want bool) {
	t.Helper()
	if has := strings.Contains(got, canary); has != want {
		if want {
			t.Errorf("%s: the launcher's value should have been expanded here", field)
			return
		}
		t.Errorf("%s: carries a value read from the launcher's environment", field)
	}
}

// OAuth preparation walks the whole catalog before the manager exists, and it
// used to return on the first malformed `auth:` block — failing the RUN.
//
// That put a file in the target repository in charge of whether the run
// starts at all, and it fired before any of the paths that are supposed to
// handle an unusable server: the ambient degrade, the typed refusal, the
// node's fallbacks. Whose mistake it is decides the blast radius now.
func TestAMalformedAuthBlockFailsTheRunOnlyWhenItIsTheOperatorsOwn(t *testing.T) {
	malformed := &ir.MCPAuth{Type: "oauth2"} // no URLs: rejected by the broker

	t.Run("a workflow-controlled server fails only itself", func(t *testing.T) {
		wf := &ir.Workflow{ResolvedMCPServers: map[string]*ir.MCPServer{
			"repo": {
				Name: "repo", Origin: string(mcp.OriginProject),
				Transport: ir.MCPTransportHTTP, URL: expansionTestHost, Auth: malformed,
			},
			"fine": {Name: "fine", Origin: string(mcp.OriginProject), Transport: ir.MCPTransportStdio, Command: "/bin/echo"},
		}}
		manager, _, err := buildMCPManager(wf, t.TempDir(), iterlog.Nop(), mcp.StartAllServers)
		if err != nil {
			t.Fatalf("a repository's malformed auth block must not abort the run: %v", err)
		}
		broken, ok := manager.ServerConfig("repo")
		if !ok {
			t.Fatal("the server stays in the catalog")
		}
		if broken.StartErr == nil {
			t.Error("the reason must be recorded on the server, so its own use fails with it")
		}
		if healthy, _ := manager.ServerConfig("fine"); healthy.StartErr != nil {
			t.Errorf("an unrelated server must be untouched: %v", healthy.StartErr)
		}
	})

	t.Run("an operator server still fails the run", func(t *testing.T) {
		wf := &ir.Workflow{ResolvedMCPServers: map[string]*ir.MCPServer{
			"firecrawl": {
				Name: "firecrawl", Origin: string(mcp.OriginPlugin),
				Transport: ir.MCPTransportHTTP, URL: expansionTestHost, Auth: malformed,
			},
		}}
		if _, _, err := buildMCPManager(wf, t.TempDir(), iterlog.Nop(), mcp.StartAllServers); err == nil {
			t.Error("the operator's own malformed config must fail fast, not surface as 401s mid-run")
		}
	})
}

// The prediction, the nine spec literals and their sweep guard are all
// upstream of ONE assignment: the policy has to reach the manager. It did
// not. buildMCPManager took the argument and built the manager without it,
// so every run's manager sat at the zero value — the whole arming layer was
// inert, and nothing was red: the prediction was unit-tested in isolation,
// the gate was tested on managers built by hand, and an unused PARAMETER is
// legal Go that no linter in this repo flags.
//
// This is the witness for the normal path. It asserts the value, not the
// shape, because the shape was right.
func TestBuildMCPManagerArmsTheManagerWithItsPolicyArgument(t *testing.T) {
	for _, want := range []mcp.StartPolicy{
		mcp.StartAllServers,
		mcp.StartOperatorServersOnly,
		mcp.StartPolicyUnknown,
	} {
		t.Run(want.String(), func(t *testing.T) {
			wf := &ir.Workflow{ResolvedMCPServers: map[string]*ir.MCPServer{
				"s": {Name: "s", Origin: string(mcp.OriginProject), Transport: ir.MCPTransportStdio, Command: "/bin/echo"},
			}}
			manager, _, err := buildMCPManager(wf, t.TempDir(), iterlog.Nop(), want)
			if err != nil {
				t.Fatalf("buildMCPManager: %v", err)
			}
			if got := manager.StartPolicy(); got != want {
				t.Errorf("the manager holds %v, want %v — the policy never reached it", got, want)
			}
		})
	}
}

// And the other half of the same wiring: a run with no MCP server at all
// builds no manager, so there is nothing to arm and nothing to refuse.
func TestAWorkflowWithoutMCPServersBuildsNoManager(t *testing.T) {
	manager, broker, err := buildMCPManager(&ir.Workflow{}, t.TempDir(), iterlog.Nop(), mcp.StartOperatorServersOnly)
	if err != nil {
		t.Fatalf("buildMCPManager: %v", err)
	}
	if manager != nil || broker != nil {
		t.Errorf("expected no manager and no broker, got %v / %v", manager, broker)
	}
}

// The warning names an escape hatch. The hatch reads the INHERITED value, and
// a project `.env` is exactly where iterion teaches people to put run
// variables — so an operator who followed the advice got the identical
// warning back, advice included, with nothing changed and nothing said.
func TestTheHatchAdviceKnowsWhereItWasSet(t *testing.T) {
	envtrust.ResetForTest()
	t.Cleanup(envtrust.ResetForTest)
	t.Setenv(envtrust.EnvPlantedNames, "")

	if got := expandHatchAdvice(); !strings.Contains(got, "in your shell") ||
		strings.Contains(got, "does not speak for the operator") {
		t.Errorf("unset: the advice is to set it; got %q", got)
	}

	t.Setenv(mcp.EnvExpandUntrustedEnv, "true")
	envtrust.MarkPlanted(mcp.EnvExpandUntrustedEnv)

	got := expandHatchAdvice()
	if !strings.Contains(got, "project `.env`") || !strings.Contains(got, "export it in your shell") {
		t.Errorf("planted: the advice must say the remedy was applied in a place that does not count; got %q", got)
	}
}
