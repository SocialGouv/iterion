package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	"github.com/SocialGouv/iterion/internal/envtrust"
	"github.com/SocialGouv/iterion/pkg/backend/model"
)

// isolateEndpointEnv puts the whole pair class under the test's control: the
// operator's own environment is read by every factory on a model's path, and
// this binary inherits the shell that launched it — a ZAI_API_KEY exported
// for an unrelated reason would otherwise decide which case runs. Every name
// is restored by t.Setenv; the loader's own plants are restored by hand.
func isolateEndpointEnv(t *testing.T) {
	t.Helper()
	envtrust.ResetForTest()
	t.Cleanup(envtrust.ResetForTest)
	t.Setenv(envtrust.EnvPlantedNames, "")
	for _, endpoint := range envtrust.EndpointNames() {
		clearEnv(t, endpoint)
		restoreEnv(t, endpoint)
		for _, cred := range envtrust.CredentialsPresentedAt(endpoint) {
			clearEnv(t, cred)
			restoreEnv(t, cred)
		}
	}
}

// clearEnv unsets a variable for the test, restoring it afterwards: t.Setenv
// cannot express "absent", and absent is the state a `.env` fills.
func clearEnv(t *testing.T, name string) {
	t.Helper()
	restoreEnv(t, name)
	if err := os.Unsetenv(name); err != nil {
		t.Fatalf("unset %s: %v", name, err)
	}
}

// captureStderr runs fn with os.Stderr on a pipe and returns what it wrote.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	fn()
	os.Stderr = orig
	if err := w.Close(); err != nil {
		t.Fatalf("close the pipe: %v", err)
	}
	said, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read the pipe: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("close the read end: %v", err)
	}
	return string(said)
}

// writeDotEnv writes a `.env` in a temp repo and returns its directory.
func writeDotEnv(t *testing.T, lines ...string) string {
	t.Helper()
	repo := t.TempDir()
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(repo, ".env"), []byte(body), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}
	return repo
}

// An endpoint a project `.env` names is not applied while the credential
// presented there is the operator's own — the whole class, through the real
// producer: a repository ships a `.env` naming every endpoint on a model's
// path, the reviewer's `iterion run` auto-loads it from its working
// directory, and each line is refused by name.
//
// Refused at the PLANT rather than at each read because the reads are not
// all iterion's: claw's vendored provider packages read these names
// themselves and every child process inherits a planted value, so the one
// state that closes them all is the name never being set.
func TestAProjectDotenvCannotChooseTheHostACredentialIsPresentedTo(t *testing.T) {
	isolateEndpointEnv(t)

	// The floor, spelled out: this case ENUMERATES the table, so an entry
	// removed from it would quietly leave the case proving less rather than
	// failing. pkg/backend/model's class guard judges the table against the
	// names the program reads; this keeps the enumeration from going vacuous.
	for _, want := range []string{
		"OPENAI_BASE_URL", "ANTHROPIC_BASE_URL", "XAI_BASE_URL",
		"MOONSHOT_BASE_URL", "AZURE_OPENAI_ENDPOINT", "OPENAI_COMPATIBLE_BASE_URL",
	} {
		if !slices.Contains(envtrust.EndpointNames(), want) {
			t.Fatalf("%s left the endpoint table (%v): either it decides no host any more, or this case stopped covering the class it was written for", want, envtrust.EndpointNames())
		}
	}

	const repoHost = "https://endpoint.repo-under-review.example"
	var lines []string
	for _, endpoint := range envtrust.EndpointNames() {
		lines = append(lines, endpoint+"="+repoHost)
		// The operator's own channel, for every credential of the pair.
		for _, cred := range envtrust.CredentialsPresentedAt(endpoint) {
			t.Setenv(cred, "operator-"+strings.ToLower(cred))
		}
	}
	repo := writeDotEnv(t, lines...)
	t.Chdir(repo)

	said := captureStderr(t, loadDotEnvFromCwd)

	for _, endpoint := range envtrust.EndpointNames() {
		if got, set := os.LookupEnv(endpoint); set {
			t.Errorf("%s = %q: the repository under review picked the host the operator's %v is presented to", endpoint, got, envtrust.CredentialsPresentedAt(endpoint))
		}
		if envtrust.Planted(endpoint) {
			t.Errorf("%s reads as planted: it was refused, not filled, so nothing downstream should distrust a value that is not there", endpoint)
		}
		// A refusal nobody can read is the silence the round before was
		// filed for: the variable, the credential at stake and the channel
		// that answers for the operator.
		for _, want := range []string{endpoint, "project `.env`", "export " + endpoint} {
			if !strings.Contains(said, want) {
				t.Errorf("the diagnostic does not say %q — it said: %s", want, said)
			}
		}
		for _, cred := range envtrust.CredentialsPresentedAt(endpoint) {
			if os.Getenv(cred) == "" {
				continue
			}
			if !strings.Contains(said, cred) {
				t.Errorf("the diagnostic does not name %s, the credential that would have moved — it said: %s", cred, said)
			}
			if strings.Contains(said, os.Getenv(cred)) {
				t.Errorf("the diagnostic printed the value of %s", cred)
			}
		}
	}
}

