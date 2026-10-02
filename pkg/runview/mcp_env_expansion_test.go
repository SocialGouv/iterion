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

// The policy a manager is BUILT with is a prediction, and the prediction is
// wrong on the most ordinary local run there is: `sandbox: auto` is the
// default, so the launch surface predicts a sandbox, and on a host with no
// devcontainer or no container runtime the run then settles WITHOUT one.
//
// Everything the prediction suppressed has to come back. Left stale, an
// untrusted server's `command: ${MY_BIN}` stayed BLANK after the relax, with
// a permanent StartErr — so a project `.mcp.json` that works under
// `--sandbox none` died under the default, as an untyped startup failure
// naming neither the variable nor the rule.
func TestAPredictedSuppressionIsUndoneWhenTheRunSettlesUnsandboxed(t *testing.T) {
	t.Setenv("ITERION_TEST_MCP_BIN", "/usr/bin/true")
	wf := &ir.Workflow{
		Name: "w",
		ResolvedMCPServers: map[string]*ir.MCPServer{
			"repo": {
				Name: "repo", Origin: string(mcp.OriginProject),
				Transport: ir.MCPTransportStdio, Command: "${ITERION_TEST_MCP_BIN}",
			},
		},
	}

	// Built on the restrictive PREDICTION: the variable is dropped and the
	// config is unusable, which is correct while a sandbox is expected.
	m, _, err := buildMCPManager(wf, t.TempDir(), iterlog.Nop(), mcp.StartOperatorServersOnly)
	if err != nil || m == nil {
		t.Fatalf("build: %v", err)
	}
	cfg, ok := m.ServerConfig("repo")
	if !ok || cfg.Command != "" || cfg.StartErr == nil {
		t.Fatalf("premise broken — the prediction must suppress: Command=%q StartErr=%v", cfg.Command, cfg.StartErr)
	}

	// The engine settles: no sandbox. Nothing crosses into a container, so
	// there is nothing left to suppress.
	m.SetStartPolicy(mcp.StartAllServers)

	cfg, ok = m.ServerConfig("repo")
	if !ok {
		t.Fatal("the server left the catalog")
	}
	if cfg.Command != "/usr/bin/true" {
		t.Errorf("Command = %q, want the expanded value back: the suppression was decided by a prediction "+
			"the engine has now contradicted", cfg.Command)
	}
	if cfg.StartErr != nil {
		t.Errorf("StartErr survived the relax, so the server can never start: %v", cfg.StartErr)
	}
}

// And the mirror: tightening must not hand an untrusted server its
// launcher-expanded value. The refresher runs in both directions, so the
// suppression has to be REAPPLIED, not merely recoverable.
func TestTighteningReappliesTheSuppression(t *testing.T) {
	t.Setenv("ITERION_TEST_MCP_BIN", "/usr/bin/true")
	wf := &ir.Workflow{
		Name: "w",
		ResolvedMCPServers: map[string]*ir.MCPServer{
			"repo": {
				Name: "repo", Origin: string(mcp.OriginProject),
				Transport: ir.MCPTransportStdio, Command: "${ITERION_TEST_MCP_BIN}",
			},
		},
	}
	m, _, err := buildMCPManager(wf, t.TempDir(), iterlog.Nop(), mcp.StartAllServers)
	if err != nil || m == nil {
		t.Fatalf("build: %v", err)
	}
	if cfg, _ := m.ServerConfig("repo"); cfg.Command != "/usr/bin/true" {
		t.Fatalf("premise broken — an unsandboxed run expands: %q", cfg.Command)
	}

	m.SetStartPolicy(mcp.StartOperatorServersOnly)

	cfg, _ := m.ServerConfig("repo")
	if cfg.Command != "" {
		t.Errorf("Command = %q — a sandbox settled, so the launcher's value must not travel into the "+
			"container as this server's config", cfg.Command)
	}
}

// An OPERATOR server's config is the operator's own, so it expands either
// way: a policy change must not take their `${VAR}` away.
func TestAnOperatorServerKeepsItsExpansionAcrossAPolicyChange(t *testing.T) {
	t.Setenv("ITERION_TEST_MCP_BIN", "/usr/bin/true")
	wf := &ir.Workflow{
		Name: "w",
		ResolvedMCPServers: map[string]*ir.MCPServer{
			"installed": {
				Name: "installed", Origin: string(mcp.OriginPlugin),
				Transport: ir.MCPTransportStdio, Command: "${ITERION_TEST_MCP_BIN}",
			},
		},
	}
	m, _, err := buildMCPManager(wf, t.TempDir(), iterlog.Nop(), mcp.StartAllServers)
	if err != nil || m == nil {
		t.Fatalf("build: %v", err)
	}
	for _, p := range []mcp.StartPolicy{mcp.StartOperatorServersOnly, mcp.StartPolicyUnknown, mcp.StartAllServers} {
		m.SetStartPolicy(p)
		if cfg, _ := m.ServerConfig("installed"); cfg.Command != "/usr/bin/true" {
			t.Errorf("under %v the operator's own server lost its variable: %q", p, cfg.Command)
		}
	}
}

