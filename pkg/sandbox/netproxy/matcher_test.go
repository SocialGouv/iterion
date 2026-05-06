package netproxy

import "testing"

func TestCompileRejectsInvalidMode(t *testing.T) {
	_, err := Compile(Mode("nope"), nil)
	if err == nil {
		t.Fatal("expected error for invalid mode")
	}
}

func TestCompileRejectsEmbeddedWildcard(t *testing.T) {
	_, err := Compile(ModeAllowlist, []string{"a*b.com"})
	if err == nil {
		t.Fatal("expected rejection of embedded wildcard")
	}
}

func TestCompileRejectsTrailingWildcard(t *testing.T) {
	_, err := Compile(ModeAllowlist, []string{"github.com.*"})
	if err == nil {
		t.Fatal("expected rejection of trailing wildcard")
	}
}

func TestCompileRejectsBareNegation(t *testing.T) {
	_, err := Compile(ModeAllowlist, []string{"!"})
	if err == nil {
		t.Fatal("expected rejection of bare !")
	}
}

func TestCompileRejectsBadCIDR(t *testing.T) {
	_, err := Compile(ModeAllowlist, []string{"10.0.0.0/99"})
	if err == nil {
		t.Fatal("expected rejection of invalid CIDR")
	}
}

func TestAllowlistAllowsListedHosts(t *testing.T) {
	p, err := Compile(ModeAllowlist, []string{"api.anthropic.com", "**.github.com"})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]bool{
		"api.anthropic.com":     true,
		"github.com":            true, // **.github.com matches the bare host
		"raw.github.com":        true,
		"a.b.github.com":        true,
		"evil.site":             false,
		"":                      false,
		"api.anthropic.com:443": true, // port stripped
	}
	for host, want := range cases {
		if got := p.Allow(host); got != want {
			t.Errorf("Allow(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestSingleStarMatchesOneLabel(t *testing.T) {
	p, _ := Compile(ModeAllowlist, []string{"*.example.com"})
	cases := map[string]bool{
		"foo.example.com": true,
		"bar.example.com": true,
		"a.b.example.com": false, // two labels
		"example.com":     false, // zero labels
	}
	for host, want := range cases {
		if got := p.Allow(host); got != want {
			t.Errorf("Allow(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestDoubleStarMatchesAnyLabels(t *testing.T) {
	p, _ := Compile(ModeAllowlist, []string{"**.example.com"})
	cases := map[string]bool{
		"example.com":     true,
		"foo.example.com": true,
		"a.b.example.com": true,
		"foo.evil.site":   false,
	}
	for host, want := range cases {
		if got := p.Allow(host); got != want {
			t.Errorf("Allow(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestDenylistDefaultsToAllow(t *testing.T) {
	p, _ := Compile(ModeDenylist, []string{"!**.evil.site", "!169.254.169.254"})
	cases := map[string]bool{
		"github.com":      true,
		"evil.site":       false,
		"sub.evil.site":   false,
		"169.254.169.254": false,
		"169.254.169.255": true, // not in the denylist
	}
	for host, want := range cases {
		if got := p.Allow(host); got != want {
			t.Errorf("Allow(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestLastMatchWins(t *testing.T) {
	// allow github.com, then later deny it — last match wins.
	p, _ := Compile(ModeAllowlist, []string{"github.com", "!github.com"})
	if p.Allow("github.com") {
		t.Error("last-match-wins: expected deny")
	}
	// flip the order
	p2, _ := Compile(ModeAllowlist, []string{"!github.com", "github.com"})
	if !p2.Allow("github.com") {
		t.Error("last-match-wins: expected allow when allow rule comes last")
	}
}

func TestOpenModeAcceptsEverything(t *testing.T) {
	p, _ := Compile(ModeOpen, nil)
	for _, host := range []string{"github.com", "evil.site", "1.2.3.4", ""} {
		if !p.Allow(host) {
			t.Errorf("open mode denied %q", host)
		}
	}
}

func TestCIDRMatching(t *testing.T) {
	p, _ := Compile(ModeDenylist, []string{"!10.0.0.0/8"})
	cases := map[string]bool{
		"10.0.0.1":       false,
		"10.255.255.255": false,
		"11.0.0.0":       true,
		"github.com":     true, // not an IP
	}
	for host, want := range cases {
		if got := p.Allow(host); got != want {
			t.Errorf("Allow(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestIPLiteralRequiresExplicitAllow(t *testing.T) {
	p, _ := Compile(ModeAllowlist, []string{"github.com"})
	// IP not listed → denied even though listed hostname maps to *some* IP.
	if p.Allow("1.2.3.4") {
		t.Error("IP literal should be denied in allowlist mode without explicit rule")
	}
	// Explicit IP rule accepts.
	p2, _ := Compile(ModeAllowlist, []string{"1.2.3.4"})
	if !p2.Allow("1.2.3.4") {
		t.Error("explicit IP rule should allow")
	}
}

func TestCanonicalHostStripsPort(t *testing.T) {
	cases := map[string]string{
		"github.com:443":      "github.com",
		"github.com":          "github.com",
		"  GitHub.com:8080  ": "github.com",
		"[::1]:443":           "::1",
		"":                    "",
	}
	for in, want := range cases {
		if got := canonicalHost(in); got != want {
			t.Errorf("canonicalHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPresetIterionDefault(t *testing.T) {
	rules, ok := PresetRules(PresetIterionDefault)
	if !ok {
		t.Fatal("preset iterion-default not registered")
	}
	if len(rules) == 0 {
		t.Fatal("preset is empty")
	}
	p, err := Compile(ModeAllowlist, rules)
	if err != nil {
		t.Fatalf("preset rules failed to compile: %v", err)
	}
	allowed := []string{
		"api.anthropic.com",
		"api.openai.com",
		"registry.npmjs.org",
		"github.com",
		"raw.github.com",
		"pypi.org",
		"proxy.golang.org",
	}
	for _, h := range allowed {
		if !p.Allow(h) {
			t.Errorf("preset should allow %q", h)
		}
	}
	denied := []string{
		"evil.site",
		"169.254.169.254",
	}
	for _, h := range denied {
		if p.Allow(h) {
			t.Errorf("preset should NOT allow %q", h)
		}
	}
}
