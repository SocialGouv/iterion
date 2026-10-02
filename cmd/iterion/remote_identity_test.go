package main

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

// TestMemberPathKeepsTheIdInItsSegment holds the property that makes a
// members write land where the operator named it.
//
// An id is operator input reaching a URL PATH. Concatenated raw, Go's
// ServeMux cleans the dot-segments and answers 307, and net/http follows a
// 307 preserving method AND body — so `teams members add '../../t-other/
// members/u-9' owner --team t-mine` wrote `owner` on t-other and exited 0.
// The server re-authorises against the post-redirect path, so nothing was
// granted that the caller did not already hold; what was lost is the
// operator's ability to trust the target they typed.
func TestMemberPathKeepsTheIdInItsSegment(t *testing.T) {
	const base = "/api/teams/t-mine/members"
	for _, id := range []string{
		"../../t-other/members/u-9",
		"..%2f..%2ft-other",
		"u1#fragment",
		"u1?foo=bar",
		"u1/extra",
		"a b",
	} {
		got := memberPath(base, id)
		if !strings.HasPrefix(got, base+"/") {
			t.Fatalf("memberPath(%q) = %q, left its collection", id, got)
		}
		// One segment beyond the base, and it decodes back to the input —
		// the escaping must not lose the id either.
		rest := strings.TrimPrefix(got, base+"/")
		if strings.Contains(rest, "/") {
			t.Fatalf("memberPath(%q) = %q, spans more than one segment", id, got)
		}
		back, err := url.PathUnescape(rest)
		if err != nil {
			t.Fatalf("memberPath(%q) = %q, does not unescape: %v", id, got, err)
		}
		if back != id {
			t.Fatalf("memberPath(%q) round-trips to %q", id, back)
		}
		// And the URL parser agrees the path is what we built, with no
		// dot-segment left for a mux to clean.
		u, err := url.Parse("http://x" + got)
		if err != nil {
			t.Fatalf("memberPath(%q) = %q: %v", id, got, err)
		}
		if u.EscapedPath() != got || u.RawQuery != "" || u.Fragment != "" {
			t.Fatalf("memberPath(%q) = %q parsed as path=%q query=%q frag=%q",
				id, got, u.EscapedPath(), u.RawQuery, u.Fragment)
		}
	}
}

// TestRoleBodyIsJSON pins that the request body is JSON for every byte a
// shell can pass. fmt.Sprintf("%q") emits GO string syntax — `\a`, `\v` and
// `\xNN` are valid Go and invalid JSON — so a role carrying a control byte
// or invalid UTF-8 produced a body the server could not decode, and the
// operator read a JSON parse error instead of "invalid role".
func TestRoleBodyIsJSON(t *testing.T) {
	for _, role := range []string{
		"member",
		"adm\ain",          // BEL: valid Go escape, invalid JSON
		"adm\vin",          // vertical tab: same
		"adm\x00in",        // NUL
		`member","x":true`, // the injection attempt
		`back\slash`,       // a literal backslash
		"quote\"inside",    // a literal quote
		"héllo",            // multi-byte
		strings.Repeat("a", 4096),
	} {
		body, err := roleBody(role)
		if err != nil {
			t.Fatalf("roleBody(%q): %v", role, err)
		}
		var got map[string]any
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("roleBody(%q) = %s, not JSON: %v", role, body, err)
		}
		if len(got) != 1 {
			t.Fatalf("roleBody(%q) = %s, carries %d keys — the value escaped its field",
				role, body, len(got))
		}
		// Every accepted input round-trips EXACTLY. Anything that would not
		// is refused above, not quietly rewritten.
		if v, ok := got["role"].(string); !ok {
			t.Fatalf("roleBody(%q) = %s, role is not a string", role, body)
		} else if v != role {
			t.Fatalf("roleBody(%q) round-trips to %q", role, v)
		}
	}
}

// TestRoleBodyRefusesInvalidUTF8 is the half that makes the encoder swap an
// improvement rather than a trade. json.Marshal does NOT fail on invalid
// UTF-8 — it substitutes U+FFFD — so without this check the CLI would send a
// different string than the operator typed and exit 0, where the old `%q`
// form at least produced a body the server rejected. A silent substitution is
// worse than a confusing error.
func TestRoleBodyRefusesInvalidUTF8(t *testing.T) {
	for _, role := range []string{
		"adm\xe9in",    // a lone continuation byte
		"\xff",         // never valid anywhere in UTF-8
		"ok\xc3",       // a truncated two-byte sequence
		"\xed\xa0\x80", // a surrogate half, which UTF-8 forbids
	} {
		if _, err := roleBody(role); err == nil {
			t.Fatalf("roleBody(%q) accepted invalid UTF-8 — it would be rewritten in flight", role)
		}
	}
	// The control: a legitimate multi-byte string is NOT refused.
	if _, err := roleBody("héllo-wörld-日本語"); err != nil {
		t.Fatalf("roleBody refused valid multi-byte text: %v", err)
	}
}
