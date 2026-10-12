import { describe, expect, it } from "vitest";

import { anyUnsavedBuffer, registerUnsavedBuffer } from "./unsavedBuffers";

// The registry answers for buffers no document store owns. Every probe this
// suite registers is released before the next case, so no answer leaks
// between tests.
describe("unsavedBuffers", () => {
  it("answers false while nothing is registered", () => {
    expect(anyUnsavedBuffer()).toBe(false);
  });

  it("answers for a registered probe, and only while it says dirty", () => {
    let dirty = false;
    const release = registerUnsavedBuffer(() => dirty);
    try {
      expect(anyUnsavedBuffer()).toBe(false);
      dirty = true;
      expect(anyUnsavedBuffer()).toBe(true);
      dirty = false;
      expect(anyUnsavedBuffer()).toBe(false);
    } finally {
      release();
    }
  });

  it("answers true while ANY of the probes is dirty", () => {
    const releases = [
      registerUnsavedBuffer(() => false),
      registerUnsavedBuffer(() => true),
      registerUnsavedBuffer(() => false),
    ];
    try {
      expect(anyUnsavedBuffer()).toBe(true);
    } finally {
      for (const release of releases) release();
    }
  });

  it("stops answering for a released probe", () => {
    const release = registerUnsavedBuffer(() => true);
    release();
    expect(anyUnsavedBuffer()).toBe(false);
  });

  it("survives a probe released twice", () => {
    const release = registerUnsavedBuffer(() => true);
    release();
    expect(() => release()).not.toThrow();
    expect(anyUnsavedBuffer()).toBe(false);
  });
});
