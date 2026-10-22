package secrets

import (
	"context"
	"errors"
	"math/rand"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/SocialGouv/iterion/pkg/store"
)

// A gateway key is sealed with its base URL in the AAD: the same blob on a
// row repointed at another host opens nowhere, so a store-side edit can
// never make the key travel to a host its owner did not register. Vendor
// keys keep the id-only AAD and the SealAPIKey form.
func TestSealAPIKeyFor_AGatewayKeyIsBoundToItsBaseURL(t *testing.T) {
	sealer := newSealer(t)
	k := ApiKey{ID: "k-gw", Provider: ProviderOpenAICompatible, BaseURL: "https://gw.example.test"}
	sealed, err := SealAPIKeyFor(sealer, k, []byte("sk-gateway-virtual-key-000"))
	if err != nil {
		t.Fatal(err)
	}
	k.SealedSecret = sealed
	if pt, err := OpenApiKey(sealer, k); err != nil || string(pt) != "sk-gateway-virtual-key-000" {
		t.Fatalf("open on the registered host: %q %v", pt, err)
	}
	repointed := k
	repointed.BaseURL = "https://attacker.example.test"
	if _, err := OpenApiKey(sealer, repointed); err == nil {
		t.Fatal("a repointed gateway row opened — the AAD does not bind the base URL")
	}
	if _, err := SealAPIKeyFor(sealer, ApiKey{ID: "k-gw", Provider: ProviderOpenAICompatible}, []byte("sk-gateway-virtual-key-000")); err == nil {
		t.Fatal("a gateway key sealed without a base URL")
	}
	vendor := ApiKey{ID: "k-ant", Provider: ProviderAnthropic}
	sealedV, err := SealAPIKey(sealer, vendor.ID, []byte("sk-ant-vendor"))
	if err != nil {
		t.Fatal(err)
	}
	vendor.SealedSecret = sealedV
	vendor.BaseURL = "https://ignored.example.test" // never part of a vendor key's AAD
	if pt, err := OpenApiKey(sealer, vendor); err != nil || string(pt) != "sk-ant-vendor" {
		t.Fatalf("vendor key must keep the id-only AAD: %q %v", pt, err)
	}
}

// One token on two gateways is two identities; every other provider keeps
// sha256(secret), so no existing fingerprint moves.
func TestAPIKeyFingerprint_GatewayIdentityFoldsTheBaseURL(t *testing.T) {
	a := APIKeyFingerprint(ProviderOpenAICompatible, "sk-same-token-0000", "https://gw-a.example.test")
	b := APIKeyFingerprint(ProviderOpenAICompatible, "sk-same-token-0000", "https://gw-b.example.test")
	if a == b || len(a) != 16 {
		t.Fatalf("two gateways with one token must have two 16-hex identities: %q %q", a, b)
	}
	if APIKeyFingerprint(ProviderOpenAICompatible, "sk-same-token-0000", "https://gw-a.example.test") != a {
		t.Fatal("the gateway fingerprint is not deterministic")
	}
	for _, p := range []Provider{ProviderAnthropic, ProviderOpenAI, ProviderZAI} {
		if APIKeyFingerprint(p, "sk-same-token-0000", "https://gw-a.example.test") != FingerprintSHA256("sk-same-token-0000") {
			t.Fatalf("provider %s must keep sha256(secret) whatever base URL is passed", p)
		}
	}
	if APIKeyFingerprint(ProviderOpenAI, "sk-same-token-0000", "") == a {
		t.Fatal("a vendor key and a gateway key with one token must not share an identity")
	}
}

