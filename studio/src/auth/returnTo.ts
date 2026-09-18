import { studioBase } from "@/lib/scope";

type BrowserLocation = Pick<Location, "pathname" | "search" | "hash">;

// Keep the same-origin boundary through browser URL normalization too.
function isLocalPath(value: string | null): value is string {
  return !!value && value.startsWith("/") &&
    !/^\/(\/|%2f|%5c)/i.test(value) &&
    !/[\\\p{Cc}]/u.test(value);
}

/**
 * defaultReturnTo is where a sign-in lands when the URL names no destination:
 * the studio, not "/". "/" is the product home, and dropping someone who has
 * just authenticated onto the marketing page is not signing them in.
 *
 * Exported because callers that decide whether a `next` parameter is worth
 * carrying must compare against THIS, not against a literal — the one that
 * compared against "/" kept writing a redundant ?next= the moment the default
 * moved.
 */
export function defaultReturnTo(): string {
  return studioBase();
}

export function signInURL(target: string): string {
  return `/login?${new URLSearchParams({ next: isLocalPath(target) ? target : defaultReturnTo() })}`;
}

export function signInReturnTo(location: BrowserLocation): string {
  const next = new URLSearchParams(location.search).get("next");
  if (isLocalPath(next) && new URL(next, "https://iterion.invalid").pathname !== "/login") {
    return next;
  }
  if (location.pathname === "/login" || location.pathname.startsWith("/auth/")) return defaultReturnTo();
  const here = location.pathname + location.search + location.hash;
  return isLocalPath(here) ? here : defaultReturnTo();
}
