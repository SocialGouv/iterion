import { afterEach, describe, expect, it, vi } from "vitest";

import { BOTSOURCE_SCHEME, openFile, saveFile } from "./client";
import type { IterDocument } from "./types";

// The cloud save of a bot in several files: the document is written back
// by provenance through /api/unparse, which is handed the revision the
// document was opened at — the server refuses a bundle whose files moved
// since — and the rewritten files are patched into the WHOLE bundle for one
// versioned PUT; the revision the bundle has next comes back to the caller.

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

afterEach(() => {
  vi.restoreAllMocks();
});

describe("saveFile of a cloud bot in several files", () => {
  it("presents the revision to /api/unparse, patches the whole bundle, and returns the next revision", async () => {
    const bundle = {
      id: "b1",
      slug: "demo",
      version: 3,
      files: {
        "main.bot": 'import "lib/nodes.bot"\n\nworkflow w:\n  entry: worker\n  worker -> done\n',
        "lib/nodes.bot": "agent worker:\n  model: \"m\"\n",
        "manifest.yaml": "name: demo\n",
      },
    };
    const calls = mockFetch(({ url, init }) => {
      if (url.endsWith("/unparse")) {
        return { source: bundle.files["main.bot"], files: { "lib/nodes.bot": "agent worker:\n  model: \"m2\"\n" }, revision: "r2" };
      }
      if (url.includes("/bot-sources/demo") && (init?.method ?? "GET") === "GET") return bundle;
      return {};
    });
    const document = { agents: [] } as unknown as IterDocument;
    const result = await saveFile(`${BOTSOURCE_SCHEME}t1/demo/main.bot`, document, { revision: "r1" });

    const unparse = calls.find((c) => c.url.endsWith("/unparse"));
    expect(unparse).toBeTruthy();
    const body = JSON.parse(String(unparse?.init?.body));
    expect(body.revision).toBe("r1");
    expect(body.main).toBe("main.bot");
    expect(Object.keys(body.files).sort()).toEqual(["lib/nodes.bot", "main.bot", "manifest.yaml"]);

    const put = calls.find((c) => c.init?.method === "PUT");
    expect(put).toBeTruthy();
    const putBody = JSON.parse(String(put?.init?.body));
    expect(putBody.version).toBe(3);
    expect(putBody.files["manifest.yaml"]).toBe("name: demo\n");
    expect(putBody.files["lib/nodes.bot"]).toContain("m2");
    expect(putBody.files["main.bot"]).toBe(bundle.files["main.bot"]);

    expect(result.revision).toBe("r2");
    expect(result.files).toEqual(["lib/nodes.bot"]);
  });

  it("takes the unit path on the document's revision, never on the stored text", async () => {
    // The stored main lost its import since the open: the document is
    // still a unit, and says so through its revision.
    const bundle = { id: "b1", slug: "demo", version: 4, files: { "main.bot": "workflow w:\n  entry: done\n", "lib/nodes.bot": "agent a:\n  model: \"m\"\n" } };
    const calls = mockFetch(({ url, init }) => {
      if (url.endsWith("/unparse")) return { source: "workflow w:\n  entry: done\n", files: {}, revision: "r9" };
      if (url.includes("/bot-sources/demo") && (init?.method ?? "GET") === "GET") return bundle;
      return {};
    });
    const document = { agents: [] } as unknown as IterDocument;
    await saveFile(`${BOTSOURCE_SCHEME}t1/demo/main.bot`, document, { revision: "r1" });
    expect(calls.some((c) => c.url.endsWith("/unparse") && JSON.parse(String(c.init?.body)).revision === "r1")).toBe(true);
    expect(calls.some((c) => c.init?.method === "PUT" && c.url.endsWith("/files/main.bot"))).toBe(false);

    // Without a revision the save is a single file's, whatever the
    // stored main says: the server refuses a unit document there.
    const importing = { ...bundle, files: { ...bundle.files, "main.bot": 'import "lib/nodes.bot"\n\nworkflow w:\n  entry: done\n' } };
    const single = mockFetch(({ url, init }) => {
      if (url.endsWith("/unparse")) return { source: "workflow w:\n  entry: done\n" };
      if (url.includes("/bot-sources/demo") && (init?.method ?? "GET") === "GET") return importing;
      return {};
    });
    await saveFile(`${BOTSOURCE_SCHEME}t1/demo/main.bot`, document);
    expect(single.some((c) => c.init?.method === "PUT" && c.url.endsWith("/files/main.bot"))).toBe(true);
    expect(single.some((c) => c.url.endsWith("/unparse") && "files" in JSON.parse(String(c.init?.body)))).toBe(false);
  });

  it("opens a companion workflow that imports as its unit, not only main.bot", async () => {
    const bundle = {
      id: "b1",
      slug: "demo",
      version: 2,
      files: {
        "main.bot": "workflow w:\n  entry: done\n",
        "workflows/x.bot": 'import "lib/n.bot"\n\nworkflow x:\n  entry: a\n  a -> done\n',
        "workflows/lib/n.bot": "agent a:\n  model: \"m\"\n",
      },
    };
    const calls = mockFetch(({ url, init }) => {
      if (url.endsWith("/parse")) return { document: { agents: [] }, diagnostics: [], unit: { root: "", main: "workflows/x.bot", revision: "r1", files: [] } };
      if (url.includes("/bot-sources/demo") && (init?.method ?? "GET") === "GET") return bundle;
      return {};
    });
    const opened = await openFile(`${BOTSOURCE_SCHEME}t1/demo/workflows/x.bot`);
    const parse = calls.find((c) => c.url.endsWith("/parse"));
    expect(parse).toBeTruthy();
    const body = JSON.parse(String(parse?.init?.body));
    expect(body.main).toBe("workflows/x.bot");
    expect(Object.keys(body.files ?? {}).sort()).toEqual(["main.bot", "workflows/lib/n.bot", "workflows/x.bot"]);
    expect(opened.unit?.main).toBe("workflows/x.bot");
  });
});
