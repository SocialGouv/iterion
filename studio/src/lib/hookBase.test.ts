import { describe, expect, it } from "vitest";

import { hookBaseProblem, hookDeliveryAddress, isAbsoluteHookBase } from "./hookBase";

// A hook base is an address a forge dials from its own network: anything that
// is not an absolute http(s) URL produces a hook URL the forge refuses (GitLab
// answers 422 "Invalid url given"), so a provisioning surface must say so
// instead of announcing it as the delivery address.
//
// PARITY_TABLE is the one table both readings of a base are held to: the same
// inputs, carrying the SERVER's verdict, in pkg/server/hook_base_parity_test.go,
// where `absoluteHTTPURL` answers them. The server hands the base to the forge
// as written, so a base this module accepts and the server refuses is an
// address announced in monospace and then refused by the forge.
const PARITY_TABLE: Array<[string, boolean]> = [
  // Dialable: a scheme, two slashes, a host — with or without a port, a
  // prefix path, credentials, a fragment, a query, an IPv6 literal, and in
  // any case.
  ["https://iterion.example.org", true],
  ["http://iterion.example.org:8080", true],
  ["https://iterion.example.org/", true],
  ["https://iterion.example.org/behind/a/prefix", true],
  ["HTTPS://ITERION.EXAMPLE.ORG", true],
  ["Https://iterion.example.org", true],
  ["https://user:pass@iterion.example.org", true],
  ["https://[::1]:8080", true],
  ["https://[2001:db8::1]", true],
  ["https://iterion.example.org:", true],
  ["https://itérion.example.org", true],
  ["https://xn--itrion-fsa.example.org", true],
  ["HTTP://iterion.example.org", true],
  ["https://iterion.example.org#frag", true],
  ["https://iterion.example.org?q=1", true],
  ["http://@host", true],
  ["https://host:0", true],
  // Nothing to dial: no address at all, a path, a host with no scheme, a
  // protocol-relative address, a scheme no forge posts over, a scheme with no
  // host, an unparsable port, an encoded space for a host.
  ["", false],
  ["   ", false],
  ["/iterion", false],
  ["iterion.example.org", false],
  ["//iterion.example.org", false],
  ["ftp://iterion.example.org", false],
  ["javascript:alert(1)", false],
  ["https://", false],
  ["http://", false],
  ["mailto:a@b.c", false],
  ["file:///etc/passwd", false],
  ["https://iterion.example.org:abc", false],
  ["https://%20host", false],
  // What the WHATWG parser would repair, and the forge would not: one slash
  // read as two, a scheme with none, backslashes, and whitespace trimmed or
  // stripped from anywhere in the string.
  ["http:/x", false],
  ["http:/iterion.example.org", false],
  ["https:/iterion.example.org", false],
  ["https:iterion.example.org", false],
  ["https://iterion.example.org ", false],
  [" https://iterion.example.org", false],
  ["\thttps://iterion.example.org", false],
  ["https://iterion.example.org\n", false],
  ["https://iterion.ex\nample.org", false],
  ["https:\\\\iterion.example.org", false],
  // A control character the WHATWG parser strips from the ends, and the
  // server refuses anywhere.
  ["https://iterion.example.org\u0001", false],
  ["\u001fhttps://iterion.example.org", false],
  // What url.Parse refuses and the WHATWG parser repairs or accepts: a third
  // slash (no authority), a backslash in the path, an escaped ASCII byte in
  // the host, a malformed escape.
  ["https:///iterion.example.org", false],
  ["https://iterion.example.org\\path", false],
  ["https://%41iterion.example.org", false],
  ["https://iterion.example.org/%zz", false],
];

/** The one input of the table whose verdict belongs to the runtime rather than
 *  to this module: `URL` validates the punycode of an `xn--` label against an
 *  IDNA table that moves (node 22 refuses this host, node 24 accepts it),
 *  where the server's `url.Parse` never looks. Refusing it costs a warning
 *  naming the setting; the reverse — announcing an address no forge can reach
 *  — is what the rest of the table prevents. */
const RUNTIME_IDNA = "https://xn--itrion-fsa.example.org";

describe("isAbsoluteHookBase", () => {
  it("never accepts a base the server refuses", () => {
    const refused = PARITY_TABLE.filter(([, server]) => !server);
    expect(refused).toHaveLength(29);
    for (const [base] of refused) {
      expect(isAbsoluteHookBase(base), JSON.stringify(base)).toBe(false);
    }
  });

  it("accepts every base the server dials", () => {
    const dialable = PARITY_TABLE.filter(
      ([base, server]) => server && base !== RUNTIME_IDNA,
    );
    expect(dialable).toHaveLength(16);
    for (const [base] of dialable) {
      expect(isAbsoluteHookBase(base), JSON.stringify(base)).toBe(true);
    }
  });

  it("holds the same inputs as the server's half of the table", () => {
    // A row dropped on one side is a shape nobody reads any more: the count is
    // pinned on both (pkg/server/hook_base_parity_test.go).
    expect(PARITY_TABLE).toHaveLength(46);
    expect(new Set(PARITY_TABLE.map(([base]) => base)).size).toBe(46);
  });
});

describe("hookBaseProblem", () => {
  it("says nothing about a base a hook URL can be built from", () => {
    expect(hookBaseProblem("https://iterion.example.org")).toBeNull();
  });

  it("names the missing public URL for an empty base", () => {
    expect(hookBaseProblem("")).toBe("no address — this deployment has no public URL");
  });

  it("quotes a base that is not an absolute http(s) URL, and names the setting", () => {
    expect(hookBaseProblem("iterion.example.org")).toBe(
      'no address — "iterion.example.org" is not an absolute http(s) URL (auth.publicUrl / ITERION_PUBLIC_URL)',
    );
    // A relative address is what a deployment with no public URL at all
    // produces once something prefixes the path.
    expect(hookBaseProblem("/hooks")).toContain("is not an absolute http(s) URL");
  });

  it("quotes a base the parser would have repaired", () => {
    // A public URL read from a file keeps its trailing newline, and one slash
    // is a plausible typo: both reach the forge as written, so both are a
    // problem the operator is told about — quoted, so the stray character is
    // visible in the sentence.
    expect(hookBaseProblem("https:/iterion.example.org")).toBe(
      'no address — "https:/iterion.example.org" is not an absolute http(s) URL (auth.publicUrl / ITERION_PUBLIC_URL)',
    );
    expect(hookBaseProblem("https://iterion.example.org\n")).toContain(
      "is not an absolute http(s) URL",
    );
  });
});

describe("hookDeliveryAddress", () => {
  it("builds the address the server registers", () => {
    expect(hookDeliveryAddress("https://iterion.example.org", "gitlab")).toBe(
      "https://iterion.example.org/api/webhooks/gitlab/…",
    );
  });
});