// The silent shape of the same defect: unusableAfterDroppedRefs only refuses
// a config whose command or url went EMPTY, so a literal `command` with a
// blanked `${VAR}` ARGUMENT carries no StartErr at all. Under the prediction
// that is harmless (the server is refused anyway); after the run settles
// unsandboxed it would START — with a blank credential and nothing, anywhere,
// saying so. This repo's own `.mcp.json` has exactly that shape.
func TestABlankedArgumentIsRestoredWhenTheRunSettlesUnsandboxed(t *testing.T) {
	t.Setenv("ITERION_TEST_MCP_TOKEN", "s3cret-shaped")
	wf := &ir.Workflow{
		Name: "w",
		ResolvedMCPServers: map[string]*ir.MCPServer{
			"repo": {
				Name: "repo", Origin: string(mcp.OriginProject),
				Transport: ir.MCPTransportStdio, Command: "npx",
				Args: []string{"-y", "some-mcp-server", "--access-token=${ITERION_TEST_MCP_TOKEN}"},
			},
		},
	}
	m, _, err := buildMCPManager(wf, t.TempDir(), iterlog.Nop(), mcp.StartOperatorServersOnly)
	if err != nil || m == nil {
		t.Fatalf("build: %v", err)
	}
	cfg, _ := m.ServerConfig("repo")
	if cfg.StartErr != nil {
		t.Fatalf("premise broken: this shape carries no StartErr, which is the whole point: %v", cfg.StartErr)
	}
	if got := cfg.Args[2]; got != "--access-token=" {
		t.Fatalf("premise broken: the prediction must blank the argument, got %q", got)
	}

	m.SetStartPolicy(mcp.StartAllServers)

	cfg, _ = m.ServerConfig("repo")
	if got := cfg.Args[2]; got != "--access-token=s3cret-shaped" {
		t.Errorf("the argument stayed blank after the run settled unsandboxed (%q): the server would start "+
			"with an empty credential, and nothing carries a StartErr for this shape", got)
	}
}

// A catalog is not finished when its `${VAR}` are expanded: buildMCPManager
// then installs each server's OAuth AuthFunc and records a malformed
// `auth:` block as that server's StartErr. A refresher that re-ran the
// EXPANSION alone dropped both — so once the sandbox settled, every OAuth
// server was dialled with no Authorization header (rpc.go installs no
// round-tripper for a nil AuthFunc and no static headers), and a server the
// build had REFUSED became startable, unauthenticated, against an endpoint
// the repository under review declared.
func TestTheSettledPolicyKeepsTheCatalogsAuthWork(t *testing.T) {
	wf := &ir.Workflow{
		Name: "w",
		ResolvedMCPServers: map[string]*ir.MCPServer{
			"installed": {
				Name: "installed", Origin: string(mcp.OriginPlugin), Transport: ir.MCPTransportHTTP,
				URL: "https://example.invalid/mcp",
				Auth: &ir.MCPAuth{Type: "oauth2", AuthURL: "https://example.invalid/a",
					TokenURL: "https://example.invalid/t", ClientID: "cid"},
			},
			"repo": {
				// A malformed block on an untrusted server fails THAT
				// server, and must keep failing it.
				Name: "repo", Origin: string(mcp.OriginWorkflow), Transport: ir.MCPTransportHTTP,
				URL: "https://example.invalid/mcp", Auth: &ir.MCPAuth{Type: "oauth2"},
			},
		},
	}
	m, _, err := buildMCPManager(wf, t.TempDir(), iterlog.Nop(), mcp.StartOperatorServersOnly)
	if err != nil || m == nil {
		t.Fatalf("build: %v", err)
	}
	authed, _ := m.ServerConfig("installed")
	broken, _ := m.ServerConfig("repo")
	if authed.AuthFunc == nil || broken.StartErr == nil {
		t.Fatalf("premise broken: the build must install the AuthFunc and record the bad block: "+
			"AuthFunc!=nil=%v StartErr=%v", authed.AuthFunc != nil, broken.StartErr)
	}

	for _, settled := range []mcp.StartPolicy{mcp.StartAllServers, mcp.StartOperatorServersOnly} {
		m.SetStartPolicy(settled)

		authed, _ = m.ServerConfig("installed")
		if authed.AuthFunc == nil {
			t.Errorf("under %v the OAuth server lost its AuthFunc: it would be dialled anonymously", settled)
		}
		broken, _ = m.ServerConfig("repo")
		if broken.StartErr == nil {
			t.Errorf("under %v the server the build refused for a malformed `auth:` block became startable",
				settled)
		}
	}
}

