import { beforeEach, describe, expect, it } from "vitest";

import { anyUnsavedBuffer } from "@/lib/unsavedBuffers";

import { bundleBufferKey, runFileBufferKey, useEditBuffersStore } from "./editBuffers";

beforeEach(() => {
  useEditBuffersStore.setState({ bundle: {}, runFiles: {} });
});

describe("editBuffers", () => {
  it("holds a bundle buffer keyed by team and slug, and releases it", () => {
    const key = bundleBufferKey("team-1", "demo");
    useEditBuffersStore.getState().setBundle(key, {
      rel: "skills/notes.md",
      value: "# mine\n",
      original: "# notes\n",
      created: false,
    });
    expect(useEditBuffersStore.getState().bundle[key]).toEqual({
      rel: "skills/notes.md",
      value: "# mine\n",
      original: "# notes\n",
      created: false,
    });

    useEditBuffersStore.getState().setBundle(key, null);
    expect(useEditBuffersStore.getState().bundle[key]).toBeUndefined();
  });

  it("holds buffers for two bundles apart", () => {
    const demo = bundleBufferKey("team-1", "demo");
    const other = bundleBufferKey("team-1", "other");
    useEditBuffersStore.getState().setBundle(demo, {
      rel: "a.md",
      value: "demo text",
      original: "",
      created: false,
    });
    expect(useEditBuffersStore.getState().bundle[other]).toBeUndefined();
  });

  it("holds run-file buffers keyed by run and path, beside the bundle ones", () => {
    const key = runFileBufferKey("run-1", ".gitignore");
    useEditBuffersStore.getState().setRunFile(key, { value: "tmp/\n", original: "" });
    expect(useEditBuffersStore.getState().runFiles[key]).toEqual({
      value: "tmp/\n",
      original: "",
    });
    expect(useEditBuffersStore.getState().bundle).toEqual({});
  });

  it("answers the unsaved-buffer registry while any entry is dirty, and only then", () => {
    expect(anyUnsavedBuffer()).toBe(false);

    const demo = bundleBufferKey("team-1", "demo");
    useEditBuffersStore
      .getState()
      .setBundle(demo, { rel: "a.md", value: "typed", original: "typed", created: false });
    // Clean: value equals original.
    expect(anyUnsavedBuffer()).toBe(false);

    useEditBuffersStore
      .getState()
      .setBundle(demo, { rel: "a.md", value: "typed more", original: "typed", created: false });
    expect(anyUnsavedBuffer()).toBe(true);

    // A clean run-file buffer does not answer, a dirty one does, and the
    // answer outlives the surface that typed it — it is read at ask time.
    const runKey = runFileBufferKey("run-1", ".gitignore");
    useEditBuffersStore.getState().setRunFile(runKey, { value: "same", original: "same" });
    useEditBuffersStore.getState().setBundle(demo, null);
    expect(anyUnsavedBuffer()).toBe(false);
    useEditBuffersStore.getState().setRunFile(runKey, { value: "changed", original: "same" });
    expect(anyUnsavedBuffer()).toBe(true);
  });
});
