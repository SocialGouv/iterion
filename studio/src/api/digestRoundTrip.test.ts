import { afterEach, describe, expect, it, vi } from "vitest";

import { BOTSOURCE_SCHEME, openFile, saveFile } from "./client";
import type { IterDocument } from "./types";

// The studio edit round trip, end to end at the HTTP boundary: the REAL
// client (openFile → edit → saveFile) against a stand-in of the server's
// bot-source contract — the version CAS, the digest rule (a write presents
// the id, the version and the per-file digests its read issued; a mismatch
// is a 409) and the unit render. What is asserted is what crossed the
// boundary: the requests the client makes and the bytes they land.

type Bundle = {
  id: string;
  slug: string;
  version: number;
  files: Record<string, string>;
  file_sha256: Record<string, string>;
};

const sha = (s: string): string => {
  let h = 0x811c9dc5;
  for (let i = 0; i < s.length; i++) {
    h ^= s.charCodeAt(i);
    h = Math.imul(h, 0x01000193) >>> 0;
  }
  return "f" + h.toString(16).padStart(8, "0") + s.length.toString(16);
};

const MAIN = 'import "lib/nodes.bot"\n\nworkflow w:\n  entry: worker\n  worker -> done\n';
const NODES = 'agent worker:\n  model: "m"\n';

function contractServer(bundle: Bundle, opts?: { onPut?: (body: Record<string, unknown>) => void }) {
  const calls: Array<{ method: string; url: string; body: unknown }> = [];
  const fetchStub = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    const method = init?.method ?? "GET";
    const body = init?.body ? (JSON.parse(String(init.body)) as Record<string, unknown>) : undefined;
    calls.push({ method, url, body });
    const json = (v: unknown) => new Response(JSON.stringify(v), { status: 200 });
    const fail = (status: number, error: string) =>
      new Response(JSON.stringify({ error }), { status });
    if (method === "GET" && url.includes("/bot-sources/demo")) return json(bundle);
    if (method === "PUT" && url.includes("/bot-sources/demo/files/")) {
      opts?.onPut?.(body!);
      // The server's per-file checks, at the boundary: identity, version,
      // and the digest of the stored text the write replaces.
      if (bundle.version !== (body!.version as number)) return fail(409, "version conflict");
      if ((body as { id?: string }).id !== bundle.id) return fail(409, "not the bot this write read");
      const rel = url.split("/files/")[1] ?? "";
      const stored = bundle.files[decodeURIComponent(rel)];
      if (stored !== undefined && (body as { stored_sha256?: string }).stored_sha256 !== sha(stored)) {
        return fail(409, "carries no digest of the read it replaces");
      }
      bundle.files[decodeURIComponent(rel)] = body!.content as string;
      bundle.file_sha256[decodeURIComponent(rel)] = sha(body!.content as string);
      bundle.version += 1;
      return json({ ...bundle });
    }
    if (method === "PUT" && url.includes("/bot-sources/demo")) {
      opts?.onPut?.(body!);
      if (bundle.version !== (body!.version as number)) return fail(409, "version conflict");
      if ((body as { id?: string }).id !== bundle.id) return fail(409, "not the bot this write read");
      for (const [path, digest] of Object.entries(
        (body!.stored_sha256 ?? {}) as Record<string, string>,
      )) {
        if (bundle.file_sha256[path] !== digest) return fail(409, `${path} carries no digest of the read it replaces`);
      }
      for (const stored of Object.keys(bundle.files)) {
        if (!(stored in (body!.files as Record<string, string>))) {
          // An omission deletes: it must carry the omitted file's digest.
          if (!(stored in ((body!.stored_sha256 ?? {}) as Record<string, string>))) {
            return fail(409, `${stored} carries no digest of the read it replaces`);
          }
        }
      }
      bundle.files = body!.files as Record<string, string>;
      for (const [path, text] of Object.entries(bundle.files)) bundle.file_sha256[path] = sha(text);
      bundle.version += 1;
      return json({ ...bundle });
    }
    if (url.endsWith("/parse") && method === "POST") {
      // The cloud editor's unit open: the revision is the read's.
      return json({
        document: { agents: [] },
        diagnostics: [],
        bindable: true,
        unit: {
          root: "",
          main: (body as { main: string }).main,
          revision: `rev${bundle.version}`,
          files: Object.keys(bundle.files).map((rel) => ({
            rel,
            digest: bundle.file_sha256[rel],
          })),
        },
      });
    }
    if (url.endsWith("/unparse")) {
      // The unit write-back: the render answers the rewritten files and the
      // revision they will have, and refuses a render whose revision it
      // does not know.
      const b = body as { files: Record<string, string>; main: string; revision: string };
      if (b.revision !== `rev${bundle.version}`) {
        return new Response(JSON.stringify({ error: "the files changed since the document was opened" }), { status: 409 });
      }
      return json({ source: b.files["main.bot"], files: { "lib/nodes.bot": NODES + "\nprompt kept:\n  by the render\n" }, revision: `rev${bundle.version + 1}` });
    }
    return json({});
  });
  vi.stubGlobal("fetch", fetchStub);
  return calls;
}

