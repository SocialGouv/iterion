package credheader_test

import (
	"slices"
	"strings"
	"testing"

	clawapi "github.com/SocialGouv/claw-code-go/pkg/api"
	"github.com/SocialGouv/iterion/internal/credheader"
)

// The block this package reads is the one the CLI reads, so the walk is pinned
// against the CLI's own parser rather than against a second description of the
// format: claw's ParseCustomHeaders is that reader (vendored, first-party). On
// every block it ACCEPTS, "which lines authenticate" must mean the same thing
// on both sides of the wire — a mirror written from the documentation drifts at
// the edges the documentation does not mention (a \r line ending, a blank line,
// a colon in the value, the word in a value instead of a name).
//
// On a block the parser REFUSES there is no mirror to hold: the reading is per
// LINE on purpose, and each such block says here what it owes. The kept text
// keeps the malformed line, so the CLI goes on refusing the block — the
// operator's own to fix — instead of being handed a half-repaired one.
func TestStripCredentialLines_AgreesWithTheCLIsParser(t *testing.T) {
	cases := []struct {
		raw string
		// On a block the CLI's parser REFUSES: what this package owes,
		// written per input rather than derived, so the arm discriminates
		// instead of restating the code under test. wantKept keeps the lines
		// the operator has to go and fix, so that parser goes on refusing the
		// block instead of being handed a half-repaired one.
		wantRefused []string
		wantKept    string
	}{
		{raw: ""},
		{raw: "   "},
		{raw: "x-tenant: acme"},
		{raw: "User-Agent: my-tool/1.0"},
		{raw: "Authorization: Bearer sk-operator-gateway"},
		{raw: "authorization: Bearer sk-lower"},
		{raw: "  Authorization  :  Bearer sk-spaced"},
		{raw: "x-api-key: sk-ambient-key"},
		{raw: "X-API-Key: sk-ambient-upper"},
		{raw: "api-key: sk-foundry-shape"},
		{raw: "x-tenant: acme\nAuthorization: Bearer sk-second-line"},
		{raw: "Authorization: Bearer sk-crlf\r\nx-tenant: acme"},
		{raw: "x-tenant: acme\n\nAuthorization: Bearer sk-after-a-blank-line"},
		{raw: "Authorization: Bearer sk-a\nx-api-key: sk-b\nx-tenant: acme"},
		{raw: "x-note: see the authorization: guide"},
		{raw: "X-Authorization: Bearer sk-other-header"},
		{raw: "x-proxy: https://gw.example.test/authorization: no"},
		// The refused half: blocks the parser rejects, with and without a
		// well-formed credential line beside the bad one.
		{raw: "Authorization", wantRefused: nil, wantKept: "Authorization"},
		{raw: "nocolon", wantRefused: nil, wantKept: "nocolon"},
		{raw: "Authorization: Bearer sk-x\nnocolon", wantRefused: []string{"authorization"}, wantKept: "nocolon"},
		{raw: "# our gateway\nx-api-key: sk-x", wantRefused: []string{"x-api-key"}, wantKept: "# our gateway"},
		{raw: "Authorization: Bearer sk-x\n:novalue", wantRefused: []string{"authorization"}, wantKept: ":novalue"},
	}
	for _, tc := range cases {
		t.Run(strings.ReplaceAll(tc.raw, "\n", "|"), func(t *testing.T) {
			kept, refused := credheader.StripCredentialLines(tc.raw)
			headers, err := clawapi.ParseCustomHeaders(tc.raw)
			if err != nil {
				if !slices.Equal(refused, tc.wantRefused) {
					t.Errorf("StripCredentialLines(%q) refused %v on a block the CLI's parser refuses (%v), want %v",
						tc.raw, refused, err, tc.wantRefused)
				}
				if kept != tc.wantKept {
					t.Errorf("StripCredentialLines(%q) kept %q, want %q — the lines the CLI refuses stay the operator's to fix",
						tc.raw, kept, tc.wantKept)
				}
				return
			}
			var want []string
			for name := range headers {
				if credheader.IsCredentialName(name) {
					want = append(want, strings.ToLower(strings.TrimSpace(name)))
				}
			}
			slices.Sort(want)
			if !slices.Equal(refused, want) {
				t.Errorf("StripCredentialLines(%q) refused %v, the CLI's parser carries %v of those names (headers %v)",
					tc.raw, refused, want, headers)
			}
			// What crossed, read by the CLI's own parser: exactly the lines
			// that do not authenticate, with their values intact.
			keptHeaders, keptErr := clawapi.ParseCustomHeaders(kept)
			if keptErr != nil {
				t.Fatalf("the CLI's parser refuses the kept text %q: %v", kept, keptErr)
			}
			for name, value := range headers {
				got, present := keptHeaders[name]
				if credheader.IsCredentialName(name) {
					if present {
						t.Errorf("%s survived the strip of %q", name, tc.raw)
					}
					continue
				}
				if !present || got != value {
					t.Errorf("%s = %q, present=%v in the kept text %q, want the operator's own %q", name, got, present, kept, value)
				}
			}
		})
	}
}

