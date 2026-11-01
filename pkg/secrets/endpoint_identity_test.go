package secrets

import (
	"strings"
	"testing"
)

// zaiRespellings are the spellings that reach the z.ai default endpoint. Each
// one is a value an operator (or a vendor's own documentation) can write into
// ANTHROPIC_BASE_URL, and every one of them dials api.z.ai.
var zaiRespellings = []struct{ name, spelling string }{
	{"the published spelling", ZAIDefaultBaseURL},
	{"surrounding whitespace", "  " + ZAIDefaultBaseURL + "  "},
	{"a trailing slash", ZAIDefaultBaseURL + "/"},
	{"an upper-cased spelling", strings.ToUpper(ZAIDefaultBaseURL)},
	{"the port https implies", "https://api.z.ai:443/api/anthropic"},
	{"a leading-zero spelling of that port", "https://api.z.ai:0443/api/anthropic"},
	{"a padded spelling of that port", "https://api.z.ai:00000443/api/anthropic"},
	{"the absolute-FQDN form of the host", "https://api.z.ai./api/anthropic"},
	{"userinfo, which authenticates and does not route", "https://token@api.z.ai/api/anthropic"},
	{"a fragment, which never leaves the client", ZAIDefaultBaseURL + "#frag"},
	{"a query no vendor publishes in a base URL", ZAIDefaultBaseURL + "?x=1"},
	{"a percent-escaped path character", "https://api.z.ai/api/anthropi%63"},
	{"an empty path segment", "https://api.z.ai/api//anthropic"},
	{"a dot segment", "https://api.z.ai/api/./anthropic"},
	{"a dot-dot segment", "https://api.z.ai/extra/../api/anthropic"},
	{"the host's full-width internationalised spelling", "https://ａｐｉ.ｚ.ａｉ/api/anthropic"},
}

// zaiOtherDestinations are spellings that do NOT reach api.z.ai, including the
// ones built to look like it. Each must stay unequal: a fold that swallowed
// them would read the operator's own gateway as the vendor and refuse a route
// nobody substituted.
var zaiOtherDestinations = []struct{ name, spelling string }{
	{"another host", "https://gateway.example/anthropic"},
	{"a host that merely contains the vendor's name", "https://api.z.ai.evil.example/api/anthropic"},
	{"the vendor's name as userinfo over another host", "https://api.z.ai@evil.example/api/anthropic"},
	{"a non-default port", "https://api.z.ai:8443/api/anthropic"},
	{"a port no dial accepts", "https://api.z.ai:99999999999999999999/api/anthropic"},
	{"a port-shaped label on another domain", "https://api.z.ai:443.evil.example/api/anthropic"},
	{"a space inside the host is a name nothing dials", "https://api.z.ai /api/anthropic"},
	{"a non-breaking space inside the host", "https://api.z.ai\u00a0/api/anthropic"},
	{"another scheme, another implied port", "http://api.z.ai/api/anthropic"},
	{"the other vendor of the same wire", MoonshotDefaultBaseURL},
	{"the same vendor's other published host", ZAIChinaBaseURL},
	{"an unset value", ""},
}

// SameEndpoint is the fold every credential-routing verdict compares through
// (FacadeVendorEndpoints via delegate.OperatorEndpointClaim,
// AnthropicForfaitWireOK). Both halves are asserted, because each half alone
// is satisfiable by a cheat: a fold of everything makes every endpoint the
// vendor's, a fold of nothing makes the vendor's own respellings an operator
// claim and lets the mislabel through.
func TestSameEndpointFoldsRespellingsAndNothingElse(t *testing.T) {
	t.Run("a spelling that routes to the vendor compares equal", func(t *testing.T) {
		for _, tc := range zaiRespellings {
			t.Run(tc.name, func(t *testing.T) {
				if !SameEndpoint(tc.spelling, ZAIDefaultBaseURL) {
					t.Errorf("SameEndpoint(%q, %q) = false, want true — both dial api.z.ai, so both are the vendor's own wiring (canonical: %q vs %q)",
						tc.spelling, ZAIDefaultBaseURL, CanonicalEndpoint(tc.spelling), CanonicalEndpoint(ZAIDefaultBaseURL))
				}
			})
		}
	})

	t.Run("a spelling that routes elsewhere never compares equal", func(t *testing.T) {
		for _, tc := range zaiOtherDestinations {
			t.Run(tc.name, func(t *testing.T) {
				if SameEndpoint(tc.spelling, ZAIDefaultBaseURL) {
					t.Errorf("SameEndpoint(%q, %q) = true, want false — it names another destination (canonical: %q vs %q)",
						tc.spelling, ZAIDefaultBaseURL, CanonicalEndpoint(tc.spelling), CanonicalEndpoint(ZAIDefaultBaseURL))
				}
			})
		}
	})

	t.Run("a port is the number the dialler resolves", func(t *testing.T) {
		// The fold is numeric on both sides of the default: two spellings of
		// one port are one endpoint, whether or not the scheme implies it. A
		// string comparison would read ":0443" as a port of its own and hand
		// the vendor's own route back as the operator's.
		if !SameEndpoint("https://api.z.ai:08443/api/anthropic", "https://api.z.ai:8443/api/anthropic") {
			t.Errorf("SameEndpoint(:08443, :8443) = false, want true — net.LookupPort reads both as 8443, which is the port dialled (canonical: %q vs %q)",
				CanonicalEndpoint("https://api.z.ai:08443/api/anthropic"), CanonicalEndpoint("https://api.z.ai:8443/api/anthropic"))
		}
		// A number over 65535 is a port net.Dial refuses: it is kept as
		// written, so it names neither the vendor's endpoint nor a port.
		if SameEndpoint("https://api.z.ai:99999999999999999999/api/anthropic", "https://api.z.ai:443/api/anthropic") {
			t.Error("SameEndpoint(a port no dial accepts, :443) = true, want false — a port that does not resolve is not folded")
		}
	})

	t.Run("every published endpoint is its own destination", func(t *testing.T) {
		// Each facade publishes two hosts, and the second is not the first:
		// a fold that collapsed the ensemble into one entry would pass every
		// assertion above while making the set meaningless.
		var published []string
		for _, p := range FacadeProviders() {
			published = append(published, FacadeVendorEndpoints(p)...)
		}
		if len(published) < 2 {
			t.Fatalf("the published ensemble holds %d endpoints — nothing to tell apart", len(published))
		}
		for i, a := range published {
			for j, b := range published {
				if i != j && SameEndpoint(a, b) {
					t.Errorf("SameEndpoint(%q, %q) = true, want false — two published endpoints are two destinations", a, b)
				}
			}
		}
	})
}