describe("the studio edit round trip under the digest rule", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("opens, edits and lands a unit save carrying the opened id, version and digests", async () => {
    const bundle: Bundle = {
      id: "b-1",
      slug: "demo",
      version: 3,
      files: { "main.bot": MAIN, "lib/nodes.bot": NODES },
      file_sha256: { "main.bot": sha(MAIN), "lib/nodes.bot": sha(NODES) },
    };
    const bodies: Array<Record<string, unknown>> = [];
    const calls = contractServer(bundle, { onPut: (b) => bodies.push(b) });
    const editorPath = `${BOTSOURCE_SCHEME}t1/demo/main.bot`;

    const opened = await openFile(editorPath);
    expect(opened.unit?.revision).toBe(`rev${bundle.version}`);
    const document = { agents: [] } as unknown as IterDocument;
    const result = await saveFile(editorPath, document, { revision: opened.unit!.revision });

    console.log("CALLS", JSON.stringify(calls.map((c) => [c.method, c.url.replace(/[a-z0-9-]+\/bot-sources/, "B"), Object.keys((c.body ?? {}) as object)])));
    const put = calls.find((c) => c.method === "PUT" && !c.url.includes("/files/"));
    expect(put).toBeTruthy();
    const putBody = put!.body as { id: string; version: number; stored_sha256: Record<string, string> };
    // What an open decided, the write carries.
    expect(putBody.id).toBe("b-1");
    expect(putBody.version).toBe(3);
    expect(putBody.stored_sha256["lib/nodes.bot"]).toBe(sha(NODES));
    // The bot kept every file the save did not rewrite, and its answer is
    // the state to save against next.
    expect(bundle.files["manifest.yaml" in bundle.files ? "manifest.yaml" : "lib/nodes.bot"]).toContain("prompt kept");
    expect(result.revision).toBe(`rev4`);
  });

  it("refuses the save of a tab whose open issued no digest, and never refetches at save time", async () => {
    const bundle: Bundle = {
      id: "b-2",
      slug: "demo",
      version: 3,
      files: { "main.bot": MAIN },
      file_sha256: { "main.bot": sha(MAIN) },
    };
    const calls = contractServer(bundle);
    const editorPath = `${BOTSOURCE_SCHEME}t1/demo/main.bot`;
    await openFile(editorPath);
    // Another editor writes meanwhile: this tab's snapshot is stale, and
    // the save presents the STALE read — never a fresh fetch at save time.
    bundle.version = 9;
    const document = { agents: [] } as unknown as IterDocument;
    const afterOpen = calls.length;
    // The render refuses the stale revision first — the boundary the rule
    // answers at (the server never sees a PUT decided on another read).
    await expect(saveFile(editorPath, document, { revision: `rev3` })).rejects.toThrow(/changed since|conflict/i);
    // The render is refused before anything is PUT, and no GET ran: the
    // read the save presented is the open's, never a fresh fetch.
    expect(calls.length).toBe(afterOpen + 1);
    expect(calls.slice(afterOpen).some((c) => c.method === "GET")).toBe(false);
    expect(calls.some((c) => c.method === "PUT")).toBe(false);
  });

  it("a concurrent editor's write is a 409 naming what changed, never an overwrite", async () => {
    const bundle: Bundle = {
      id: "b-3",
      slug: "demo",
      version: 3,
      files: { "main.bot": MAIN },
      file_sha256: { "main.bot": sha(MAIN) },
    };
    contractServer(bundle);
    const editorPath = `${BOTSOURCE_SCHEME}t1/demo/main.bot`;
    const opened = await openFile(editorPath);
    const theirs = MAIN + "\nprompt theirs:\n  by the other editor\n";
    bundle.files["main.bot"] = theirs;
    bundle.file_sha256["main.bot"] = sha(theirs);
    bundle.version += 1;
    const document = { agents: [] } as unknown as IterDocument;
    await expect(
      saveFile(editorPath, document, { revision: opened.unit?.revision }),
    ).rejects.toThrow(/changed since|carries no digest|version conflict/i);
    // The other editor's bytes stand.
    expect(bundle.files["main.bot"]).toBe(theirs);
  });
});
