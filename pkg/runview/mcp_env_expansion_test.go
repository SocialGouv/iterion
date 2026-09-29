package runview

import (
	"strings"
	"testing"

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
// platform's credentials. The expanded value then travels into the container
// as the CLI backends' MCP config — so for a server whose definition comes
// from the workflow's own source tree, this is a read of the launcher's
// environment on behalf of the tree under review.
//
// The matrix is deliberate: three origins × three fields × three reference
// forms × two sources (process environment, and the platform overlay the
// cloud installs). Each axis has been the one that carried the value.
func TestOnlyOperatorServersExpandAgainstTheLauncherEnvironment(t *testing.T) {
	t.Setenv(envCanaryPlain, canaryPlainValue)
	ir.SetEnvOverlay(func(name string) (string, bool) {
		if name == envCanaryOverlay {
			return canaryOverlayVal, true
		}
		return "", false
	})
	t.Cleanup(func() { ir.SetEnvOverlay(nil) })

	for _, tc := range []struct {
		origin   mcp.Origin
		expanded bool
	}{
		{mcp.OriginPlugin, true},
		{mcp.OriginProject, false},
		{mcp.OriginWorkflow, false},
		{mcp.OriginUnknown, false},
	} {
		t.Run(tc.origin.String(), func(t *testing.T) {
			catalog := buildCatalogForTest(t, tc.origin)
			cfg := catalog["s"]

			// Braced, bare and defaulted forms: ExpandWithDefault honours all
			// three, so suppressing only the braced one would leave `$VAR`
			// carrying the value.
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

// The restriction is load-bearing, not absolute: an operator running their own
// repository locally may legitimately keep a token in their shell. The escape
// hatch is greppable and documented, and it restores the previous behaviour
// exactly.
func TestTheOperatorCanOptBackIntoLauncherEnvExpansion(t *testing.T) {
	t.Setenv(envCanaryPlain, canaryPlainValue)
	t.Setenv(mcp.EnvExpandUntrustedEnv, "true")

	cfg := buildCatalogForTest(t, mcp.OriginProject)["s"]
	assertCanary(t, "command", cfg.Command, canaryPlainValue, true)
}

func buildCatalogForTest(t *testing.T, origin mcp.Origin) map[string]*mcp.ServerConfig {
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
	manager, _, err := buildMCPManager(wf, t.TempDir(), iterlog.Nop(), mcp.StartAllServers)
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
