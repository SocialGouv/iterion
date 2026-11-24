package envtrust

import "strings"

// credentialsPresentedAt names, for every environment variable that decides
// WHICH HOST a model call is dialled at, the credential variables presented
// there. The two halves are one credential presentation: `<VENDOR>_BASE_URL`
// says where the call goes, the key beside it says what authenticates it.
//
// A project `.env` fills in every name the operator left unset (cmd/iterion
// applyDotEnv), and that file sits in a repository — including the one a run
// is reviewing. Naming the endpoint alone is therefore enough to pick the
// host the operator's own key is presented to: on a router-style endpoint
// that selects which virtual key, hence which budget, model allowlist and
// tenant the reviewer's calls are billed to, and the request bodies carry
// whatever the run is reading. So the ENDPOINT is the half the loader
// refuses: a key already presented cannot be taken back.
//
// Asked at the plant rather than at each read because the reads are not all
// iterion's. The vendored claw provider packages read these names through
// os.Getenv of their own (foundry reads AZURE_OPENAI_ENDPOINT and
// AZURE_OPENAI_API_KEY when the registry passes no BaseURL), and every child
// iterion spawns inherits a planted value in its own environment. A name
// that is never set is closed for all of them at once, where a guard at one
// read site closes exactly that site.
//
// Hosts only. A name that picks a deployment, an API version or a region
// INSIDE infrastructure the operator already named moves no credential off
// it, and a credential no environment variable carries — Google ADC from the
// instance metadata server, a Claude forfait from disk — is not something
// this table can see: pkg/backend/model's class test carries those
// exclusions with their reasons.
var credentialsPresentedAt = map[string][]string{
	// claw's openai provider: OpenAI itself, and any OpenAI-shaped backend
	// (OpenRouter / Ollama / vLLM) the registry forwards the name to. The
	// ChatGPT forfait is not listed: openAIOAuthAllowed refuses the OAuth
	// path outright once this name is set, whoever set it.
	"OPENAI_BASE_URL": {"OPENAI_API_KEY"},
	// The anthropic wire carries three environment credentials: the API
	// key, the BYOK bearer, and the z.ai key the registry synthesises a
	// bearer from. The Claude forfait is not among them —
	// secrets.AnthropicForfaitWireOK pins it to api.anthropic.com whoever
	// named the wire.
	"ANTHROPIC_BASE_URL": {"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ZAI_API_KEY"},
	"XAI_BASE_URL":       {"XAI_API_KEY"},
	// Moonshot's Anthropic-compatible host, read by its own factory and
	// deliberately not ANTHROPIC_BASE_URL (that is z.ai's wiring knob).
	"MOONSHOT_BASE_URL": {"MOONSHOT_API_KEY"},
	// The Azure resource claw's foundry provider dials, which it reads
	// itself along with the key.
	"AZURE_OPENAI_ENDPOINT": {"AZURE_OPENAI_API_KEY"},
	// The OpenAI-compatible gateway (pkg/backend/compatgw), which refuses
	// the pair by name when either half is planted.
	"OPENAI_COMPATIBLE_BASE_URL": {"OPENAI_COMPATIBLE_API_KEY"},
}

// EndpointNames are the variables credentialsPresentedAt answers for — the
// host-deciding half of every pair on a model's path — sorted, so a caller
// enumerating the class reads the same list every run.
func EndpointNames() []string {
	names := make([]string, 0, len(credentialsPresentedAt))
	for name := range credentialsPresentedAt {
		names = append(names, name)
	}
	sortStrings(names)
	return names
}

// CredentialsPresentedAt returns the credential variables a model call is
// authenticated with at the host name decides, in the order they are read —
// whatever their provenance. Empty for a name that decides no host.
func CredentialsPresentedAt(name string) []string {
	creds := credentialsPresentedAt[CanonicalName(name)]
	if len(creds) == 0 {
		return nil
	}
	return append([]string(nil), creds...)
}

// InheritedCredentialsAt is the subset of CredentialsPresentedAt this process
// INHERITED with a value: the credentials a project `.env` filling name would
// redirect to a host of that file's choosing.
//
// Empty — so the `.env` applies, as it does for an API key next to a project
// — for a name that decides no host, and for an endpoint whose credentials
// the operator left to the project too: there the pair is the project's own
// and nothing of the deployment's moves. The question is asked on the
// ENDPOINT alone for that reason: a planted key spent at the published
// default endpoint is the project's key on its vendor's own host, which is
// what `.env` loading exists for.
func InheritedCredentialsAt(name string) []string {
	var out []string
	for _, cred := range credentialsPresentedAt[CanonicalName(name)] {
		if strings.TrimSpace(Inherited(cred)) != "" {
			out = append(out, cred)
		}
	}
	return out
}