// The log must carry ONE verdict about a dropped `${VAR}`, and it must be the
// settled one. Two passes over the same catalog used to warn twice — and on
// the ordinary `sandbox: auto` run that degrades to unsandboxed, the first
// warning said the variable was dropped when the second was about to expand
// it. Exactly the class fixed one file over for the degrade event: a sentence
// the next step makes false.
func TestTheDroppedVariableIsWarnedAboutOnce_BySettledVerdict(t *testing.T) {
	t.Setenv("ITERION_TEST_MCP_ONCE", "/usr/bin/true")
	newWorkflow := func() *ir.Workflow {
		return &ir.Workflow{
			Name: "w",
			ResolvedMCPServers: map[string]*ir.MCPServer{
				"repo": {
					Name: "repo", Origin: string(mcp.OriginProject),
					Transport: ir.MCPTransportStdio, Command: "${ITERION_TEST_MCP_ONCE}",
				},
			},
		}
	}
	count := func(buf *strings.Builder) int {
		return strings.Count(buf.String(), "ITERION_TEST_MCP_ONCE")
	}

	t.Run("settles unsandboxed: nothing was dropped, so nothing is said", func(t *testing.T) {
		var buf strings.Builder
		m, _, err := buildMCPManager(newWorkflow(), t.TempDir(),
			iterlog.New(iterlog.LevelDebug, &buf), mcp.StartOperatorServersOnly)
		if err != nil || m == nil {
			t.Fatalf("build: %v", err)
		}
		if n := count(&buf); n != 0 {
			t.Errorf("the prediction warned %d time(s) about a variable the settled pass restores", n)
		}
		m.SetStartPolicy(mcp.StartAllServers)
		if n := count(&buf); n != 0 {
			t.Errorf("the run expands this variable; the log must not claim it was dropped (%d line(s))", n)
		}
	})

	t.Run("settles sandboxed: said exactly once", func(t *testing.T) {
		var buf strings.Builder
		m, _, err := buildMCPManager(newWorkflow(), t.TempDir(),
			iterlog.New(iterlog.LevelDebug, &buf), mcp.StartOperatorServersOnly)
		if err != nil || m == nil {
			t.Fatalf("build: %v", err)
		}
		m.SetStartPolicy(mcp.StartOperatorServersOnly)
		if n := count(&buf); n != 1 {
			t.Errorf("the variable must be named exactly once, got %d line(s):\n%s", n, buf.String())
		}
	})
}

// The emptiness diagnostic used to fire only on the suppression path — so
// the ONE origin the launcher actually starts, an installed plugin's server,
// got an emptied `command` with no StartErr and not one line of log, and the
// launcher then spawned "". That is verbatim the failure this diagnostic
// exists to prevent, unmet for the only origin that reaches a spawn, while
// an untrusted server one line away got the full message.
func TestAnOperatorServerEmptiedByAnUnsetVariableSaysSo(t *testing.T) {
	newWorkflow := func(origin mcp.Origin) *ir.Workflow {
		return &ir.Workflow{
			Name: "w",
			ResolvedMCPServers: map[string]*ir.MCPServer{
				"s": {
					Name: "s", Origin: string(origin), Transport: ir.MCPTransportStdio,
					Command: "${ITERION_TEST_MCP_DEFINITELY_UNSET}",
				},
			},
		}
	}

	for _, tc := range []struct {
		origin mcp.Origin
		// what the message must name, whichever origin it is
		wants []string
	}{
		{mcp.OriginPlugin, []string{"ITERION_TEST_MCP_DEFINITELY_UNSET", "installed plugin", "${VAR:-default}"}},
		{mcp.OriginProject, []string{"ITERION_TEST_MCP_DEFINITELY_UNSET", "not expanded"}},
	} {
		t.Run(string(tc.origin), func(t *testing.T) {
			m, _, err := buildMCPManager(newWorkflow(tc.origin), t.TempDir(),
				iterlog.Nop(), mcp.StartOperatorServersOnly)
			if err != nil || m == nil {
				t.Fatalf("build: %v", err)
			}
			cfg, ok := m.ServerConfig("s")
			if !ok {
				t.Fatal("the server left the catalog")
			}
			if cfg.Command != "" {
				t.Fatalf("premise broken: an unset variable must empty the command, got %q", cfg.Command)
			}
			if cfg.StartErr == nil {
				t.Fatalf("an emptied command must carry its reason, or the launcher spawns \"\" and the " +
					"operator reads \"protocol_or_startup_failure, raw diagnostics withheld\"")
			}
			for _, want := range tc.wants {
				if !strings.Contains(cfg.StartErr.Error(), want) {
					t.Errorf("the reason must name %q: %v", want, cfg.StartErr)
				}
			}
		})
	}
}

