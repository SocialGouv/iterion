import { describe, expect, it } from "vitest";
import { signInReturnTo, signInURL } from "./returnTo";
import { STUDIO_BASE } from "@/lib/scope";

const at = (path: string) => new URL(path, "https://iterion.cloud");

// Where a sign-in goes when the URL names nowhere. Not "/": that is the
// product home, and someone who just typed a password is asking for the
// studio, not for the marketing page.
const DEFAULT_DESTINATION = STUDIO_BASE;

describe("sign-in destination", () => {
  it("preserves a run's full address through the login URL", () => {
    const target = "/studio/runs/review-123?tab=events&filter=llm%20calls#node-converge";
    expect(signInReturnTo(at(signInURL(target)))).toBe(target);
  });

  it("keeps a direct local sign-in destination and invitation return", () => {
    expect(signInReturnTo(at("/studio/runs/review-123?tab=events#last"))).toBe("/studio/runs/review-123?tab=events#last");
    const target = "/invitations/accept?token=invite";
    expect(signInReturnTo(at(signInURL(target)))).toBe(target);
  });

  it.each(["https://evil.example", "//evil.example", "/\\evil.example", "/%2fevil.example", "/%5cevil.example", "/\t/evil.example", "/\n/evil.example", "javascript:alert(1)", "/login?next=/studio/runs/123"])(
    "rejects an unsafe or looping return target: %j",
    (next) => expect(signInReturnTo(at(`/login?${new URLSearchParams({ next })}`))).toBe(DEFAULT_DESTINATION),
  );

  it("does not reuse authentication parameters as a destination", () => {
    expect(signInReturnTo(at("/auth/password/change?temp=temporary"))).toBe(DEFAULT_DESTINATION);
    expect(signInReturnTo(at("/login"))).toBe(DEFAULT_DESTINATION);
  });

  // A rejected target must not fall back onto the product home either: that is
  // where an operator lands with no session-shaped reason to be there, and the
  // studio is one click further away than before the move.
  it("falls back into the studio, never onto the product home", () => {
    expect(signInReturnTo(at("/login?next=https://evil.example"))).not.toBe("/");
    expect(signInURL("https://evil.example")).toBe(`/login?next=${encodeURIComponent(STUDIO_BASE)}`);
  });
});
