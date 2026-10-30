package envtrust

import (
	"slices"
	"testing"
)

// The floor of the routing class: a real name per family the suffixes exist
// for, read somewhere in this tree (the inventory lives on routingSuffixes).
// A suffix that stopped matching its family fails here rather than letting
// the loader plant routes again quietly.
func TestRoutingShapedNameCoversTheFamiliesTheTreeReads(t *testing.T) {
	for _, name := range []string{
		// provider endpoints (_BASE_URL)
		"OPENAI_BASE_URL", "ANTHROPIC_BASE_URL", "XAI_BASE_URL", "MOONSHOT_BASE_URL",
		"OPENAI_COMPATIBLE_BASE_URL", "DASHSCOPE_BASE_URL", "CLAUDE_CODE_API_BASE_URL",
		// the legacy OpenAI name (_API_BASE)
		"OPENAI_API_BASE",
		// endpoints (_ENDPOINT)
		"AZURE_OPENAI_ENDPOINT", "OTEL_EXPORTER_OTLP_ENDPOINT",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "ITERION_S3_ENDPOINT",
		"SEARXNG_ENDPOINT", "AWS_EC2_METADATA_SERVICE_ENDPOINT", "MSI_ENDPOINT",
		"IMDS_ENDPOINT", "ITERION_SANDBOX_PROXY_ENDPOINT",
		// hosts (_HOST, the Azure authority hosts among them)
		"DOCKER_HOST", "KUBERNETES_SERVICE_HOST", "GCE_METADATA_HOST",
		"ITERION_SMTP_HOST", "GH_HOST", "AZURE_AUTHORITY_HOST",
		"AZURE_POD_IDENTITY_AUTHORITY_HOST",
		// proxies (_PROXY, both cases)
		"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY",
		"http_proxy", "https_proxy", "all_proxy", "no_proxy",
	} {
		if !RoutingShapedName(name) {
			t.Errorf("%s is not read as routing-shaped: a project `.env` would plant it", name)
		}
	}
}

// Case does not decide: the uppercase asks the same question as the
// lowercase, which on Windows is not even two names.
func TestRoutingShapedNameIsCaseInsensitiveOnTheShape(t *testing.T) {
	for _, name := range []string{"openai_base_url", "Azure_Authority_Host", "docker_host", "Openai_Api_Base"} {
		if !RoutingShapedName(name) {
			t.Errorf("%s: the shape must match whatever the case", name)
		}
	}
}

// The names the class must NOT take, each because something else decides it:
// the model-path exclusions (a deployment, an API version, a kill switch, a
// Google project no visible credential moves), the keys themselves, the
// provenance marker, the opt-in — and a name that merely CONTAINS a routing
// word is not on the wire for it.
func TestRoutingShapedNameLeavesTheNonRoutingNamesAlone(t *testing.T) {
	for _, name := range []string{
		"OPENAI_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ZAI_API_KEY",
		"ITERION_HOME", "ITERION_DOTENV_PLANTED", "ITERION_DOTENV_ENDPOINTS",
		"GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_REGION",
		"AZURE_OPENAI_DEPLOYMENT", "AZURE_OPENAI_API_VERSION",
		"CLAW_OPENAI_REASONING_SUMMARY", "ANTHROPIC_CUSTOM_HEADERS",
		"ITERION_CLAW_COMPACT_THRESHOLD_RATIO", "ITERION_MODEL_SPECS_CACHE",
		// the suffix alone names nothing
		"_HOST", "_BASE_URL",
	} {
		if RoutingShapedName(name) {
			t.Errorf("%s is read as routing-shaped: the loader would refuse a name nothing routes with", name)
		}
	}
}

// The suffix list is the inventory: dropping a suffix would quietly narrow
// the class, so the case above proves nothing unless the list still holds
// every shape it was written for.
func TestEveryRoutingSuffixIsPresent(t *testing.T) {
	for _, want := range []string{"_BASE_URL", "_API_BASE", "_ENDPOINT", "_HOST", "_PROXY"} {
		if !slices.Contains(routingSuffixes, want) {
			t.Errorf("%s left routingSuffixes (%v): either the tree stopped reading that shape, or the class was narrowed without saying so", want, routingSuffixes)
		}
	}
}

// The opt-in answers only to the INHERITED environment, in its own spelling.
func TestDotenvEndpointsOptInIsAnsweredOnlyWhenInherited(t *testing.T) {
	ResetForTest()
	t.Cleanup(ResetForTest)
	t.Setenv(EnvPlantedNames, "")

	t.Setenv(DotenvEndpointsOptIn, "allow")
	if !DotenvEndpointsAllowed() {
		t.Error("the operator's own ITERION_DOTENV_ENDPOINTS=allow did not grant the opt-in")
	}
	// Surrounding whitespace is ignored (documented), but nothing else is:
	// the spelling is the operator's consent, not a fuzzy hint.
	t.Setenv(DotenvEndpointsOptIn, " allow ")
	if !DotenvEndpointsAllowed() {
		t.Error("ITERION_DOTENV_ENDPOINTS=\" allow \" did not grant the opt-in; whitespace around it is ignored")
	}

	for _, no := range []string{"", "0", "1", "true", "Allow", "always", "allow anything"} {
		t.Setenv(DotenvEndpointsOptIn, no)
		if DotenvEndpointsAllowed() {
			t.Errorf("ITERION_DOTENV_ENDPOINTS=%q granted the opt-in; only `allow` does", no)
		}
	}
}

// A `.env` line naming the opt-in must not grant it: the loader marks what it
// plants, and the planted value reads as nothing said here — the file that
// plants routes beside the opt-in cannot vote for itself, whatever order it
// put its lines in.
func TestAPlantedOptInGrantsNothing(t *testing.T) {
	ResetForTest()
	t.Cleanup(ResetForTest)
	t.Setenv(EnvPlantedNames, "")
	t.Setenv(DotenvEndpointsOptIn, "allow")

	MarkPlanted(DotenvEndpointsOptIn)
	if Inherited(DotenvEndpointsOptIn) != "" {
		t.Fatal("a planted opt-in still reads as inherited: the seed is wrong, so the rest of this case proves nothing")
	}
	if DotenvEndpointsAllowed() {
		t.Error("a planted ITERION_DOTENV_ENDPOINTS=allow granted the opt-in: the repository consented on the operator's behalf")
	}
}
