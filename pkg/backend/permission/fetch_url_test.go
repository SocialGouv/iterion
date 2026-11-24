package permission

import "testing"

// TestTheGateReadsAURLAsAWHATWGFetcherDoes: readFetchURL against the reading
// Node's `new URL()` gives — the parser claude_code's WebFetch runs. Every
// expected host and resolved URL below is the one Node v24 printed for the
// input (`host`, and `pathname` decoded and resolved), and every refusal is a
// URL Node throws on or a scheme no WebFetch backend fetches.
func TestTheGateReadsAURLAsAWHATWGFetcherDoes(t *testing.T) {
	for _, tc := range []struct {
		raw      string
		host     string // "" = the gate refuses the URL
		resolved string
	}{
		{"https:e.example/docs/../admin/users", "e.example", "https://e.example/admin/users"},
		{"https:/e.example/admin/users", "e.example", "https://e.example/admin/users"},
		{"https:///e.example/admin/users", "e.example", "https://e.example/admin/users"},
		{`https:\\e.example/x`, "e.example", "https://e.example/x"},
		{`https://e.example\docs\..\admin\users`, "e.example", "https://e.example/admin/users"},
		{"https://e.example/docs/.\t./admin/users", "e.example", "https://e.example/admin/users"},
		{" https://e.example/admin/users\n", "e.example", "https://e.example/admin/users"},
		{"HTTPS://E.example/admin/users", "e.example", "https://e.example/admin/users"},
		{"https://e.example:443/x", "e.example", "https://e.example/x"},
		{"https://e.example:0443/x", "e.example", "https://e.example/x"},
		{"https://e.example:/x", "e.example", "https://e.example/x"},
		{"https://e.example:8443/x", "e.example:8443", "https://e.example:8443/x"},
		{"http://e.example:443/x", "e.example:443", "http://e.example:443/x"},
		{"https://a@b@secret.example/x", "secret.example", "https://secret.example/x"},
		{"https://e.example%2F@evil.example/", "evil.example", "https://evil.example/"},
		{`https://evil.example\@e.example/`, "evil.example", "https://evil.example/@e.example/"},
		{"https://secret%2Eexample/x", "secret.example", "https://secret.example/x"},
		{"https://ｓecret.example/x", "secret.example", "https://secret.example/x"},
		{"https://secret。example/x", "secret.example", "https://secret.example/x"},
		{"https://İ.example/", "xn--i-9bb.example", "https://xn--i-9bb.example/"},
		{"https://a_b.example/", "a_b.example", "https://a_b.example/"},
		{"https://secret.example./x", "secret.example.", "https://secret.example./x"},
		{"http://0xa9fea9fe/", "169.254.169.254", "http://169.254.169.254/"},
		{"http://2852039166/", "169.254.169.254", "http://169.254.169.254/"},
		{"http://0251.0376.0251.0376/", "169.254.169.254", "http://169.254.169.254/"},
		{"http://192.168./", "192.0.0.168", "http://192.0.0.168/"},
		{"http://1.256/", "1.0.1.0", "http://1.0.1.0/"},
		{"http://0x/", "0.0.0.0", "http://0.0.0.0/"},
		{"https://[0:0::1]:0443/x", "[::1]", "https://[::1]/x"},
		{"https://[::ffff:127.0.0.1]/", "[::ffff:7f00:1]", "https://[::ffff:7f00:1]/"},
		{"https://[1:0:0:2::3:0]/", "[1::2:0:0:3:0]", "https://[1::2:0:0:3:0]/"},
		{"https://e.example", "e.example", "https://e.example/"},
		{"https://e.example?x", "e.example", "https://e.example/?x"},
		{`https://e.example/a?b\c#d`, "e.example", `https://e.example/a?b\c`},
		{"https://e.example/a#b?c", "e.example", "https://e.example/a"},
		{"https://e.example/%61dmin/x", "e.example", "https://e.example/admin/x"},
		{"https://e.example/a/.%2e/b", "e.example", "https://e.example/b"},
		{"https://e.example/a/b/..", "e.example", "https://e.example/a/"},
		{"HtTp://E.EXAMPLE:80/A/./B/../C", "e.example", "http://e.example/A/C"},
		{"https://e.example:99999/x", "", ""},
		{"https://e.example:12a/x", "", ""},
		{"https://user@/x", "", ""},
		{"https://:443/x", "", ""},
		{"https://", "", ""},
		{"https://a b.example/", "", ""},
		{"https://a%20b.example/", "", ""},
		{"https://a^b.example/", "", ""},
		{"https://a%zzb.example/", "", ""},
		{"https://e\x00.example/", "", ""},
		{"http://1.2.3.4.5/", "", ""},
		{"http://09/", "", ""},
		{"http://1..2/", "", ""},
		{"http://0x100000000/", "", ""},
		{"http://256.1.1.1/", "", ""},
		{"http://99999999999999999999999/", "", ""},
		{"http://a.0x1/", "", ""},
		{"https://[fe80::1%25eth0]/", "", ""},
		{"https://[::1/", "", ""},
		{"https://[1.2.3.4]/", "", ""},
		{"//e.example/x", "", ""},
		{"e.example/x", "", ""},
		{"1https://e.example/", "", ""},
		{"ftp://e.example/", "", ""},
		{"file:///etc/passwd", "", ""},
		{"data:text/plain,hi", "", ""},
	} {
		f, ok := readFetchURL(tc.raw)
		if tc.host == "" {
			if ok {
				t.Errorf("readFetchURL(%q) = host %q, want a refusal", tc.raw, f.host())
			}
			continue
		}
		if !ok {
			t.Errorf("readFetchURL(%q) refused, want host %q", tc.raw, tc.host)
			continue
		}
		if f.host() != tc.host || f.resolved() != tc.resolved {
			t.Errorf("readFetchURL(%q) = host %q, resolved %q; want %q, %q", tc.raw, f.host(), f.resolved(), tc.host, tc.resolved)
		}
	}
}