// Whether a config was expanded here is decided by the SUPPRESSION — origin
// AND policy AND hatch — not by the origin alone. A run with no sandbox
// expands every origin, so a repository's `.mcp.json` server emptied there
// was being told its config "is not expanded against this process's
// environment", and handed an escape hatch that was already in force: a
// false diagnosis whose remedy does nothing.
func TestTheEmptiedReasonFollowsTheSuppressionNotTheOrigin(t *testing.T) {
	wf := &ir.Workflow{
		Name: "w",
		ResolvedMCPServers: map[string]*ir.MCPServer{
			"repo": {
				Name: "repo", Origin: string(mcp.OriginProject), Transport: ir.MCPTransportStdio,
				Command: "${ITERION_TEST_MCP_DEFINITELY_UNSET}",
			},
		},
	}
	// StartAllServers is the launch surface saying this run is NOT sandboxed,
	// which is exactly when an untrusted origin does expand.
	m, _, err := buildMCPManager(wf, t.TempDir(), iterlog.Nop(), mcp.StartAllServers)
	if err != nil || m == nil {
		t.Fatalf("build: %v", err)
	}
	cfg, ok := m.ServerConfig("repo")
	if !ok {
		t.Fatal("the server left the catalog")
	}
	if cfg.StartErr == nil {
		t.Fatal("premise broken: an unset variable must empty the command and carry its reason")
	}
	got := cfg.StartErr.Error()
	for _, want := range []string{"ITERION_TEST_MCP_DEFINITELY_UNSET", "IS expanded here", "${VAR:-default}"} {
		if !strings.Contains(got, want) {
			t.Errorf("the reason must name %q: %v", want, got)
		}
	}
	// The two marks of the wrong arm: the claim, and the useless remedy.
	if strings.Contains(got, "not expanded") {
		t.Errorf("this run DOES expand the reference; the reason must not claim otherwise: %v", got)
	}
	if strings.Contains(got, mcp.EnvExpandUntrustedEnv) {
		t.Errorf("the hatch is already in force here — advising it is a remedy that changes nothing: %v", got)
	}
}

// `${PORT:-8080}` is not a dropped reference: the author supplied the answer.
// resolveBracedSegment looks the name up BEFORE it falls back, so a defaulted
// reference reaches the recorder indistinguishable from one that resolved to
// nothing — and it was recorded, which made the refusal name variables that
// were never needed and told the operator to set them.
func TestADefaultedReferenceIsNotADroppedReference(t *testing.T) {
	// `command` is emptied by a genuinely unset variable, so the refusal
	// fires and we get to read the names it lists; `args` carries the
	// defaulted one, which must not appear among them.
	newWorkflow := func(origin mcp.Origin) *ir.Workflow {
		return &ir.Workflow{
			Name: "w",
			ResolvedMCPServers: map[string]*ir.MCPServer{
				"s": {
					Name: "s", Origin: string(origin), Transport: ir.MCPTransportStdio,
					Command: "${ITERION_TEST_MCP_DEFINITELY_UNSET}",
					Args:    []string{"--port=${ITERION_TEST_MCP_ALSO_UNSET:-8080}"},
				},
			},
		}
	}

	for _, tc := range []struct {
		name   string
		origin mcp.Origin
		policy mcp.StartPolicy
		// Only the suppression path names each variable in the log; the
		// expanded path reports through StartErr alone.
		warns bool
	}{
		{"suppressed", mcp.OriginProject, mcp.StartOperatorServersOnly, true},
		{"expanded", mcp.OriginPlugin, mcp.StartOperatorServersOnly, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf strings.Builder
			m, _, err := buildMCPManager(newWorkflow(tc.origin), t.TempDir(),
				iterlog.New(iterlog.LevelDebug, &buf), tc.policy)
			if err != nil || m == nil {
				t.Fatalf("build: %v", err)
			}
			cfg, ok := m.ServerConfig("s")
			if !ok {
				t.Fatal("the server left the catalog")
			}
			if cfg.Args[0] != "--port=8080" {
				t.Fatalf("premise broken: the default must answer, got %q", cfg.Args[0])
			}
			if cfg.StartErr == nil {
				t.Fatal("premise broken: the unset variable must still empty the command")
			}
			if strings.Contains(cfg.StartErr.Error(), "ALSO_UNSET") {
				t.Errorf("a reference its author defaulted must not be listed as dropped: %v", cfg.StartErr)
			}
			if !tc.warns {
				return
			}
			// The build pass is deliberately silent — the settled verdict is
			// what speaks. Settle it, or this assertion cannot redden.
			m.SetStartPolicy(tc.policy)
			if !strings.Contains(buf.String(), "DEFINITELY_UNSET") {
				t.Fatalf("premise broken: the settled pass is what names a dropped variable, got:\n%s",
					buf.String())
			}
			if strings.Contains(buf.String(), "ALSO_UNSET") {
				t.Errorf("…nor warned about: %s", buf.String())
			}
		})
	}
}

