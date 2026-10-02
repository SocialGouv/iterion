import { afterEach, describe, expect, it, vi } from "vitest";

import { deleteBotSourceFile, putBotSourceFile } from "./botSources";

// Every per-file write of a bundle presents the if-match token of the bundle
// its caller read. Without one, `version: 0` reaches both store twins as "no
// if-match" (pkg/botsource/memory.go, mongo.go) and a second editor's change
// is overwritten with no error anywhere (#1650). The token is required in the
// signature so a caller that genuinely has none says `"unchecked"` rather
// than omitting an argument nobody notices is missing.

type Call = { url: string; init?: RequestInit };

function mockFetch(): Call[] {
  const calls: Call[] = [];
  globalThis.fetch = vi.fn(async (url: string, init?: RequestInit) => {
    calls.push({ url, init });
    return { ok: true, status: 200, json: async () => ({ id: "b", slug: "demo", version: 4, files: {} }), text: async () => "" };
  }) as unknown as typeof fetch;
  return calls;
}

afterEach(() => {
  vi.restoreAllMocks();
});

describe("putBotSourceFile", () => {
  it("sends the if-match version in the body", async () => {
    const calls = mockFetch();
    await putBotSourceFile("team-1", "demo", "skills/a.md", "hello", 3);
    const body = JSON.parse(String(calls[0]!.init!.body));
    expect(calls[0]!.init!.method).toBe("PUT");
    expect(body).toEqual({ content: "hello", version: 3 });
  });

  it("omits it for an unchecked write, rather than sending 0", async () => {
    const calls = mockFetch();
    await putBotSourceFile("team-1", "demo", "skills/a.md", "hello", "unchecked");
    const body = JSON.parse(String(calls[0]!.init!.body));
    // `version: 0` and an absent version mean the same to the store, but the
    // wire says what the client meant.
    expect(body).toEqual({ content: "hello" });
    expect("version" in body).toBe(false);
  });

  it("escapes each path segment without eating the separators", async () => {
    const calls = mockFetch();
    await putBotSourceFile("team 1", "my bot", "skills/a b.md", "x", 1);
    expect(calls[0]!.url).toBe(
      "/api/teams/team%201/bot-sources/my%20bot/files/skills/a%20b.md",
    );
  });
});

describe("deleteBotSourceFile", () => {
  it("sends the if-match version in the query, since a DELETE carries no body", async () => {
    const calls = mockFetch();
    await deleteBotSourceFile("team-1", "demo", "skills/a.md", 3);
    expect(calls[0]!.init!.method).toBe("DELETE");
    expect(calls[0]!.url).toBe(
      "/api/teams/team-1/bot-sources/demo/files/skills/a.md?version=3",
    );
    expect(calls[0]!.init!.body).toBeUndefined();
  });

  it("omits the query for an unchecked delete", async () => {
    const calls = mockFetch();
    await deleteBotSourceFile("team-1", "demo", "skills/a.md", "unchecked");
    expect(calls[0]!.url).toBe("/api/teams/team-1/bot-sources/demo/files/skills/a.md");
    expect(calls[0]!.url).not.toContain("version");
  });
});
