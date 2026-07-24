import { describe, expect, it } from "vitest";

import { editorDeepLinkTargetsDocument } from "./editorDeepLink";

describe("editorDeepLinkTargetsDocument", () => {
  it("accepts the visible tab already bound to the requested file", () => {
    expect(
      editorDeepLinkTargetsDocument(
        true,
        "bots/town-dev/main.bot",
        "bots/town-dev/main.bot",
      ),
    ).toBe(true);
  });

  it("rejects an active untitled tab while the destination tab hydrates", () => {
    expect(
      editorDeepLinkTargetsDocument(
        true,
        null,
        "bots/town-dev/main.bot",
      ),
    ).toBe(false);
  });

  it("rejects hidden hydrated tabs that share the browser URL", () => {
    expect(
      editorDeepLinkTargetsDocument(
        false,
        "bots/another-bot/main.bot",
        "bots/town-dev/main.bot",
      ),
    ).toBe(false);
  });
});
