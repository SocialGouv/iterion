import { afterEach, describe, expect, it, vi } from "vitest";
import { BOTSOURCE_SCHEME, loadExample, openFile, parseSource, parseUnit } from "./client";

// A replacement gives up on a load that does not answer, and aborts it
// through the signal it hands the load: every request the load makes has to
// carry that signal to fetch, or the request lives on after the editor has
// given up on it.
type Call = { url: string; init?: RequestInit };
function mockFetch(routes: (call: Call) => unknown): Call[] {
  const calls: Call[] = [];
  globalThis.fetch = vi.fn(async (url: string, init?: RequestInit) => {
    const call = { url, init };
    calls.push(call);
    return { ok: true, status: 200, json: async () => routes(call), text: async () => "" };
  }) as unknown as typeof fetch;
  return calls;
}
const parsed = { document: { agents: [] }, diagnostics: [] };

afterEach(() => {
  vi.restoreAllMocks();
});

describe("the loads a replacement makes carry its signal to every request", () => {
  it("opening a file on disk", async () => {
    const calls = mockFetch(() => ({ source: "", ...parsed, path: "bots/a.bot" }));
    const signal = new AbortController().signal;
    await openFile("bots/a.bot", { signal });
    expect(calls).toHaveLength(1);
    expect(calls[0]?.init?.signal).toBe(signal);
  });

  it("opening a cloud bot in one file: the bundle, then its parse", async () => {
    const calls = mockFetch(({ url }) =>
      url.includes("/bot-sources/") ? { files: { "main.bot": "workflow w:\n  entry: done\n" } } : parsed,
    );
    const signal = new AbortController().signal;
    await openFile(`${BOTSOURCE_SCHEME}t1/demo/main.bot`, { signal });
    expect(calls.map((c) => c.init?.signal === signal)).toEqual([true, true]);
  });

  it("opening a cloud bot in several files: the bundle, then the unit's parse", async () => {
    const calls = mockFetch(({ url }) =>
      url.includes("/bot-sources/")
        ? { files: { "main.bot": 'import "lib/nodes.bot"\n\nworkflow w:\n  entry: done\n', "lib/nodes.bot": "" } }
        : { ...parsed, unit: { root: "", main: "main.bot", revision: "r1", files: [] } },
    );
    const signal = new AbortController().signal;
    await openFile(`${BOTSOURCE_SCHEME}t1/demo/main.bot`, { signal });
    expect(calls.map((c) => c.init?.signal === signal)).toEqual([true, true]);
  });

  it("an example, a parse, a unit's parse", async () => {
    const calls = mockFetch(() => ({ source: "", ...parsed }));
    const signal = new AbortController().signal;
    await loadExample("x/main.bot", { signal });
    await parseSource("workflow w:\n  entry: done\n", { signal });
    await parseUnit({ "main.bot": "" }, "main.bot", { signal });
    expect(calls.map((c) => c.init?.signal === signal)).toEqual([true, true, true]);
  });
});
