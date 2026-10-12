package netproxy

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// A request to a model API carries a conversation or a prompt, and the
// placeholders in its body are what a model must see — the harness's own
// model calls (the claude CLI, the claw runner) leave the sandbox through
// this proxy like any other request. Substitution skips such a body; its
// headers (an API key given as a placeholder) are substituted as usual.
//
// The match errs toward a model request: a body left in placeholder form
// fails loudly at the API it was bound for, while a conversation
// materialised on its way to a provider leaks every secret it names.

// modelHosts are the model APIs' own hosts: the LLM entries of the
// iterion-default preset, narrowed to the model endpoints — the preset's
// **.googleapis.com also allows OAuth, whose token request carries a client
// secret in its body.
var modelHosts = map[string]bool{
	"api.anthropic.com":                 true,
	"api.openai.com":                    true,
	"openrouter.ai":                     true,
	"api.x.ai":                          true,
	"api.mistral.ai":                    true,
	"api.z.ai":                          true,
	"api.moonshot.ai":                   true,
	"api.moonshot.cn":                   true,
	"generativelanguage.googleapis.com": true,
	"aiplatform.googleapis.com":         true,
	// the ChatGPT forfait's backend (claw's and pi's ChatGPT-OAuth mode);
	// its OAuth refresh is on auth.openai.com
	"chatgpt.com": true,
	// pi's Radius gateway (its conversation goes to <base>/messages)
	"radius.pi.dev": true,
}

// modelPathSuffixes are the model APIs' paths, whatever the host (a gateway,
// a base URL of the operator's): Anthropic's Messages API and its compatible
// facades, OpenAI's and its compatibles' (the Responses API under any base —
// a gateway's, Copilot's, the ChatGPT backend's), Gemini, Anthropic on
// Vertex, Ollama.
var modelPathSuffixes = []string{
	"/v1/messages", "/v1/messages/count_tokens", "/v1/messages/batches",
	"/chat/completions", "/v1/completions", "/responses", "/v1/embeddings", "/images/generations",
	":generateContent", ":streamGenerateContent", ":countTokens", ":rawPredict", ":streamRawPredict",
	"/api/chat", "/api/generate", "/api/embed",
}

// bedrockPathSuffixes are Bedrock's model calls, under /model/{id}.
var bedrockPathSuffixes = []string{"/invoke", "/invoke-with-response-stream", "/converse", "/converse-stream", "/count-tokens"}

// modelRequest reports whether a request to host (a bare hostname) at path
// goes to a model API. extra holds the operator's own model hosts
// (Options.ModelHosts); nil when there are none.
//
// A built-in provider host is always one — no operator entry lifts it.
// Otherwise the operator's last matching entry decides: a pattern makes the
// host a model host, a `!` exclusion lifts the model detection for it, its
// path included (a TOOL API whose path ends like a model API's is then
// inspected and substituted like any other request). A host no entry
// matches is a model host when its path is a model API's.
func modelRequest(host, path string, extra *Policy) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if builtInProviderHost(host) {
		return true
	}
	if extra != nil {
		if matched, model := extra.lastMatch(canonicalHost(host)); matched {
			return model
		}
	}
	path = strings.TrimSuffix(path, "/")
	for _, s := range modelPathSuffixes {
		if strings.HasSuffix(path, s) {
			return true
		}
	}
	if strings.Contains(path, "/model/") {
		for _, s := range bedrockPathSuffixes {
			if strings.HasSuffix(path, s) {
				return true
			}
		}
	}
	return false
}

// modelHostPolicy compiles Options.ModelHosts with the network rules' syntax
// (a host, `*.corp`, `**.corp`, an IP, a CIDR, `!` to exclude — the last
// matching entry winning); a base URL or host:port names its host. An entry
// that is none of these — or that would match every host — is refused, never
// ignored: an ignored gateway would have its conversation substituted, and one
// matching every host would leave every body unsubstituted. So is an exclusion
// lying within the built-in provider hosts, which no entry lifts (see
// modelRequest). The error names an entry by its position (a URL's userinfo
// may hold a credential).
func modelHostPolicy(hosts []string) (*Policy, error) {
	var rules []string
	for i, raw := range hosts {
		h := strings.TrimSpace(raw)
		if h == "" {
			continue
		}
		bad := func(why string) error {
			return fmt.Errorf("netproxy: model host #%d: %s", i+1, why)
		}
		neg := strings.HasPrefix(h, "!")
		h = strings.TrimPrefix(h, "!")
		if strings.Contains(h, "://") {
			u, err := url.Parse(h)
			if err != nil || u.Hostname() == "" {
				return nil, bad("a URL with no host")
			}
			h = u.Hostname()
		} else if host, port, err := net.SplitHostPort(h); err == nil && numericPort(port) {
			h = host
		}
		pattern := strings.TrimSuffix(strings.Trim(h, "[]"), ".")
		if net.ParseIP(pattern) == nil && !strings.Contains(pattern, "/") && strings.ContainsAny(pattern, ":@! \t\n\r\v\f") {
			return nil, bad("not a host pattern or a URL")
		}
		if pattern == "*" || pattern == "**" {
			return nil, bad("matches every host")
		}
		if net.ParseIP(pattern) == nil && !strings.Contains(pattern, "/") {
			// A request's host is matched in its canonical form (lowercase,
			// IDN folded): an entry spelt otherwise, or with an empty label,
			// would match no host.
			wild := ""
			if strings.HasPrefix(pattern, "**.") || strings.HasPrefix(pattern, "*.") {
				wild = pattern[:strings.Index(pattern, ".")+1]
			}
			name := canonicalHost(strings.TrimPrefix(pattern, wild))
			if name == "" || strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".") || strings.Contains(name, "..") {
				return nil, bad("not a host pattern or a URL")
			}
			pattern = wild + name
		}
		if neg {
			if builtInProviderHost(pattern) {
				return nil, bad("names a built-in model provider — a model provider host cannot be excluded from the model detection")
			}
			pattern = "!" + pattern
		}
		if _, err := Compile(ModeAllowlist, []string{pattern}); err != nil {
			return nil, bad("not a host pattern or a URL")
		}
		rules = append(rules, pattern)
	}
	if len(rules) == 0 {
		return nil, nil
	}
	return Compile(ModeAllowlist, rules)
}

// builtInProviderHost reports whether name is one of the built-in model
// provider hosts or lies within one of their families. Such a host is always
// a model request: its bodies keep their placeholders, the guarantee the
// model detection exists for, and no operator exclusion lifts it.
func builtInProviderHost(name string) bool {
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	if modelHosts[name] {
		return true
	}
	return strings.HasSuffix(name, "-aiplatform.googleapis.com") ||
		strings.HasSuffix(name, ".openai.azure.com") ||
		(strings.HasPrefix(name, "bedrock-runtime.") && strings.HasSuffix(name, ".amazonaws.com"))
}

// numericPort reports whether p is a decimal port: a colon that is not a port
// separator (a scheme, a userinfo) does not pass for one.
func numericPort(p string) bool {
	if p == "" {
		return false
	}
	for _, r := range p {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
