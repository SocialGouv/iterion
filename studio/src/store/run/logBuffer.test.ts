import { describe, expect, it } from "vitest";

import {
  clampLogBody,
  sliceFromByteOffset,
  sliceToByteOffset,
  utf8Len,
} from "./logBuffer";

// The replay scrubber cuts the log at Event.log_offset — a BYTE
// position — so the head slice must count UTF-8 bytes, not UTF-16 code
// units. The run console is dense with multi-byte glyphs (ℹ️, 🔧, ▸),
// which is exactly where a code-unit slice drifts.
describe("sliceToByteOffset", () => {
  it("cuts ASCII at the byte position", () => {
    expect(sliceToByteOffset("hello world", 5)).toBe("hello");
  });

  it("returns the whole string at or past its byte length", () => {
    expect(sliceToByteOffset("abc", 3)).toBe("abc");
    expect(sliceToByteOffset("abc", 99)).toBe("abc");
  });

  it("returns empty for byteLen <= 0", () => {
    expect(sliceToByteOffset("abc", 0)).toBe("");
    expect(sliceToByteOffset("abc", -1)).toBe("");
  });

  it("counts multi-byte glyphs by their UTF-8 width", () => {
    // "ℹ" is 3 bytes; "🔧" is 4 bytes (surrogate pair).
    expect(sliceToByteOffset("ℹab", 3)).toBe("ℹ");
    expect(sliceToByteOffset("ℹab", 4)).toBe("ℹa");
    expect(sliceToByteOffset("🔧ab", 4)).toBe("🔧");
    expect(sliceToByteOffset("🔧ab", 5)).toBe("🔧a");
  });

  it("never ends with a partial code point", () => {
    // Cutting inside the 4-byte 🔧 excludes it entirely.
    expect(sliceToByteOffset("🔧ab", 3)).toBe("");
    expect(sliceToByteOffset("aℹ", 2)).toBe("a");
  });

  it("complements sliceFromByteOffset at every boundary", () => {
    const s = "aℹ️ b🔧c ▸ d";
    const total = utf8Len(s);
    for (let cut = 0; cut <= total; cut++) {
      const head = sliceToByteOffset(s, cut);
      const tail = sliceFromByteOffset(s, cut);
      // On a code-point boundary the two halves reassemble exactly; on
      // a mid-code-point cut the straddling glyph is dropped from both
      // (head excludes it, tail skips it) — never duplicated.
      expect(utf8Len(head) <= cut).toBe(true);
      expect(head + tail === s || utf8Len(head) + utf8Len(tail) < total).toBe(
        true,
      );
    }
  });
});

// Regression for the "No log captured." replay bug: on a log larger
// than MAX_LOG_BYTES the store trims the head, and a scrub position
// below the window's start rendered as an empty panel. clampLogBody
// must serve those bytes from the fetched prefix instead.
describe("clampLogBody", () => {
  const start = 100; // window covers [100, …)
  const windowText = "window-bytes";
  const prefix = { until: start, text: "p".repeat(100) };

  it("returns the window untouched when not clamping", () => {
    expect(clampLogBody(windowText, start, null, null)).toBe(windowText);
    expect(clampLogBody(windowText, start, prefix, undefined)).toBe(windowText);
  });

  it("serves a clamp position inside the evicted head from the prefix", () => {
    expect(clampLogBody(windowText, start, prefix, 40)).toBe("p".repeat(40));
  });

  it("is empty (never stale window bytes) while the prefix is missing", () => {
    expect(clampLogBody(windowText, start, null, 40)).toBe("");
  });

  it("stitches prefix + window slice past the window start", () => {
    expect(clampLogBody(windowText, start, prefix, start + 6)).toBe(
      "p".repeat(100) + "window",
    );
  });

  it("ignores a stale prefix that no longer reaches the window start", () => {
    const stale = { until: 50, text: "p".repeat(50) };
    // Below the window with a stale prefix: nothing trustworthy to show.
    expect(clampLogBody(windowText, start, stale, 40)).toBe("");
    // Above the window start: window slice only, no misaligned stitch.
    expect(clampLogBody(windowText, start, stale, start + 6)).toBe("window");
  });

  it("clamps at exactly the stream end", () => {
    expect(clampLogBody(windowText, start, prefix, start + windowText.length)).toBe(
      prefix.text + windowText,
    );
  });
});
