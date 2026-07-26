import { afterEach, describe, expect, it, vi } from "vitest";

import {
  getRunFileContent,
  saveRunFileContent,
  type RunFileContent,
} from "./runs";

// These lock the wire contract of the in-run file editor's two endpoints
// (GET/PUT /api/runs/:id/files/content) without mounting Monaco: the URL
// shape, the ?path= encoding, and the PUT method+body the Go handlers
// (handleGetRunFileContent / handleSaveRunFileContent) expect.

function mockFetchOnce(body: unknown): typeof fetch {
  const fn = vi.fn(async () => ({
    ok: true,
    status: 200,
    json: async () => body,
  })) as unknown as typeof fetch;
  globalThis.fetch = fn;
  return fn;
}

// firstFetchCall returns the [url, init] args of the first recorded fetch
// call, typed, without repeating the double-cast at every assertion site.
function firstFetchCall(fn: typeof fetch): [string, RequestInit?] {
  return (fn as unknown as ReturnType<typeof vi.fn>).mock.calls[0] as [
    string,
    RequestInit?,
  ];
}

afterEach(() => {
  vi.restoreAllMocks();
});

describe("getRunFileContent", () => {
  it("GETs /files/content with the path query-encoded", async () => {
    const payload: RunFileContent = {
      path: ".gitignore",
      content: ".tmp-gocache/\n",
      binary: false,
      exists: true,
    };
    const fetchMock = mockFetchOnce(payload);

    const out = await getRunFileContent("run-1", ".gitignore");

    expect(out).toEqual(payload);
    const [url] = firstFetchCall(fetchMock);
    expect(url).toContain("/runs/run-1/files/content");
    expect(url).toContain("path=.gitignore");
  });

  it("encodes traversal-looking paths rather than passing them raw", async () => {
    const fetchMock = mockFetchOnce({
      path: "a/b.txt",
      content: "",
      binary: false,
      exists: false,
    });

    await getRunFileContent("run-1", "a/b.txt");

    const [url] = firstFetchCall(fetchMock);
    // The slash is percent-encoded by URLSearchParams, so the server sees a
    // single ?path= value (still validated server-side by ValidateRelPath).
    expect(url).toContain("path=a%2Fb.txt");
  });
});

describe("saveRunFileContent", () => {
  it("PUTs the {path, content} body", async () => {
    const fetchMock = mockFetchOnce({
      path: ".gitignore",
      content: "node_modules/\n",
      binary: false,
      exists: true,
    });

    await saveRunFileContent("run-1", ".gitignore", "node_modules/\n");

    const [url, init] = firstFetchCall(fetchMock);
    expect(url).toContain("/runs/run-1/files/content");
    expect(init?.method).toBe("PUT");
    expect(JSON.parse(init?.body as string)).toEqual({
      path: ".gitignore",
      content: "node_modules/\n",
    });
  });
});