// TestADomainDenyMeetsTheHostAFetcherReaches: a `domain:` deny bounds the
// host the fetch reaches, whatever spelling of it the URL carries — no
// authority slashes, a `\` for a separator, case, a port (default or not),
// a percent-escape, a full-width or ideographic spelling, the root-zone dot,
// an IPv4 address written as a number.
func TestADomainDenyMeetsTheHostAFetcherReaches(t *testing.T) {
	p := mustPolicy(t, ModeDeny, []string{"WebFetch"}, nil, []string{"WebFetch(domain:secret.example)", "WebFetch(domain:169.254.169.254)"})
	for _, u := range []string{
		"https:secret.example/x",
		"https:secret.example/docs/../x",
		`https://secret.example\x`,
		"https://SECRET.example/x",
		"https://secret.example:443/x",
		"https://secret.example:8443/x",
		"https://u@secret.example/x",
		"https://secret%2Eexample/x",
		"https://ｓecret.example/x",
		"https://secret。example/x",
		"https://secret.example./x",
		`https://secret.example\@good.example/`,
		"http://0xa9fea9fe/latest/meta-data",
		"http://2852039166/",
		"http://0251.0376.0251.0376/",
	} {
		if got, rule := p.Evaluate("WebFetch", map[string]any{"url": u}); got != Deny {
			t.Errorf("deny domain:secret.example: WebFetch(%q) = %v (rule %q), want Deny", u, got, rule)
		}
	}
	for _, u := range []string{"https://good.example/x", "https://secret.example.good.example/x"} {
		if got, rule := p.Evaluate("WebFetch", map[string]any{"url": u}); got != Allow {
			t.Errorf("deny domain:secret.example: WebFetch(%q) = %v (rule %q), want Allow", u, got, rule)
		}
	}
}

