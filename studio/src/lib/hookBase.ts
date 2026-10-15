// The address a forge is told to deliver hooks to: the base a provisioning
// preview reports (`hook_base_url` — the connection's pin when it has one,
// else the deployment's public URL), and the two shapes no forge can deliver
// to. Both provisioning surfaces — the Enable panel and the bind wizard's
// Review step — read this one table, so they cannot describe the same base
// differently.

/** True when a base is an absolute http(s) URL with a host: the only shape a
 *  forge accepts for a hook.
 *
 *  The server hands the base to the forge as written and reads it with
 *  `url.Parse`, which repairs nothing (pkg/server/forge_provisioning_routes.go,
 *  `absoluteHTTPURL`); the WHATWG `URL` parser does — it trims and strips
 *  whitespace, and reads a single slash after a special scheme as two. So the
 *  scheme and the slashes are matched on the raw string, and whitespace or a
 *  control character is refused outright; `URL` is then asked for the host
 *  alone. The two readings are held to one table: the inputs of
 *  `hookBase.test.ts` are the inputs of `pkg/server/hook_base_parity_test.go`. */
export function isAbsoluteHookBase(base: string): boolean {
  if (hasSpaceOrControl(base)) return false;
  // url.Parse repairs no backslash, and reads the authority as everything up
  // to the next "/", "?" or "#": a third slash leaves it empty.
  const m = /^https?:\/\/([^/?#]*)/i.exec(base);
  if (!m || base.includes("\\")) return false;
  const authority = m[1] ?? "";
  // A percent escape: url.Parse refuses a malformed one anywhere but the
  // query, and an escaped ASCII byte in the host.
  if (/%(?![0-9a-f]{2})/i.test(base.split("?")[0] ?? "")) return false;
  const host = authority.slice(authority.lastIndexOf("@") + 1);
  if (host === "") return false;
  if (/%[0-7][0-9a-f]/i.test(host) && !/%25/i.test(host)) return false;
  // The bytes the WHATWG host parser accepts and url.Parse refuses in a host.
  if (/[`{}]/.test(host)) return false;
  try {
    return new URL(base).host !== "";
  } catch {
    return false;
  }
}

/** True when a string carries whitespace or a control character (U+0000–U+001F,
 *  U+007F) — read by code point, since a control range in a regex literal is
 *  what the lint refuses. */
function hasSpaceOrControl(s: string): boolean {
  for (const ch of s) {
    const c = ch.codePointAt(0) ?? 0;
    if (c <= 0x1f || c === 0x7f) return true;
  }
  return /\s/.test(s);
}

/** What the operator must be told about an announced base, or null when a
 *  hook address can be built from it. The remedy clause belongs to the
 *  surface: where the pin is made differs between them. */
export function hookBaseProblem(base: string): string | null {
  if (base.trim() === "") return "no address — this deployment has no public URL";
  if (!isAbsoluteHookBase(base)) {
    return `no address — "${base}" is not an absolute http(s) URL (auth.publicUrl / ITERION_PUBLIC_URL)`;
  }
  return null;
}

/** The hook address a provision announces, for a base this table accepts. */
export function hookDeliveryAddress(base: string, provider: string): string {
  return `${base}/api/webhooks/${provider}/…`;
}