// The fold's output is one of the spellings it folds, so folding it again
// changes nothing. Without that, an endpoint stored canonical and compared
// against a freshly read one would answer differently from the same pair read
// twice.
func TestCanonicalEndpointIsIdempotent(t *testing.T) {
	var spellings []string
	for _, tc := range zaiRespellings {
		spellings = append(spellings, tc.spelling)
	}
	for _, tc := range zaiOtherDestinations {
		spellings = append(spellings, tc.spelling)
	}
	spellings = append(spellings,
		"https://[::1]:443/api/anthropic",
		"https://127.0.0.1:8443/v1",
		"https://gateway.example/café", // a path the fold re-escapes
		"api.z.ai/api/anthropic",       // no scheme: nothing structural to read
		"not a url",
		"https://api.z.ai",
	)
	for _, raw := range spellings {
		once := CanonicalEndpoint(raw)
		if twice := CanonicalEndpoint(once); twice != once {
			t.Errorf("CanonicalEndpoint(%q) = %q, and folding that again gives %q — the fold is not idempotent", raw, once, twice)
		}
	}
}

// AnthropicForfaitWireOK is the second reader of the same fold: it decides
// whether a Claude subscription bearer — the whole account, not a scoped key
// — may be presented at a base URL. The spellings of Anthropic's own host
// must pass (a forfait refused on a route that dials Anthropic leaves the
// node with no credential at all), and every spelling that dials anything
// else must not.
func TestAnthropicForfaitWireOKJudgesTheDialledHost(t *testing.T) {
	for _, tc := range []struct {
		name    string
		baseURL string
		want    bool
	}{
		{"unset is the SDK default, which is Anthropic's own", "", true},
		{"blank is unset", "   ", true},
		{"the plain host", "https://api.anthropic.com", true},
		{"an upper-cased host", "https://API.ANTHROPIC.COM/v1", true},
		{"an upper-cased scheme is still the https wire", "HTTPS://api.anthropic.com/v1", true},
		{"a plaintext scheme is not Anthropic's wire", "http://api.anthropic.com/v1", false},
		{"the absolute-FQDN form of the host", "https://api.anthropic.com./v1", true},
		{"userinfo over Anthropic's host", "https://token@api.anthropic.com", true},
		{"the port https implies", "https://api.anthropic.com:443", true},
		{"a padded spelling of that port", "https://api.anthropic.com:0443", true},
		{"a port of the operator's own is still Anthropic's host", "https://api.anthropic.com:8443", true},
		{"a vendor facade", ZAIDefaultBaseURL, false},
		{"the vendor's mainland host", ZAIChinaBaseURL, false},
		{"Anthropic's name as userinfo over another host", "https://api.anthropic.com@gateway.example/v1", false},
		{"a host that merely contains Anthropic's name", "https://api.anthropic.com.gateway.example/v1", false},
		{"an interception proxy", "https://gateway.example/anthropic", false},
		{"a space inside the host", "https://api.anthropic.com /v1", false},
		{"a non-breaking space inside the host", "https://api.anthropic.com\u00a0/v1", false},
		{"a line separator inside the host", "https://api.anthropic.com\u2028/v1", false},
		{"an ideographic space inside the host", "https://api.anthropic.com\u3000/v1", false},
		{"a value with no host", "api.anthropic.com", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := AnthropicForfaitWireOK(tc.baseURL); got != tc.want {
				t.Errorf("AnthropicForfaitWireOK(%q) = %v, want %v", tc.baseURL, got, tc.want)
			}
		})
	}
}
