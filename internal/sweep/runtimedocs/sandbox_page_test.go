package runtimedocs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
	"github.com/SocialGouv/iterion/internal/isolationenv"
	"github.com/SocialGouv/iterion/internal/sweepwalk"
	"github.com/SocialGouv/iterion/pkg/backend/mcp"
	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/platformcfg"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/sandbox/devcontainer"
)

// TestThePromiseAboutTheNeutralisedSubClassIsBoundedToTheEnvRoad is the
// npmrc measurement, kept.
//
// A FILE the image lays down re-plants a neutralised name with NO
// environment variable at all: an image writing
// `node-options=--require=/planted/replant.js` into npm's own global
// config — `/usr/etc/npmrc` in the default image, which
// `npm config get globalconfig` names and no variable points at — had
// `npm run` and `npm exec` children start with
// `NODE_OPTIONS=--require=…` and the planted module re-export the
// credentials, with every neutralisation of round 3 applied.
//
// So the unbounded sentence this guard refuses — "the repository's value
// never reaches a process" — was measurably false, and the page owes the
// bounded one plus the residue. The forbidden string is the sentence
// itself: a page that drifts back to it fails here, which is the only
// thing that makes a prose promise testable.
func TestThePromiseAboutTheNeutralisedSubClassIsBoundedToTheEnvRoad(t *testing.T) {
	page := theSandboxPage(t)

	const unbounded = "for a name of that sub-class the repository's value never reaches a process —"
	if strings.Contains(page, unbounded) {
		t.Errorf("docs/sandbox.md promises %q: an image-laid npmrc re-plants NODE_OPTIONS with no variable at all (measured), so the promise holds for the ENV road and must say so", unbounded)
	}
	for _, required := range []string{
		"the repository's value never reaches a process *by way of the image's `ENV`*",
		"What the ENV road does not close, measured.",
		"still define the container\" auto mode's rule",
	} {
		if !strings.Contains(page, required) {
			t.Errorf("docs/sandbox.md no longer states %q — the bounded promise and the residue beside it are what the npmrc measurement bought", required)
		}
	}
}

// TestThePageStatesTheResidueOfTheStartupCodeSubClass is the prose half of
// the table above. Two forbidden shapes and one required one, because
// doctrine A (Jo's question #63, ARB-121) is a statement about what the
// page may PROMISE:
//
//   - it must not say the neutralised sub-class covers the channels that
//     lead to code — it covers the credential/routing names and nothing
//     else;
//   - it must not promise a bound BY NAME either ("measured to lead to
//     code … whose empty value is inert" was a criterion, and a criterion
//     is a promise about every name that meets it);
//   - it must state the residue as the contract it is: an image a
//     repository chooses is the repository's code.
//
// A prose promise is only testable as the exact words it is made in.
func TestThePageStatesTheResidueOfTheStartupCodeSubClass(t *testing.T) {
	page := theSandboxPage(t)

	for _, forbidden := range []string{
		"and every channel that leads to CODE, or to a configuration a program EXECUTES, at the startup of a process iterion runs there**",
		"and every channel MEASURED to lead to CODE",
	} {
		if strings.Contains(page, forbidden) {
			t.Errorf("docs/sandbox.md promises %q: the screen neutralises the credential/routing sub-class alone, and every channel into code the image ships is reported (ARB-121) — a page that keeps the old sentence tells the operator the ENV road to code is shut", forbidden)
		}
	}
	for _, required := range []string{
		"the **credential and routing** sub-class is **neutralised**",
		"**Every channel into CODE is told, and kept.**",
		"**an image a repository chooses is the repository's code**",
		"the fork road into that position is refused at admission",
	} {
		if !strings.Contains(page, required) {
			t.Errorf("docs/sandbox.md no longer states %q — the one promise the screen keeps, and the residue beside it, are what four rounds of measurement bought", required)
		}
	}
}

