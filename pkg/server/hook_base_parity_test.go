package server

import "testing"

// The server's half of the hook-base parity table. The studio announces the
// delivery address a provisioning preview reports (`hook_base_url`) and warns
// instead when that base is not an address a forge can dial; the server hands
// the very same string to the forge. So the two readings are held to ONE table:
// these inputs, in this order, are the inputs of
// studio/src/lib/hookBase.test.ts, which carries the studio's verdicts.
//
// A base the studio accepts and this function refuses is the expensive
// direction: the panel prints it in monospace, Enable stays live, and the
// forge answers 422 (the branch hookRefusedDetail writes for exactly that
// case). The studio being the stricter of the two only costs a warning naming
// the setting to fix.
var hookBaseParityTable = []struct {
	base string
	want bool
}{
	// Dialable: a scheme, two slashes, a host — with or without a port, a
	// prefix path, credentials, a fragment, a query, an IPv6 literal, and in
	// any case.
	{"https://iterion.example.org", true},
	{"http://iterion.example.org:8080", true},
	{"https://iterion.example.org/", true},
	{"https://iterion.example.org/behind/a/prefix", true},
	{"HTTPS://ITERION.EXAMPLE.ORG", true},
	{"Https://iterion.example.org", true},
	{"https://user:pass@iterion.example.org", true},
	{"https://[::1]:8080", true},
	{"https://[2001:db8::1]", true},
	{"https://iterion.example.org:", true},
	{"https://itérion.example.org", true},
	// url.Parse never validates the punycode of an `xn--` label; a WHATWG URL
	// parser does, against an IDNA table that moves between runtimes. This is
	// the one row the studio may answer differently (its test names it).
	{"https://xn--itrion-fsa.example.org", true},
	{"HTTP://iterion.example.org", true},
	{"https://iterion.example.org#frag", true},
	{"https://iterion.example.org?q=1", true},
	{"http://@host", true},
	{"https://host:0", true},
	// Nothing to dial: no address at all, a path, a host with no scheme, a
	// protocol-relative address, a scheme no forge posts over, a scheme with
	// no host, an unparsable port, an encoded space for a host.
	{"", false},
	{"   ", false},
	{"/iterion", false},
	{"iterion.example.org", false},
	{"//iterion.example.org", false},
	{"ftp://iterion.example.org", false},
	{"javascript:alert(1)", false},
	{"https://", false},
	{"http://", false},
	{"mailto:a@b.c", false},
	{"file:///etc/passwd", false},
	{"https://iterion.example.org:abc", false},
	{"https://%20host", false},
	// What a WHATWG URL parser would repair and this one will not: one slash
	// read as two, a scheme with none, backslashes, and whitespace trimmed or
	// stripped from anywhere in the string.
	{"http:/x", false},
	{"http:/iterion.example.org", false},
	{"https:/iterion.example.org", false},
	{"https:iterion.example.org", false},
	{"https://iterion.example.org ", false},
	{" https://iterion.example.org", false},
	{"\thttps://iterion.example.org", false},
	{"https://iterion.example.org\n", false},
	{"https://iterion.ex\nample.org", false},
	{"https:\\\\iterion.example.org", false},
	// A control character the WHATWG parser strips from the ends, and this
	// one refuses anywhere.
	{"https://iterion.example.org\x01", false},
	{"\x1fhttps://iterion.example.org", false},
	// What this parser refuses and a WHATWG one repairs or accepts: a third
	// slash (no authority), a backslash in the path, an escaped ASCII byte in
	// the host, a malformed escape.
	{"https:///iterion.example.org", false},
	{"https://iterion.example.org\\path", false},
	{"https://%41iterion.example.org", false},
	{"https://iterion.example.org/%zz", false},
}

func TestAbsoluteHTTPURL_ParityTable(t *testing.T) {
	for _, tc := range hookBaseParityTable {
		if got := absoluteHTTPURL(tc.base); got != tc.want {
			t.Errorf("absoluteHTTPURL(%q) = %v, want %v", tc.base, got, tc.want)
		}
	}
}

func TestHookBaseParityTable_HoldsEveryShape(t *testing.T) {
	// A row dropped on one side is a shape nobody reads any more: the counts
	// are pinned on both halves (studio/src/lib/hookBase.test.ts).
	if len(hookBaseParityTable) != 46 {
		t.Fatalf("parity table has %d rows, want 46 — the studio half pins the same count", len(hookBaseParityTable))
	}
	seen := make(map[string]bool, len(hookBaseParityTable))
	refused := 0
	for _, tc := range hookBaseParityTable {
		if seen[tc.base] {
			t.Errorf("duplicate parity row %q", tc.base)
		}
		seen[tc.base] = true
		if !tc.want {
			refused++
		}
	}
	if refused != 29 {
		t.Errorf("table refuses %d bases, want 29", refused)
	}
}
