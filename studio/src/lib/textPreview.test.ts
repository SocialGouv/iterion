import { describe, expect, it } from "vitest";

import { prettyJSON, textPreviewKind } from "./textPreview";

describe("textPreviewKind", () => {
  it("classifies JSON from mime or extension", () => {
    expect(textPreviewKind("application/json", "outline.json")).toBe("json");
    expect(textPreviewKind("application/octet-stream", "tokens.json")).toBe("json");
    expect(textPreviewKind("application/json; charset=utf-8", "x")).toBe("json");
  });

  it("classifies markdown from mime or extension", () => {
    expect(textPreviewKind("text/markdown", "brief")).toBe("markdown");
    expect(textPreviewKind("text/plain", "outline_review.md")).toBe("markdown");
    expect(textPreviewKind("", "notes.mdx")).toBe("markdown");
  });

  it("classifies other text documents", () => {
    expect(textPreviewKind("text/plain", "notes.txt")).toBe("text");
    expect(textPreviewKind("application/yaml", "cfg.yaml")).toBe("text");
    expect(textPreviewKind("", "log.csv")).toBe("text");
  });

  it("leaves media and unknowns to the download fallback", () => {
    expect(textPreviewKind("image/png", "shot.png")).toBeNull();
    expect(textPreviewKind("video/mp4", "clip.mp4")).toBeNull();
    expect(textPreviewKind("audio/wav", "mix.wav")).toBeNull();
    expect(textPreviewKind("application/zip", "bundle.zip")).toBeNull();
    expect(textPreviewKind("", "Makefile")).toBeNull();
  });

  it("does not re-preview types neutralizeActiveMIME downgrades", () => {
    expect(textPreviewKind("application/octet-stream", "data.xml")).toBeNull();
    expect(textPreviewKind("application/xml", "cfg.xml")).toBeNull();
    expect(textPreviewKind("text/xml", "x")).toBeNull();
    expect(textPreviewKind("text/html", "page.html")).toBeNull();
    expect(textPreviewKind("image/svg+xml", "icon.svg")).toBeNull();
    expect(textPreviewKind("text/javascript", "app.js")).toBeNull();
    expect(textPreviewKind("application/octet-stream", "notes.txt")).toBe("text");
  });
});

describe("prettyJSON", () => {
  it("indents valid JSON and leaves invalid bodies alone", () => {
    expect(prettyJSON('{"a":1}')).toBe('{\n  "a": 1\n}');
    expect(prettyJSON("not json")).toBe("not json");
  });
});