// expandMCPCatalog is documented as a pure function of (workflow, policy) so
// it can run again when the sandbox settles. Two catalogs sharing one map
// with the IR was the one thing about it that was not.
func TestTheCatalogDoesNotShareItsMapsWithTheIR(t *testing.T) {
	wf := &ir.Workflow{
		Name: "w",
		ResolvedMCPServers: map[string]*ir.MCPServer{
			"s": {
				Name: "s", Origin: string(mcp.OriginPlugin), Transport: ir.MCPTransportHTTP,
				URL:     "https://example.invalid/mcp",
				Headers: map[string]string{"X-A": "1"},
				Env:     map[string]string{"E": "1"},
			},
		},
	}
	build := expandMCPCatalog(wf, mcp.StartAllServers, iterlog.Nop())
	settle := expandMCPCatalog(wf, mcp.StartOperatorServersOnly, iterlog.Nop())

	build["s"].Headers["CANARY"] = "written-by-the-build-pass"
	build["s"].Env["CANARY"] = "written-by-the-build-pass"

	if _, leaked := settle["s"].Headers["CANARY"]; leaked {
		t.Error("the two catalogs share one Headers map; a mutation in either reaches the other and the IR")
	}
	if _, leaked := settle["s"].Env["CANARY"]; leaked {
		t.Error("the two catalogs share one Env map; a mutation in either reaches the other and the IR")
	}
	if _, leaked := wf.ResolvedMCPServers["s"].Headers["CANARY"]; leaked {
		t.Error("the catalog wrote into the workflow's own Headers map")
	}
}

// Two writers reach StartErr: the dropped-`${VAR}` diagnostic, which
// carries a remedy, and the auth verdict, which runs second. Assigning
// blindly replaced the actionable reason with a bare oauth complaint — in
// the `Cause` that travels into the typed refusal and into the run event.
func TestTheRemedyBearingReasonSurvivesTheAuthVerdict(t *testing.T) {
	wf := &ir.Workflow{
		Name: "w",
		ResolvedMCPServers: map[string]*ir.MCPServer{
			"repo": {
				Name: "repo", Origin: string(mcp.OriginWorkflow), Transport: ir.MCPTransportHTTP,
				// Both problems at once: an unexpandable url AND a
				// malformed auth block.
				URL:  "${ITERION_TEST_MCP_ABSENT_URL}",
				Auth: &ir.MCPAuth{Type: "oauth2"},
			},
		},
	}
	for _, policy := range []mcp.StartPolicy{mcp.StartOperatorServersOnly, mcp.StartAllServers} {
		t.Run(policy.String(), func(t *testing.T) {
			m, _, err := buildMCPManager(wf, t.TempDir(), iterlog.Nop(), policy)
			if err != nil || m == nil {
				t.Fatalf("build: %v", err)
			}
			cfg, _ := m.ServerConfig("repo")
			if cfg.StartErr == nil {
				t.Fatal("premise broken: this server cannot start for two reasons; one must be recorded")
			}
			if !strings.Contains(cfg.StartErr.Error(), "ITERION_TEST_MCP_ABSENT_URL") {
				t.Errorf("the reason with a remedy must be the one that survives, got: %v", cfg.StartErr)
			}
		})
	}
}