// The other half of the same rule, and the one that keeps `.env` loading
// worth having: with no credential inherited for it, an endpoint a project
// `.env` names IS applied — a laptop configuration, the key and the host in
// one file — and a key next to a project still reaches the published
// endpoint it was always spent at. The question is asked on the endpoint
// alone for exactly that reason.
func TestAProjectDotenvStillConfiguresItsOwnGateway(t *testing.T) {
	isolateEndpointEnv(t)

	const (
		repoHost = "https://gw.my-laptop.example"
		repoKey  = "sk-my-own-laptop-key"
	)
	repo := writeDotEnv(t,
		"OPENAI_BASE_URL="+repoHost,
		"OPENAI_API_KEY="+repoKey,
		// A credential with no endpoint beside it: the vendor's published
		// host answers, as it did before any of this.
		"XAI_API_KEY=xai-project-key",
	)
	t.Chdir(repo)
	for _, name := range []string{"OPENAI_BASE_URL", "OPENAI_API_KEY", "XAI_API_KEY"} {
		restoreEnv(t, name)
	}

	said := captureStderr(t, loadDotEnvFromCwd)

	if got := os.Getenv("OPENAI_BASE_URL"); got != repoHost {
		t.Errorf("OPENAI_BASE_URL = %q, want the project's %q: a laptop lost the gateway its own .env configures", got, repoHost)
	}
	if got := os.Getenv("OPENAI_API_KEY"); got != repoKey {
		t.Errorf("OPENAI_API_KEY = %q, want the project's own key", got)
	}
	if got := os.Getenv("XAI_API_KEY"); got != "xai-project-key" {
		t.Errorf("XAI_API_KEY = %q: a key next to a project is what .env loading exists for", got)
	}
	// Planted, so every question of AUTHORITY still reads it as nothing
	// said — the value works, it just does not speak for the operator.
	for _, name := range []string{"OPENAI_BASE_URL", "OPENAI_API_KEY", "XAI_API_KEY"} {
		if !envtrust.Planted(name) {
			t.Errorf("%s does not read as planted: a project file would speak for the operator", name)
		}
	}
	if said != "" {
		t.Errorf("a project configuring its own gateway was warned about: %s", said)
	}
}

// The wire, through the real road: a repository's `.env` names the endpoint,
// the operator exported the key, and the request the node sends must not
// reach the host that file chose.
//
// Driven on Azure because its endpoint is REQUIRED — claw's foundry provider
// refuses by name when AZURE_OPENAI_ENDPOINT is unset, before any dial — so
// the case needs no network to prove the negative. On the defect this closes,
// the planted endpoint received `api-key: <the operator's>` (the H-10
// measurement of round 2, p1.log, made on OPENAI_BASE_URL in the same class).
func TestTheOperatorsKeyIsNotDialledAtAnEndpointAProjectDotenvNamed(t *testing.T) {
	isolateEndpointEnv(t)

	seen := make(chan string, 4)
	repoEndpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get("api-key")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer repoEndpoint.Close()

	const operatorKey = "operator-azure-key"
	t.Setenv("AZURE_OPENAI_API_KEY", operatorKey)
	repo := writeDotEnv(t, "AZURE_OPENAI_ENDPOINT="+repoEndpoint.URL)
	t.Chdir(repo)
	restoreEnv(t, "AZURE_OPENAI_ENDPOINT")
	loadDotEnvFromCwd()

	client, err := model.NewRegistry().Resolve("foundry/a-deployment")
	if err == nil {
		// Only reached when the endpoint was applied after all: send the
		// request, so what the host RECEIVES is what the case reports.
		if _, serr := client.StreamResponse(context.Background(), api.CreateMessageRequest{
			Model: "a-deployment", MaxTokens: 1,
			Messages: []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "hi"}}}},
		}); serr != nil {
			t.Logf("StreamResponse: %v", serr)
		}
	}
	select {
	case got := <-seen:
		t.Fatalf("the endpoint the repository under review named received api-key %q: the operator's Azure credential left the deployment", got)
	default:
	}
	if err == nil {
		t.Fatal("a client was built for an endpoint only the repository's .env named: nothing stops the next request from reaching it")
	}
	// The refusal names the variable the operator has to set, and no node
	// runs against a second resource in silence.
	if !strings.Contains(err.Error(), "AZURE_OPENAI_ENDPOINT") {
		t.Errorf("the refusal does not name AZURE_OPENAI_ENDPOINT: %v", err)
	}
	if strings.Contains(err.Error(), operatorKey) {
		t.Error("the refusal printed the credential")
	}
}
