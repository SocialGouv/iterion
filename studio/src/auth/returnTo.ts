type BrowserLocation = Pick<Location, "pathname" | "search" | "hash">;

// Keep the same-origin boundary through browser URL normalization too.
function isLocalPath(value: string | null): value is string {
  return !!value && value.startsWith("/") &&
    !/^\/(\/|%2f|%5c)/i.test(value) &&
    !/[\\\p{Cc}]/u.test(value);
}

export function signInURL(target: string): string {
  return `/login?${new URLSearchParams({ next: isLocalPath(target) ? target : "/" })}`;
}

export function signInReturnTo(location: BrowserLocation): string {
  const next = new URLSearchParams(location.search).get("next");
  if (isLocalPath(next) && new URL(next, "https://iterion.invalid").pathname !== "/login") {
    return next;
  }
  if (location.pathname === "/login" || location.pathname.startsWith("/auth/")) return "/";
  const here = location.pathname + location.search + location.hash;
  return isLocalPath(here) ? here : "/";
}