func TestValidateGatewayBaseURL(t *testing.T) {
	ok := map[string]string{
		"https://gw.example.test":            "https://gw.example.test",
		"https://gw.example.test/":           "https://gw.example.test",
		"https://gw.example.test/v1":         "https://gw.example.test",
		"https://gw.example.test/v1/":        "https://gw.example.test",
		"https://gw.example.test/litellm/v1": "https://gw.example.test/litellm",
		" https://gw.example.test:8443/x ":   "https://gw.example.test:8443/x",
		// One endpoint, one identity: host case, a trailing dot, the
		// default port and a doubled /v1 all fold to the same string.
		"https://GW.Example.TEST":       "https://gw.example.test",
		"https://gw.example.test.":      "https://gw.example.test",
		"https://gw.example.test:443":   "https://gw.example.test",
		"https://gw.example.test/v1/v1": "https://gw.example.test",
		"HTTPS://gw.example.test":       "https://gw.example.test",
		"https://203.0.113.10:8080/v1":  "https://203.0.113.10:8080",
		"https://[2001:db8::10]/v1":     "https://[2001:db8::10]",
		// Literal addresses print as net.IP does, ports as numbers, names
		// in punycode: one endpoint, one spelling.
		"https://[2001:DB8:0:0::10]":    "https://[2001:db8::10]",
		"https://[::ffff:203.0.113.10]": "https://203.0.113.10",
		"https://gw.example.test:08080": "https://gw.example.test:8080",
		"https://bücher.example":        "https://xn--bcher-kva.example",
	}
	for in, want := range ok {
		got, err := ValidateGatewayBaseURL(in)
		if err != nil || got != want {
			t.Errorf("%q → %q %v, want %q", in, got, err, want)
			continue
		}
		// Idempotent on its own output: re-sending a stored URL is a
		// no-op, never a "change" that asks for the secret again.
		if again, err := ValidateGatewayBaseURL(got); err != nil || again != got {
			t.Errorf("%q → %q → %q %v: not idempotent", in, got, again, err)
		}
	}
	for _, bad := range []string{"", "http://gw.example.test", "gw.example.test", "https://", "https://user:pw@gw.example.test", "https://gw.example.test/?x=1", "https://gw.example.test/#frag", "ftp://gw.example.test",
		// Addresses no run may dial, and names a cluster or a cloud reserves.
		"https://127.0.0.1:8443", "https://localhost", "https://10.1.2.3", "https://169.254.169.254/latest", "https://[::1]", "https://0.0.0.0", "https://metadata.google.internal", "https://kubernetes.default.svc", "https://kubernetes.default", "https://gw.svc.cluster.local", "https://printer.local", "https://api.localhost",
		// Ports outside the range, empty and dot segments.
		"https://gw.example.test:0", "https://gw.example.test:99999", "https://gw.example.test//v1", "https://gw.example.test/a/../b", "https://gw.example.test/./x",
		// Percent-escapes (a zoned IPv6 literal rides one), whitespace and
		// control characters anywhere, a bare "metadata" or "kubernetes".
		"https://[fe80::1%25eth0]/", "https://[::1%25lo]/", "https://gw.example.test/a%3Fb", "https://gw.example.test/v1%2F", "https://gw.example.test/a b", "https://gw.example.test/\tx", "https://metadata/computeMetadata", "https://kubernetes/api"} {
		if _, err := ValidateGatewayBaseURL(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	// A refusal never carries the value: a secret pasted into the field —
	// as a scheme, userinfo, port, bracketed host, path, query, or after a
	// space or a newline — must not ride the reason into the audit trail or
	// the log, and must not be stored as the URL either.
	for _, pasted := range []string{"https://gw.example.test/\nsk-live-PASTED-SECRET-42", "https://gw.example.test/ sk-live-PASTED-SECRET-42", "sk-live-PASTED-SECRET-42://x", "https://sk-live-PASTED-SECRET-42@gw.example.test", "https://gw.example.test/?sk-live-PASTED-SECRET-42", "https://gw.example.test:sk-live-PASTED-SECRET-42/", "https://[sk-live-PASTED-SECRET-42]/", "https://gw.example.test/%zz-PASTED"} {
		got, err := ValidateGatewayBaseURL(pasted)
		if err == nil {
			t.Errorf("%q accepted as %q", pasted, got)
		} else if strings.Contains(err.Error(), "PASTED") {
			t.Errorf("the refusal echoes the value: %v", err)
		}
	}
}

// The AAD delimiter is kept out of ids: with a NUL inside an id, a vendor
// row "k1\x00https://gw" would spell the AAD of gateway row "k1" and open a
// blob sealed for another provider.
func TestApiKeyAAD_AnIdCannotSpellAnotherRowsAAD(t *testing.T) {
	sealer := newSealer(t)
	gw := ApiKey{ID: "k1", Provider: ProviderOpenAICompatible, BaseURL: "https://gw.example.test"}
	sealed, err := SealAPIKeyFor(sealer, gw, []byte("sk-gateway-virtual-key-000"))
	if err != nil {
		t.Fatal(err)
	}
	forged := ApiKey{ID: "k1\x00https://gw.example.test", Provider: ProviderAnthropic, SealedSecret: sealed}
	if _, err := OpenApiKey(sealer, forged); err == nil {
		t.Fatal("a vendor row whose id carries the delimiter opened a gateway blob")
	}
	if _, err := SealAPIKeyFor(sealer, forged, []byte("x")); err == nil {
		t.Fatal("an id carrying the delimiter was sealed")
	}
}

// A base URL never travels alone: the AAD binds it, so a row whose URL
// moved without its blob opens nowhere. The store refuses the patch.
func TestApiKeyPatch_ABaseURLTravelsWithTheSecret(t *testing.T) {
	st := NewMemoryApiKeyStore()
	ctx := store.WithTenant(t.Context(), "t")
	k := ApiKey{ID: NewApiKeyID(), TenantID: "t", ScopeTeamID: "t", Provider: ProviderOpenAICompatible, BaseURL: "https://gw.example.test", Name: "k", SealedSecret: []byte("v1"), Fingerprint: "fp1"}
	if err := st.Create(ctx, k); err != nil {
		t.Fatal(err)
	}
	moved := "https://gw2.example.test"
	if _, err := st.Patch(ctx, k.ID, ApiKeyPatch{BaseURL: &moved}); !errors.Is(err, ErrApiKeyPatchInvalid) {
		t.Fatalf("a base URL without its secret = %v, want ErrApiKeyPatchInvalid", err)
	}
	if back, _ := st.Get(ctx, k.ID); back.BaseURL != "https://gw.example.test" {
		t.Fatalf("the refused move landed: %+v", back)
	}
	got, err := st.Patch(ctx, k.ID, ApiKeyPatch{BaseURL: &moved, Secret: &ApiKeySecretPatch{Sealed: []byte("v2"), Last4: "2222", Fingerprint: "fp2"}, ExpectSealed: []byte("v1")})
	if err != nil || got.BaseURL != moved || string(got.SealedSecret) != "v2" {
		t.Fatalf("a move with its secret must land: %+v %v", got, err)
	}
}

func TestValidateAPIKeyShape_GatewayKeyIsABearerOfSomeLength(t *testing.T) {
	if err := ValidateAPIKeyShape(ProviderOpenAICompatible, "sk-litellm-virtual-key-1234567890"); err != nil {
		t.Fatalf("a real virtual key refused: %v", err)
	}
	if err := ValidateAPIKeyShape(ProviderOpenAICompatible, "EMPTY"); err == nil || !strings.Contains(err.Error(), "too short") {
		t.Fatalf("a placeholder must be refused as too short: %v", err)
	}
	// A self-hosted server's documented example key is a real key.
	if err := ValidateAPIKeyShape(ProviderOpenAICompatible, "token-abc123"); err != nil {
		t.Fatalf("vLLM's example key refused: %v", err)
	}
	if err := ValidateAPIKeyShape(ProviderOpenAICompatible, "sk-with space-1234567890"); err == nil {
		t.Fatal("the bearer rule must still apply to a gateway key")
	}
}

// A rotation prepared from a read is conditional on the row still holding
// the value it read: a rotation that landed in between is never undone by
// the stale one landing last.
func TestMemoryApiKey_PatchWithExpectSealedIsACompareAndSwap(t *testing.T) {
	st := NewMemoryApiKeyStore()
	ctx := store.WithTenant(t.Context(), "t")
	k := ApiKey{ID: NewApiKeyID(), TenantID: "t", ScopeTeamID: "t", Provider: ProviderAnthropic, Name: "k", SealedSecret: []byte("v1"), Fingerprint: "fp1"}
	if err := st.Create(ctx, k); err != nil {
		t.Fatal(err)
	}
	// A concurrent rotation lands first.
	if _, err := st.Patch(ctx, k.ID, ApiKeyPatch{Secret: &ApiKeySecretPatch{Sealed: []byte("v2"), Last4: "2222", Fingerprint: "fp2"}}); err != nil {
		t.Fatal(err)
	}
	stale := ApiKeyPatch{Secret: &ApiKeySecretPatch{Sealed: []byte("v1-resealed"), Last4: "1111", Fingerprint: "fp1b"}, ExpectSealed: []byte("v1")}
	if _, err := st.Patch(ctx, k.ID, stale); !errors.Is(err, ErrApiKeyChanged) {
		t.Fatalf("stale re-seal = %v, want ErrApiKeyChanged", err)
	}
	if back, _ := st.Get(ctx, k.ID); string(back.SealedSecret) != "v2" || back.Fingerprint != "fp2" {
		t.Fatalf("the stale re-seal landed: %+v", back)
	}
	fresh := ApiKeyPatch{Secret: &ApiKeySecretPatch{Sealed: []byte("v3"), Last4: "3333", Fingerprint: "fp3"}, ExpectSealed: []byte("v2")}
	if got, err := st.Patch(ctx, k.ID, fresh); err != nil || string(got.SealedSecret) != "v3" {
		t.Fatalf("a current re-seal must land: %+v %v", got, err)
	}
	// An empty patch still answers the guard, as the Mongo twin does.
	if _, err := st.Patch(ctx, k.ID, ApiKeyPatch{ExpectSealed: []byte("v2")}); !errors.Is(err, ErrApiKeyChanged) {
		t.Fatalf("an empty patch with a stale guard = %v, want ErrApiKeyChanged", err)
	}
	if got, err := st.Patch(ctx, k.ID, ApiKeyPatch{ExpectSealed: []byte("v3")}); err != nil || string(got.SealedSecret) != "v3" {
		t.Fatalf("an empty patch with the current guard must answer the row: %+v %v", got, err)
	}
}

func TestServesRepoForLaunch(t *testing.T) {
	open := ApiKey{}
	if !open.ServesRepoForLaunch("") || !open.ServesRepoForLaunch("Org/repo") {
		t.Fatal("an unrestricted key serves every launch")
	}
	k := ApiKey{Repos: []string{"DNUM-SocialGouv/aides-jeunes"}}
	if !k.ServesRepoForLaunch("dnum-socialgouv/aides-jeunes") || k.ServesRepoForLaunch("DNUM-SocialGouv/other") || k.ServesRepoForLaunch("") {
		t.Fatal("a restricted key serves its repos (case-insensitively), never another repo nor a repo-less launch")
	}
}

// A key restricted to repositories is resolved only for a launch that
// serves one of them — never for another repository, never for a launch
// that names none — and the withholding is reported, not silent.
func TestResolve_RepositoryAudienceGatesBeforeThePin(t *testing.T) {
	st := NewMemoryApiKeyStore()
	sealer := newSealer(t)
	k := mkKey(t, st, sealer, "team-a", "", ProviderAnthropic, "scoped", "sk-ant-scoped-0000", true)
	if _, err := st.Patch(store.WithTenant(t.Context(), "team-a"), k.ID, ApiKeyPatch{Repos: &[]string{"dnum-socialgouv/aides-jeunes"}}); err != nil {
		t.Fatal(err)
	}
	resolve := func(ctx context.Context, pin map[Provider]string) (map[Provider]Resolution, []AudienceRefusal) {
		t.Helper()
		var withheld []AudienceRefusal
		out, err := Resolve(ctx, st, "team-a", "", "review-pr", []Provider{ProviderAnthropic}, pin, sealer, nil, func(_ ApiKey, why AudienceRefusal) { withheld = append(withheld, why) })
		if err != nil {
			t.Fatal(err)
		}
		return out, withheld
	}
	base := store.WithTenant(t.Context(), "team-a")
	if out, w := resolve(WithLaunchRepo(base, "DNUM-SocialGouv/aides-jeunes"), nil); len(out) != 1 || len(w) != 0 {
		t.Fatalf("own repo: resolved=%d withheld=%d, want 1/0", len(out), len(w))
	}
	// The cause travels with the refusal: the repository, not the bot.
	if out, w := resolve(WithLaunchRepo(base, "dnum-socialgouv/other"), nil); len(out) != 0 || len(w) != 1 || !w[0].Repo || w[0].Bot {
		t.Fatalf("another repo: resolved=%d withheld=%+v, want 0 resolved and one refusal blaming the repository alone", len(out), w)
	}
	if out, w := resolve(base, nil); len(out) != 0 || len(w) != 1 || !w[0].Repo {
		t.Fatalf("no repo on the launch: resolved=%d withheld=%+v, want 0/1 blaming the repository", len(out), w)
	}
	// A repository stamped on a parent context does not leak into a launch
	// that names none: the empty stamp overrides it.
	if out, w := resolve(WithLaunchRepo(WithLaunchRepo(base, "dnum-socialgouv/aides-jeunes"), ""), nil); len(out) != 0 || len(w) != 1 {
		t.Fatalf("a repo-less launch inherited its parent's repository: resolved=%d withheld=%d, want 0/1", len(out), len(w))
	}
	// A pin does not lift the audience.
	if out, w := resolve(WithLaunchRepo(base, "dnum-socialgouv/other"), map[Provider]string{ProviderAnthropic: k.ID}); len(out) != 0 || len(w) == 0 {
		t.Fatalf("a pin must not lift the repository audience: resolved=%d withheld=%d", len(out), len(w))
	}
	// Both audiences refusing is reported as both, never as the bot alone.
	if _, err := st.Patch(store.WithTenant(t.Context(), "team-a"), k.ID, ApiKeyPatch{Bots: &[]string{"other-bot"}}); err != nil {
		t.Fatal(err)
	}
	if _, w := resolve(WithLaunchRepo(base, "dnum-socialgouv/other"), nil); len(w) != 1 || !w[0].Bot || !w[0].Repo {
		t.Fatalf("both audiences refused, reported as %+v", w)
	}
}

// The two properties the validator promises, tested as PROPERTIES rather
// than as a table of spellings: on every accepted input it is idempotent
// (the stored URL re-sent is a no-op, never a "change" that asks for the
// secret), and no refusal ever repeats a marker planted anywhere in the
// input. The generator mixes the spellings three review rounds found
// (full-width digits, invisible characters, doubled dots, zones, pastes).
func TestValidateGatewayBaseURL_Guarantees(t *testing.T) {
	rng := rand.New(rand.NewSource(20261022))
	pick := func(xs []string) string { return xs[rng.Intn(len(xs))] }
	schemes := []string{"https://", "HTTPS://", "http://", "", "sk-PASTED-9://"}
	hosts := []string{"gw.example.test", "GW.Example.TEST", "api.example.org", "gw-2.example.net", "litellm.pliage.example.fr", "adb-1234567890123456.7.azuredatabricks.net", "gw.example.test.", "gw.example.test..", "localhost", "localhost..", "localhost\u3002", "kubernetes.default.svc", "metadata.google.internal\u3002", "127.0.0.1", "\uFF11\uFF12\uFF17.\uFF10.\uFF10.\uFF11", "\uFF11\uFF16\uFF19.\uFF12\uFF15\uFF14.\uFF11\uFF16\uFF19.\uFF12\uFF15\uFF14", "[::1]", "[2001:db8::10]", "[2001:DB8:0:0::10]", "[::ffff:203.0.113.10]", "[fe80::1%25eth0]", "203.0.113.10", "b\u00FCcher.example", "xn--bcher-kva.example", "my_gateway.example.com", "ab--cd.example.com", "-bad.example.com", "a..b.example", "\u200B", "gw\u2060.example.test", "\u00ADgw.example.test", "gw.example.test\uFEFF", "PASTED-SECRET-host", "sk-PASTED-SECRET-42", "0x7f.0.0.1", "2130706433", "127.1", "ip6-localhost", "localhost.localdomain", strings.Repeat("\u3316", 600) + ".example", "gw\u212A.example.test", "\u1E9E.example", "_gateway", "instance-data", "localhost4.localdomain4", "\u0130.example"}
	ports := []string{"", "", ":443", ":8443", ":08080", ":0", ":99999", ":PASTED-SECRET"}
	paths := []string{"", "/", "/v1", "/v1/", "/v1/v1", "/litellm/v1", "/litellm", "/openai/deployments/gpt-4o", "/serving-endpoints/v1", "/a//b", "/./x", "/a/../b", "/V1", "/a%3Fb", "/ PASTED-SECRET", "/\nPASTED-SECRET", "/\u2060PASTED-SECRET", "/\u3164PASTED-SECRET", "/\u2800PASTED-SECRET", "/a\\PASTED-SECRET", "/a;PASTED-SECRET", "/?x=PASTED-SECRET", "/#PASTED-SECRET"}
	users := []string{"", "", "", "user:PASTED-SECRET@"}
	accepted, refused := 0, 0
	for i := 0; i < 8000; i++ {
		in := pick(schemes) + pick(users) + pick(hosts) + pick(ports) + pick(paths)
		got, err := ValidateGatewayBaseURL(in)
		if err != nil {
			refused++
			if strings.Contains(err.Error(), "PASTED") {
				t.Fatalf("%q: the refusal echoes the value: %v", in, err)
			}
			continue
		}
		accepted++
		if strings.Contains(got, "PASTED") {
			// The generator plants the marker behind whitespace, a format
			// character, a blank-looking letter, a port, userinfo, a query
			// or a fragment (a host label is lower-cased away): one
			// surviving into an ACCEPTED value rode a shape the validator
			// must refuse.
			t.Fatalf("%q: a pasted marker survived into the stored URL %q", in, got)
		}
		again, err := ValidateGatewayBaseURL(got)
		if err != nil || again != got {
			t.Fatalf("%q \u2192 %q \u2192 %q (%v): the canonical form is not idempotent", in, got, again, err)
		}
		for _, bad := range []string{"localhost", "127.0.0.1", "169.254.169.254", "[::1]", ".svc", ".internal", "0x7f", "2130706433"} {
			if strings.Contains(got, bad) {
				t.Fatalf("%q: a refused host slipped through as %q", in, got)
			}
		}
		if len(got) > maxGatewayBaseURLBytes {
			t.Fatalf("%q: the stored value is %d bytes, over the cap", in, len(got))
		}
		for _, r := range got {
			if r >= utf8.RuneSelf {
				t.Fatalf("%q: a non-ASCII rune survived into the stored URL %q", in, got)
			}
		}
		if hostOf := strings.SplitN(strings.TrimPrefix(got, "https://"), "/", 2)[0]; !strings.Contains(hostOf, ".") && !strings.HasPrefix(hostOf, "[") {
			t.Fatalf("%q: a single-label host survived as %q", in, got)
		}
	}
	if accepted < 100 || refused < 100 {
		t.Fatalf("the generator must exercise both outcomes: accepted=%d refused=%d", accepted, refused)
	}
	// Rows the rounds found, each a bypass of a check that ran before the
	// host was converted.
	for _, bad := range []string{
		// A host written as a number in another base is an address to a C
		// resolver or a browser and a NAME to net.ParseIP: refused as such.
		"https://0x7f.0.0.1/v1", "https://2130706433/v1", "https://017700000001/v1", "https://0177.0.0.1/v1", "https://127.1/v1", "https://127.0.0.01/v1", "https://0/v1", "https://0x0/v1", "https://169.254.43518/v1", "https://0xa9fea9fe/v1", "https://10.1/v1", "https://\uFF10\uFF58\uFF17\uFF46.\uFF10.\uFF10.\uFF11/v1",
		// The names every container or workstation resolves to itself, and
		// a single-label host (resolvable only through /etc/hosts, a search
		// domain or a synthetic record).
		"https://ip6-localhost/v1", "https://ip6-loopback/v1", "https://localhost.localdomain/v1", "https://localhost4/v1", "https://localhost6/v1", "https://localhost4.localdomain4/v1", "https://foo.localhost.localdomain/v1", "https://_gateway/v1", "https://_localdnsstub/v1", "https://instance-data/v1", "https://pliage/v1", "https://0x/v1", "https://0x:8443/v1", "https://gw.0x/v1", "https://gw.example.0x7f/v1",
		// A label or a name over the DNS wire limit resolves nowhere.
		"https://" + strings.Repeat("a", 64) + ".example/v1", "https://" + strings.Repeat("abcdefghij.", 25) + "example/v1",
		// A character outside a label's alphabet, a private-use or an
		// unassigned rune, a blank-looking printable rune in the path.
		"https://gw!.example.test/v1", "https://a~b.example/v1", "https://gw\".example.test/v1", "https://gw.example.test/\uE000x", "https://gw.example.test/\U000F0000x", "https://gw.example.test/v1\u3164sk-live-PASTED-SECRET-42", "https://gw.example.test/v1\u2800sk-live-PASTED-SECRET-42", "https://gw.example.test/caf\u00E9",
		// A backslash is a path traversal to a browser's parser, a
		// sub-delimiter is read as something else: neither belongs to a root.
		"https://gw.example.test/a\\..\\..\\admin", "https://gw.example.test/a;b", "https://gw.example.test/a=b", "https://gw.example.test/a+b",
		// A value under the cap whose host grows past it once mapped to
		// punycode: the cap reads the stored result, not the input.
		"https://" + strings.Repeat("\u3316.", 8) + "example/" + strings.Repeat("a", 2048-8-len(strings.Repeat("\u3316.", 8)+"example/")-2),
		"https://" + strings.Repeat("\u3316", 600) + ".example",
		"https://\uFF11\uFF12\uFF17.\uFF10.\uFF10.\uFF11/v1", "https://\uFF11\uFF16\uFF19.\uFF12\uFF15\uFF14.\uFF11\uFF16\uFF19.\uFF12\uFF15\uFF14/v1", "https://localhost../v1", "https://localhost\u3002/v1", "https://kubernetes.default.svc../v1", "https://\u200B/v1", "https://\u200B:8443/litellm/v1", "https://../v1", "https://gw.example.test/v1\u200Bsk-live-PASTED-SECRET-42", "https://-bad.example.com", "https://a..b.example", "https://" + strings.Repeat("a", maxGatewayBaseURLBytes) + ".example"} {
		if got, err := ValidateGatewayBaseURL(bad); err == nil {
			t.Errorf("%q accepted as %q", bad, got)
		} else if strings.Contains(err.Error(), "PASTED") {
			t.Errorf("the refusal echoes the value: %v", err)
		}
	}
	// Hosts Go dials are accepted: an underscore label, a double hyphen
	// inside a label, a 63-character label, a digit-first label.
	for in, want := range map[string]string{
		"https://my_gateway.example.com/v1":                                    "https://my_gateway.example.com",
		"https://ab--cd.example.com/v1":                                        "https://ab--cd.example.com",
		"https://" + strings.Repeat("a", 63) + ".example/v1":                   "https://" + strings.Repeat("a", 63) + ".example",
		"https://1gw.example.com":                                              "https://1gw.example.com",
		"https://\u1E9E.example":                                               "https://ss.example",
		"https://gw\u212A.example.test":                                        "https://gwk.example.test",
		"https://\u0130.example":                                               "https://xn--i-9bb.example",
		"https://adb-1234567890123456.7.azuredatabricks.net/serving-endpoints": "https://adb-1234567890123456.7.azuredatabricks.net/serving-endpoints",
		"https://123.example":                                                  "https://123.example",
		"https://" + strings.Repeat("abcdefghi.", 25) + "abc":                  "https://" + strings.Repeat("abcdefghi.", 25) + "abc",
		"https://gw.example.test../v1":                                         "",
	} {
		got, err := ValidateGatewayBaseURL(in)
		if want == "" {
			if err == nil {
				t.Errorf("%q accepted as %q", in, got)
			}
			continue
		}
		if err != nil || got != want {
			t.Errorf("%q \u2192 %q %v, want %q", in, got, err, want)
		}
	}
}
