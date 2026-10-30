package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
)

// routingWitnessNames is the witness table: one route of every family the
// loader's class covers, each read somewhere in this tree — provider
// endpoints, the legacy OpenAI name, other endpoints, hosts, proxies (the
// lowercase proxy is a distinct NAME on Linux and travels on its own line).
// The doctrine (ARB-122) is the loader's to close: the file plants no route
// unless the operator's own environment opted in.
var routingWitnessNames = []string{
	// provider endpoints
	"OPENAI_BASE_URL", "ANTHROPIC_BASE_URL", "MOONSHOT_BASE_URL", "OPENAI_COMPATIBLE_BASE_URL",
	// the legacy OpenAI name
	"OPENAI_API_BASE",
	// other endpoints
	"AZURE_OPENAI_ENDPOINT", "OTEL_EXPORTER_OTLP_ENDPOINT", "ITERION_S3_ENDPOINT",
	"ITERION_SANDBOX_PROXY_ENDPOINT",
	// hosts
	"DOCKER_HOST", "KUBERNETES_SERVICE_HOST", "AZURE_AUTHORITY_HOST", "ITERION_SMTP_HOST",
	// proxies, both cases
	"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "http_proxy",
}

// isolateRoutingEnv puts the whole witness table under the test's control:
// the loader refuses against the LIVE environment (the operator's own export
// of any of these names, the opt-in included) and plants with os.Setenv,
// which t.Setenv does not restore — both are put back by hand, the way
// isolateEndpointEnv already does it.
func isolateRoutingEnv(t *testing.T) {
	t.Helper()
	envtrust.ResetForTest()
	t.Cleanup(envtrust.ResetForTest)
	t.Setenv(envtrust.EnvPlantedNames, "")
	clearEnv(t, envtrust.DotenvEndpointsOptIn)
	for _, name := range routingWitnessNames {
		clearEnv(t, name)
	}
}

// The doctrine-A witness: a project `.env` names one route of every family
// the class covers, and after the load the process's effective environment
// holds NO trace of any of them — not set, and not even marked planted,
// since a refused name does not exist rather than existing distrusted. Every
// refusal names the file, the key and the opt-in: the three things the next
// reader needs to see why the project's routes are gone and how a
// legitimate one comes back.
//
// On the defect this closes, the same file planted every one of these names
// into the process — and through it into every child, the vendored provider
// reads included — and the repository under review picked where each wire
// went.
func TestAProjectDotenvLeavesNoRoutingNameInTheEnvironment(t *testing.T) {
	isolateRoutingEnv(t)

	const repoHost = "https://routes.repo-under-review.example"
	lines := make([]string, 0, len(routingWitnessNames))
	for _, name := range routingWitnessNames {
		lines = append(lines, name+"="+repoHost)
	}
	repo := writeDotEnv(t, lines...)
	t.Chdir(repo)

	said := captureStderr(t, loadDotEnvFromCwd)

	envPath := filepath.Join(repo, ".env")
	for _, name := range routingWitnessNames {
		if got, set := os.LookupEnv(name); set {
			t.Errorf("%s = %q: the repository under review chose where a connection goes", name, got)
		}
		if envtrust.Planted(name) {
			t.Errorf("%s reads as planted: a refused name does not exist, it is not planted-but-distrusted", name)
		}
		for _, want := range []string{name, envPath, envtrust.DotenvEndpointsOptIn + "=allow"} {
			if !strings.Contains(said, want) {
				t.Errorf("the refusal does not name %q — it said: %s", want, said)
			}
		}
	}
}

// The control: the same name EXPORTED by the operator is preserved. The file
// only fills names the environment left unset, so the operator's endpoint
// survives the load untouched — and no refusal is said about it, since a
// line the loader never applies is not a refusal the operator has to read.
func TestAnOperatorExportedRoutingNameIsPreserved(t *testing.T) {
	isolateRoutingEnv(t)

	const operatorEndpoint = "https://operator.example"
	t.Setenv("OPENAI_BASE_URL", operatorEndpoint)
	repo := writeDotEnv(t, "OPENAI_BASE_URL=https://repo-under-review.example")
	t.Chdir(repo)

	said := captureStderr(t, loadDotEnvFromCwd)

	if got := os.Getenv("OPENAI_BASE_URL"); got != operatorEndpoint {
		t.Errorf("OPENAI_BASE_URL = %q, want the operator's %q: the environment the deployment exports must survive the load", got, operatorEndpoint)
	}
	if envtrust.Planted("OPENAI_BASE_URL") {
		t.Error("the operator's own endpoint reads as planted: it came from the environment, not from the file")
	}
	if said != "" {
		t.Errorf("the operator's own endpoint was warned about: %s", said)
	}
}

// With the opt-in, the same file plants its routes — and every planted name
// is marked planted: the value works, and it still says nothing about the
// operator, which is what keeps the belt (the provenance reads, the
// gateway's own refusal) between the planted routes and a run's credentials.
func TestTheOptInLetsAProjectDotenvPlantItsRoutes(t *testing.T) {
	isolateRoutingEnv(t)
	t.Setenv(envtrust.DotenvEndpointsOptIn, "allow")

	const repoHost = "https://routes.my-laptop.example"
	lines := make([]string, 0, len(routingWitnessNames))
	for _, name := range routingWitnessNames {
		lines = append(lines, name+"="+repoHost)
	}
	repo := writeDotEnv(t, lines...)
	t.Chdir(repo)

	said := captureStderr(t, loadDotEnvFromCwd)

	for _, name := range routingWitnessNames {
		if got := os.Getenv(name); got != repoHost {
			t.Errorf("%s = %q, want the file's %q: the operator opted in, the file plants its routes", name, got, repoHost)
		}
		if !envtrust.Planted(name) {
			t.Errorf("%s does not read as planted: a project file would speak for the operator", name)
		}
		if envtrust.Inherited(name) != "" {
			t.Errorf("%s still reads as inherited once planted: the belt would lose the question of authority", name)
		}
	}
	if said != "" {
		t.Errorf("a granted opt-in was warned about: %s", said)
	}
}