// TestADenyOnAnIPv4AddressMeetsItsMappedIPv6Spelling: `[::ffff:a.b.c.d]` is
// an IPv6 host to a WHATWG reader and the IPv4 address a.b.c.d to the socket
// both backends' fetchers open, so every deny shape naming the IPv4 address —
// `domain:`, bare host, full URL, and the https URL claude_code upgrades an
// http one to — meets each spelling of the mapped address, on every backend's
// tool name and on a gate rebuilt from PolicyConfig.
func TestADenyOnAnIPv4AddressMeetsItsMappedIPv6Spelling(t *testing.T) {
	mapped := []string{
		"http://[::ffff:169.254.169.254]/latest/meta-data/",
		"http://[::ffff:a9fe:a9fe]/latest/meta-data/",
		"http://[0:0:0:0:0:ffff:169.254.169.254]/latest/meta-data/",
		"http://[::FFFF:A9FE:A9FE]:80/latest/meta-data/",
	}
	for _, deny := range []string{
		"WebFetch(domain:169.254.169.254)",
		"WebFetch(169.254.169.254)",
		"WebFetch(http://169.254.169.254/**)",
		"WebFetch(https://169.254.169.254/**)",
	} {
		engine := mustPolicy(t, ModeDeny, []string{"WebFetch"}, nil, []string{deny})
		child, err := NewPolicyFromConfig(engine.Config())
		if err != nil {
			t.Fatalf("NewPolicyFromConfig: %v", err)
		}
		for _, tool := range []string{"WebFetch", "web_fetch", "FetchURL"} {
			for _, u := range mapped {
				for side, p := range map[string]*Policy{"engine": engine, "rebuilt": child} {
					if got, rule := p.Evaluate(tool, map[string]any{"url": u}); got != Deny {
						t.Errorf("%s gate, deny %s: Evaluate(%s, %q) = %v (rule %q), want Deny", side, deny, tool, u, got, rule)
					}
				}
			}
		}
		// An IPv6 address that maps no IPv4 one is another host.
		if got, rule := engine.Evaluate("WebFetch", map[string]any{"url": "http://[2001:db8::a9fe:a9fe]/"}); got != Allow {
			t.Errorf("deny %s: an unmapped IPv6 host = %v (rule %q), want Allow", deny, got, rule)
		}
	}
}

// TestAnAllowReadsTheHostAFetcherReaches: the opening side reads the same
// host, never the URL's text: a `domain:` allow opens a spelling of its own
// host, and opens nothing a spelling of it hides another host behind.
func TestAnAllowReadsTheHostAFetcherReaches(t *testing.T) {
	p := mustPolicy(t, ModeDeny, []string{"WebFetch(domain:good.example)", "WebFetch(domain:192.168.*)"}, nil, nil)
	for _, u := range []string{
		"https:good.example/x",
		"https:/good.example/x",
		`https://good.example\x`,
		"https://GOOD.example:443/x",
		"https://u@good.example/x",
		"https://good%2Eexample/x",
		"http://192.168.1.1/",
	} {
		if got, rule := p.Evaluate("WebFetch", map[string]any{"url": u}); got != Allow {
			t.Errorf("allow domain:good.example: WebFetch(%q) = %v (rule %q), want Allow", u, got, rule)
		}
	}
	for _, u := range []string{
		`https://evil.example\@good.example/`, // a WHATWG fetcher reaches evil.example
		"http://192.168./",                    // a WHATWG fetcher reaches 192.0.0.168
		"https://good.example:8443/x",         // another port is another authority
	} {
		if got, rule := p.Evaluate("WebFetch", map[string]any{"url": u}); got != Deny {
			t.Errorf("allow domain:good.example: WebFetch(%q) = %v (rule %q), want Deny", u, got, rule)
		}
	}
}

// TestAnOpaqueURLThatClimbsIsReadByNoPattern: a URL with no authority slashes
// (`https:host/…`) is a fetch of that host, so its path is read — decoded —
// for the climb a full-URL pattern must not read past.
func TestAnOpaqueURLThatClimbsIsReadByNoPattern(t *testing.T) {
	const tree = "WebFetch(*github.com/SocialGouv/**)"
	p := mustPolicy(t, ModeDeny, []string{tree}, nil, nil)
	for _, u := range []string{
		"https:github.com/SocialGouv/../other/secret",
		"https:github.com/SocialGouv/%2e%2e/other/secret",
		"https:github.com/SocialGouv/.%2E/other/secret",
		"https:/github.com/SocialGouv/../other/secret",
		`https:github.com/SocialGouv/..\other\secret`,
	} {
		if got, rule := p.Evaluate("WebFetch", map[string]any{"url": u}); got != Deny {
			t.Errorf("allow %s: WebFetch(%q) = %v (rule %q), want Deny", tree, u, got, rule)
		}
	}
	if got, _ := p.Evaluate("WebFetch", map[string]any{"url": "https:github.com/SocialGouv/iterion"}); got != Allow {
		t.Errorf("allow %s: an opaque URL inside the tree = %v, want Allow", tree, got)
	}
}

