import { describe, expect, it } from "vitest";

import { closeReasonToken, isScopeLostClose } from "./policy";

describe("the close reason token", () => {
  it("is the first word of the server's reason", () => {
    expect(closeReasonToken("scope_lost: the run this socket watches was replaced or deleted")).toBe(
      "scope_lost",
    );
    expect(closeReasonToken("lifetime_reached: reopen the socket")).toBe("lifetime_reached");
    expect(closeReasonToken("scope_lost")).toBe("scope_lost");
    expect(closeReasonToken("")).toBe("");
    expect(closeReasonToken(undefined)).toBe("");
  });

  it("names scope_lost only for that token, never for its cause", () => {
    expect(isScopeLostClose("scope_lost: the run this socket watches is gone")).toBe(true);
    expect(isScopeLostClose("scope_lost: the run this socket watches was replaced or deleted")).toBe(
      true,
    );
    expect(isScopeLostClose("scope_lost")).toBe(true);
    expect(isScopeLostClose("scope_lostx: other")).toBe(false);
    expect(isScopeLostClose("credential_refused: scope_lost")).toBe(false);
    expect(isScopeLostClose("lifetime_reached: reopen the socket")).toBe(false);
    expect(isScopeLostClose("")).toBe(false);
  });
});
