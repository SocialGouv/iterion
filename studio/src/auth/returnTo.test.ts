import { describe, expect, it } from "vitest";
import { signInReturnTo, signInURL } from "./returnTo";

const at = (path: string) => new URL(path, "https://iterion.cloud");

describe("sign-in destination", () => {
  it("preserves a run's full address through the login URL", () => {
    const target = "/runs/review-123?tab=events&filter=llm%20calls#node-converge";
    expect(signInReturnTo(at(signInURL(target)))).toBe(target);
  });

  it("keeps a direct local sign-in destination and invitation return", () => {
    expect(signInReturnTo(at("/runs/review-123?tab=events#last"))).toBe("/runs/review-123?tab=events#last");
    const target = "/invitations/accept?token=invite";
    expect(signInReturnTo(at(signInURL(target)))).toBe(target);
  });

  it.each(["https://evil.example", "//evil.example", "/\\evil.example", "/%2fevil.example", "/%5cevil.example", "/\t/evil.example", "/\n/evil.example", "javascript:alert(1)", "/login?next=/runs/123"])(
    "rejects an unsafe or looping return target: %j",
    (next) => expect(signInReturnTo(at(`/login?${new URLSearchParams({ next })}`))).toBe("/"),
  );

  it("does not reuse authentication parameters as a destination", () => {
    expect(signInReturnTo(at("/auth/password/change?temp=temporary"))).toBe("/");
    expect(signInReturnTo(at("/login"))).toBe("/");
  });
});