// TestAURLTheGateCannotReadMeetsEveryScopedClosingRule: a URL readFetchURL
// refuses has no spelling the gate can trust to stand for where it lands, so
// every scoped deny and ask on WebFetch meets it, and no pattern opens it —
// only a bare, an unbounded or an exact allow does.
func TestAURLTheGateCannotReadMeetsEveryScopedClosingRule(t *testing.T) {
	unread := []string{
		"https://secret.example:99999/x",
		"ftp://e.example/admin/x",
		"javascript:alert(1)",
	}
	deny := mustPolicy(t, ModeDeny, []string{"WebFetch"}, nil, []string{"WebFetch(domain:secret.example)"})
	ask := mustPolicy(t, ModeDeny, []string{"WebFetch"}, []string{"WebFetch(domain:secret.example)"}, nil)
	pattern := mustPolicy(t, ModeDeny, []string{"WebFetch(*e.example/**)", "WebFetch(domain:secret.example*)"}, nil, nil)
	for _, u := range unread {
		call := map[string]any{"url": u}
		if got, rule := deny.Evaluate("WebFetch", call); got != Deny {
			t.Errorf("a scoped deny over unreadable %q = %v (rule %q), want Deny", u, got, rule)
		}
		if got, rule := ask.Evaluate("WebFetch", call); got != Ask {
			t.Errorf("a scoped ask over unreadable %q = %v (rule %q), want Ask", u, got, rule)
		}
		if got, rule := pattern.Evaluate("WebFetch", call); got != Deny {
			t.Errorf("a pattern allow over unreadable %q = %v (rule %q), want Deny", u, got, rule)
		}
		exact := mustPolicy(t, ModeDeny, []string{"WebFetch(" + u + ")"}, nil, nil)
		if got, rule := exact.Evaluate("WebFetch", call); got != Allow {
			t.Errorf("an exact allow naming unreadable %q = %v (rule %q), want Allow", u, got, rule)
		}
	}
	// No scoped closing rule: nothing to fail closed on.
	if got, _ := deny.Evaluate("WebFetch", map[string]any{"url": "https://good.example/"}); got != Allow {
		t.Errorf("a readable URL outside the deny = %v, want Allow", got)
	}
	bare := mustPolicy(t, ModeDeny, []string{"WebFetch"}, nil, []string{"Bash(rm:*)"})
	if got, _ := bare.Evaluate("WebFetch", map[string]any{"url": unread[0]}); got != Allow {
		t.Errorf("an unreadable URL under a bare allow and no WebFetch deny = %v, want Allow", got)
	}
}

// TestEveryBackendSpellingOfAFetchMeetsTheDeny: claude_code sends
// `WebFetch{url}`, claw `web_fetch{url}`, kimi `FetchURL{url}`, and a hook
// child or sandboxed runner judges with a gate rebuilt from PolicyConfig —
// each reaches the same verdict on a host-less spelling of a denied host.
func TestEveryBackendSpellingOfAFetchMeetsTheDeny(t *testing.T) {
	engine := mustPolicy(t, ModeDeny, []string{"WebFetch"}, nil, []string{"WebFetch(domain:secret.example)", "WebFetch(https://e.example/admin/**)"})
	child, err := NewPolicyFromConfig(engine.Config())
	if err != nil {
		t.Fatalf("NewPolicyFromConfig: %v", err)
	}
	for _, tool := range []string{"WebFetch", "web_fetch", "FetchURL"} {
		for _, u := range []string{"https:secret.example/x", `https://e.example\docs\..\admin\users`} {
			for side, p := range map[string]*Policy{"engine": engine, "rebuilt": child} {
				if got, rule := p.Evaluate(tool, map[string]any{"url": u}); got != Deny {
					t.Errorf("%s gate: Evaluate(%s, %q) = %v (rule %q), want Deny", side, tool, u, got, rule)
				}
			}
		}
	}
}
