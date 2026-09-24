import { afterEach, describe, expect, it } from "vitest";

import { createEmptyDocument } from "@/lib/defaults";
import { createDocumentStore } from "@/store/document";
import { useUIStore } from "@/store/ui";

import { replaceDocument, replaceDocumentNow } from "./replaceDocument";

// The one path every explicit replacement takes once its confirm is
// answered. What it certifies is the moment the ANSWER lands, not the click.

function deferred<T>() {
  let resolve!: (v: T) => void;
  let reject!: (e: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

function openStore() {
  const store = createDocumentStore();
  store.getState().setDocument(createEmptyDocument());
  store.getState().setCurrentFilePath("bots/a.bot");
  store.getState().markSaved();
  return store;
}

const applyPath = (answer: string, s: { setCurrentFilePath: (p: string) => void }) =>
  s.setCurrentFilePath(answer);

afterEach(() => useUIStore.setState({ toasts: [] }));

describe("replaceDocument", () => {
  it("applies an answer when nothing moved while it loaded", async () => {
    const store = openStore();
    const load = deferred<string>();
    const outcome = replaceDocument(store, "b.bot", () => load.promise, applyPath);
    load.resolve("bots/b.bot");
    expect(await outcome).toBe("applied");
    expect(store.getState().currentFilePath).toBe("bots/b.bot");
  });

  it("drops, quietly, an answer to a request a newer one superseded", async () => {
    const store = openStore();
    const first = deferred<string>();
    const second = deferred<string>();
    const a = replaceDocument(store, "b.bot", () => first.promise, applyPath);
    const b = replaceDocument(store, "c.bot", () => second.promise, applyPath);
    // The OLDER answer lands last: the later request still wins.
    second.resolve("bots/c.bot");
    expect(await b).toBe("applied");
    first.resolve("bots/b.bot");
    expect(await a).toBe("superseded");
    expect(store.getState().currentFilePath).toBe("bots/c.bot");
    expect(useUIStore.getState().toasts).toEqual([]);
  });

  it("refuses an answer once the document took edits, and says so", async () => {
    const store = openStore();
    const load = deferred<string>();
    const outcome = replaceDocument(store, "b.bot", () => load.promise, applyPath);
    store.getState().setDocument({ ...createEmptyDocument(), comments: [{ text: "typed meanwhile" }] });
    load.resolve("bots/b.bot");
    expect(await outcome).toBe("refused");
    expect(store.getState().currentFilePath).toBe("bots/a.bot");
    expect(useUIStore.getState().toasts.map((t) => t.message).join(" ")).toContain("b.bot");
  });

  it("refuses an answer once the Source view's text moved, which moves no generation", async () => {
    const store = openStore();
    const load = deferred<string>();
    const outcome = replaceDocument(store, "b.bot", () => load.promise, applyPath);
    const generation = store.getState()._generation;
    store.getState().setSourceBuffer({
      path: "bots/a.bot",
      rel: null,
      text: "typed meanwhile",
      base: "rendered",
      doc: null,
      session: 1,
    });
    expect(store.getState()._generation).toBe(generation);
    load.resolve("bots/b.bot");
    expect(await outcome).toBe("refused");
    expect(store.getState().sourceBuffer?.text).toBe("typed meanwhile");
  });

  it("is pending while it loads, and only its own end clears that", async () => {
    const store = openStore();
    const first = deferred<string>();
    const second = deferred<string>();
    const a = replaceDocument(store, "b.bot", () => first.promise, applyPath);
    expect(store.getState()._pendingIntent).not.toBeNull();
    const b = replaceDocument(store, "c.bot", () => second.promise, applyPath);
    const newest = store.getState()._pendingIntent;
    first.resolve("bots/b.bot");
    await a;
    // The older request ending must not clear the newer one's pending mark.
    expect(store.getState()._pendingIntent).toBe(newest);
    second.resolve("bots/c.bot");
    await b;
    expect(store.getState()._pendingIntent).toBeNull();
  });

  it("lets a failed load throw to its caller, and still ends", async () => {
    const store = openStore();
    const load = deferred<string>();
    const outcome = replaceDocument(store, "b.bot", () => load.promise, applyPath);
    load.reject(new Error("404: not found"));
    await expect(outcome).rejects.toThrow("404");
    expect(store.getState()._pendingIntent).toBeNull();
    expect(store.getState().currentFilePath).toBe("bots/a.bot");
  });
});

describe("replaceDocumentNow", () => {
  it("supersedes a replacement still loading, and leaves nothing pending", async () => {
    // File → New and Start blank: no answer to wait for, but still the
    // author's latest request.
    const store = openStore();
    const load = deferred<string>();
    const pending = replaceDocument(store, "b.bot", () => load.promise, applyPath);
    replaceDocumentNow(store, (s) => s.setCurrentFilePath(null));
    expect(store.getState()._pendingIntent).toBeNull();
    load.resolve("bots/b.bot");
    expect(await pending).toBe("superseded");
    expect(store.getState().currentFilePath).toBeNull();
  });
});

describe("the applied-replacement count", () => {
  // What a save's answer settles on: replacements that LANDED, not ones asked
  // for — an Open that fails or is refused leaves the saved document on screen.
  it("moves when a replacement lands, and only then", async () => {
    const store = openStore();
    const at = store.getState()._replaced;

    replaceDocumentNow(store, (s) => s.setCurrentFilePath(null));
    expect(store.getState()._replaced).toBe(at + 1);

    expect(await replaceDocument(store, "b.bot", async () => "bots/b.bot", applyPath)).toBe("applied");
    expect(store.getState()._replaced).toBe(at + 2);

    const refused = deferred<string>();
    const pending = replaceDocument(store, "c.bot", () => refused.promise, applyPath);
    store.getState().setDocument({ ...createEmptyDocument(), comments: [{ text: "edited" }] });
    refused.resolve("bots/c.bot");
    expect(await pending).toBe("refused");
    await expect(replaceDocument(store, "d.bot", async () => Promise.reject(new Error("404")), applyPath)).rejects.toThrow();
    expect(store.getState()._replaced).toBe(at + 2);
  });
});