// Nothing refused means nothing touched: the operator's block crosses byte for
// byte, so a value this package cannot judge is never reformatted behind their
// back.
func TestStripCredentialLines_KeepsABlockWithNoCredentialByteForByte(t *testing.T) {
	for _, raw := range []string{"", "   ", "x-tenant: acme\r\nUser-Agent: my-tool/1.0", "nocolon\n\nx-note: see the authorization: guide"} {
		kept, refused := credheader.StripCredentialLines(raw)
		if len(refused) != 0 {
			t.Errorf("StripCredentialLines(%q) refused %v, want nothing", raw, refused)
		}
		if kept != raw {
			t.Errorf("StripCredentialLines(%q) rewrote the block to %q", raw, kept)
		}
	}
}

// The refusal names the header, never its value: it is read into a log line
// the operator sees, and the value of a credential line IS the credential.
func TestStripCredentialLines_RefusalNamesTheHeaderNeverTheValue(t *testing.T) {
	const secret = "sk-ambient-gateway-credential"
	kept, refused := credheader.StripCredentialLines("Authorization: Bearer " + secret + "\nx-api-key: " + secret)
	if !slices.Equal(refused, []string{"authorization", "x-api-key"}) {
		t.Fatalf("refused = %v, want both credential names, sorted", refused)
	}
	for _, name := range refused {
		if strings.Contains(name, secret) {
			t.Errorf("the refusal %q carries the credential's value", name)
		}
	}
	if strings.Contains(kept, secret) {
		t.Errorf("the kept text %q carries the credential's value", kept)
	}
}

// CarriesName answers on the NAME alone — the claim the delegate's remediation
// makes about an ambient Authorization line — and is case-insensitive, as the
// CLI's parser and HTTP itself are.
func TestCarriesName_ReadsTheNameAlone(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{"Authorization: Bearer sk-x", true},
		{"authorization: Bearer sk-x", true},
		{"  AUTHORIZATION : Bearer sk-x", true},
		{"x-tenant: acme\nAuthorization: Bearer sk-x", true},
		{"x-note: see the authorization: guide", false},
		{"X-Authorization: Bearer sk-x", false},
		{"Authorization", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := credheader.CarriesName(tc.raw, credheader.NameAuthorization); got != tc.want {
			t.Errorf("CarriesName(%q, %q) = %v, want %v", tc.raw, credheader.NameAuthorization, got, tc.want)
		}
	}
}

// The name set is the subject of the vendored-producer guard in
// pkg/backend/model: keep it non-empty and lower-case here, or that guard
// compares against nothing.
func TestCredentialNamesAreLowerCaseAndSorted(t *testing.T) {
	names := credheader.CredentialNames()
	if len(names) == 0 {
		t.Fatal("no credential header name: the strip has nothing to refuse")
	}
	if !slices.IsSorted(names) {
		t.Errorf("CredentialNames() = %v, want them sorted", names)
	}
	for _, name := range names {
		if name != strings.ToLower(name) {
			t.Errorf("%q is not lower-case: the guard that derives these names compares them lower-cased", name)
		}
		if !credheader.IsCredentialName(strings.ToUpper(name)) {
			t.Errorf("IsCredentialName(%q) is false under another spelling: HTTP header names are case-insensitive", strings.ToUpper(name))
		}
	}
}