// TestThePageStatesTheEngineGuardLiftLineItDraws holds docs/sandbox.md's
// enumeration of the engine namespace's guard lifts to the PRODUCTION
// answer, both sides: the page names each one, and the screen answers for
// it what the page says it answers.
//
// A page that enumerates a partition is a second source of truth unless
// something executes it — and this one draws the line that decides the
// doctrine's residue, so a name that changed side in the table and not on
// the page would leave the operator reading the old line.
func TestThePageStatesTheEngineGuardLiftLineItDraws(t *testing.T) {
	page := theSandboxPage(t)
	for name, wantNeutralize := range map[string]bool{
		// The lifts the screen empties: the launcher's environment, a
		// project `.env`'s reach, the package a consented connection is
		// served from, this screen's own hatch, the host whose request
		// bodies carry the run's real secrets, the auth requirement and
		// the cookie's transport.
		envtrust.DotenvEndpointsOptIn:      true,
		envtrust.EnvDotEnvForce:            true,
		mcp.EnvExpandUntrustedEnv:          true,
		mcp.EnvServerEnvPassthrough:        true,
		platformcfg.EnvCloudEnvPassthrough: true,
		connection.ProjectCatalogEnv:       true,
		runtime.EnvSandboxImageEnvAllow:    true,
		isolationenv.ModelHosts:            true,
		"ITERION_DISABLE_AUTH":             true,
		"ITERION_COOKIE_SECURE":            true,
		"ITERION_COOKIE_DOMAIN":            true,
		// The lifts that stay reported: the isolation tier, the
		// project-trust switches, the loading switches, the
		// browser-facing guards of the server's own pages, the
		// substitution kill-switch and the bot-var pin list.
		isolationenv.SandboxDefault:           false,
		isolationenv.SandboxOverride:          false,
		isolationenv.SandboxFloor:             false,
		isolationenv.HostStateGuard:           false,
		isolationenv.SandboxDefaultImage:      false,
		"ITERION_PI_TRUST_PROJECT":            false,
		"ITERION_OPENCODE_TRUST_PROJECT":      false,
		"ITERION_MCP_AUTOLOAD":                false,
		"ITERION_MCP_HEALTHCHECK":             false,
		"ITERION_SKIP_MCP_HEALTH":             false,
		"ITERION_PLUGINS_ENABLE":              false,
		"ITERION_PLUGINS_DISABLE":             false,
		"ITERION_SKILLS":                      false,
		"ITERION_CLAUDE_CODE_STRICT_MCP":      false,
		"ITERION_SECURITY_HEADERS":            false,
		"ITERION_ALLOWED_ORIGINS":             false,
		"ITERION_REQUIRE_ORIGIN":              false,
		"ITERION_REQUIRE_WS_ORIGIN":           false,
		"ITERION_CANONICAL_REDIRECT":          false,
		"ITERION_STUDIO_INSECURE_NONLOOPBACK": false,
		"ITERION_SANDBOX_TLS_INSPECT":         false,
		platformcfg.EnvBotVarsEnvPins:         false,
	} {
		denied, neutralize := devcontainer.ScreenImageEnvName(name)
		if !denied {
			t.Errorf("ScreenImageEnvName(%q) says the name is outside the class: the page lists it as one the operator is told about", name)
		}
		if neutralize != wantNeutralize {
			side := "reported and keeps the image's value"
			if wantNeutralize {
				side = "overridden with an empty value"
			}
			t.Errorf("ScreenImageEnvName(%q) neutralize = %v: docs/sandbox.md says this one is %s — move the name on the page, or move the row", name, neutralize, side)
		}
		if !strings.Contains(page, "`"+name+"`") {
			t.Errorf("docs/sandbox.md does not name %s: the guard lifts of the engine's namespace are enumerated there, on the side the screen puts them", name)
		}
	}
}

// theSandboxPage reads docs/sandbox.md with its whitespace collapsed, so
// the assertions above are about words and not about where the page wraps
// its lines.
func theSandboxPage(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(sweepwalk.ModuleRoot(t), "docs", "sandbox.md"))
	if err != nil {
		t.Fatalf("read docs/sandbox.md: %v", err)
	}
	return strings.Join(strings.Fields(string(raw)), " ")
}
