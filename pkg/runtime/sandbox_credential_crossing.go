package runtime

import (
	"context"
	"strings"

	"github.com/SocialGouv/iterion/pkg/sandbox/netproxy"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// sandbox_credential_crossing.go is the proxy's half of ADR-162 decision 7:
// an untrusted run's container holds its LLM credential as a Layer 2
// placeholder (secrets.CredentialPlaceholder — a pure function of the value,
// which is how the arm that writes the spawn env and this rewriter agree
// with no coordination), and the proxy substitutes the placeholder for the
// real key in headers toward the credential's OWN host — the hosts
// secrets.CredentialEgressHosts names — and nowhere else. A real value
// riding a request from the container is refused on any host: toward the
// credential's host it authenticates as something other than the
// placeholder (the letter of the decision: "a request to it that
// authenticates with anything but the placeholder is refused"), and toward
// any other host it is plain exfiltration.

// crossingEntry is one credential the proxy can hold: its placeholder, its
// real value and the only hosts the substitution serves.
type crossingEntry struct {
	placeholder string
	value       string
	hosts       []string
}

// credentialCrossings reads the run's sealed credentials off ctx — the same
// Credentials the arms spend — and builds the proxy's substitution table.
// Every key of the run's set and every key pinned for a route crosses; a
// key whose provider names no egress host (an SDK signing chain) carries no
// substitution but KEEPS its DLP entry: the arms refuse to cross it, and if
// its value reaches the wire anyway the proxy blocks it.
func credentialCrossings(ctx context.Context) []crossingEntry {
	creds, ok := secrets.CredentialsFromContext(ctx)
	if !ok {
		return nil
	}
	seen := map[string]bool{}
	var entries []crossingEntry
	add := func(provider secrets.Provider, key, baseURL string) {
		if key == "" || seen[key] {
			return
		}
		placeholder := secrets.CredentialPlaceholder(key)
		if placeholder == "" {
			// Too short to carry a placeholder — and too short to match
			// safely for DLP. The arms refuse to cross it; nothing to hold.
			return
		}
		seen[key] = true
		entries = append(entries, crossingEntry{
			placeholder: placeholder,
			value:       key,
			hosts:       secrets.CredentialEgressHosts(provider, baseURL),
		})
	}
	for provider, key := range creds.APIKeys {
		add(provider, key, creds.APIBaseURLs[provider])
	}
	for provider, key := range creds.PinnedAPIKeys {
		add(provider, key, creds.PinnedAPIBaseURLs[provider])
	}
	return entries
}

// credentialCrossing is the composed Layer 2 rewriter for an untrusted run:
// the credential substitution over whatever the executor's secret guard
// already serves (declared workflow secrets, host-scoped exactly as
// before). It implements netproxy.SecretRewriter.
type credentialCrossing struct {
	inner netproxy.SecretRewriter
	creds []crossingEntry
}

// composeCredentialCrossing wraps inner (which may be nil — a run with no
// declared secrets still crosses its LLM credentials) with the credential
// substitution. Nil creds answer the inner rewriter unchanged.
func composeCredentialCrossing(inner netproxy.SecretRewriter, creds []crossingEntry) netproxy.SecretRewriter {
	if len(creds) == 0 {
		return inner
	}
	return &credentialCrossing{inner: inner, creds: creds}
}

// MaterializeForHostWithin swaps each placeholder for its real value, but
// only toward a host the credential's own egress table permits — the
// decision's "one route out". A placeholder toward any other host is left
// untouched (and the network floor refuses that host before it is ever
// dialed); a text with no placeholder answers as given. The bound follows
// the guard's contract: a result longer than limit is refused, and the
// length is computed before any substitution allocates it.
func (c *credentialCrossing) MaterializeForHostWithin(s, host string, limit int) (string, bool) {
	if len(s) > limit {
		return s, false
	}
	host = canonicalCrossingHost(host)
	for _, e := range c.creds {
		if !crossingHostAllowed(e.hosts, host) {
			continue
		}
		n := strings.Count(s, e.placeholder)
		if n == 0 {
			continue
		}
		if grow := len(e.value) - len(e.placeholder); grow > 0 && n > (limit-len(s))/grow {
			return s, false
		}
		s = strings.ReplaceAll(s, e.placeholder, e.value)
	}
	if c.inner != nil {
		return c.inner.MaterializeForHostWithin(s, host, limit)
	}
	return s, true
}

// ExfiltratesTo reports whether s carries a REAL credential value — on any
// host. Toward the credential's own host, only the placeholder may
// authenticate: a raw value there is a foreign key or a smuggled one, and
// the decision refuses it. Toward every other host a raw value is classic
// exfiltration and refuses the same way. Only values long enough to be
// credentials are matched, so a short common word cannot false-positive a
// request into the refusal.
func (c *credentialCrossing) ExfiltratesTo(s, host string) bool {
	for _, e := range c.creds {
		if len(e.value) >= 8 && strings.Contains(s, e.value) {
			return true
		}
	}
	if c.inner != nil {
		return c.inner.ExfiltratesTo(s, host)
	}
	return false
}

// crossingHostAllowed reports whether host is one the credential's egress
// table names, matching the guard's form: exact, or a parent domain (so
// "api.anthropic.com" in the table does not license "evil-api.anthropic.com.attacker.tld",
// and a subdomain the provider publishes still serves). Unlike a declared
// secret's empty Hosts — allowed everywhere — an EMPTY table allows NOTHING:
// a credential that names no host cannot cross, and its placeholder
// resolves nowhere by design.
func crossingHostAllowed(hosts []string, host string) bool {
	for _, pattern := range hosts {
		if pattern == "" {
			continue
		}
		if pattern == host || strings.HasSuffix(host, "."+pattern) {
			return true
		}
	}
	return false
}

// canonicalCrossingHost lower-cases and strips a trailing dot — the form
// the proxy's policy and the guard both key on.
func canonicalCrossingHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	return strings.TrimSuffix(host, ".")
}
