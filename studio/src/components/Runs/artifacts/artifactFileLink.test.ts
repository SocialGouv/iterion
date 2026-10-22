import { describe, expect, it } from "vitest";
import {
  artifactFileFromSearch,
  artifactFileRunPath,
  isMarkdownType,
  runPathWithoutFile,
} from "./artifactFileLink";

describe("artifactFileFromSearch", () => {
  it("reads the requested file, with or without the leading '?'", () => {
    expect(artifactFileFromSearch("?file=rgaa-2026-10-22.md")).toBe("rgaa-2026-10-22.md");
    expect(artifactFileFromSearch("file=rgaa-2026-10-22.md&tab=x")).toBe("rgaa-2026-10-22.md");
  });

  it("decodes a subdirectory path encoded as one value", () => {
    expect(artifactFileFromSearch("?file=out%2Freport%20v2.md")).toBe("out/report v2.md");
  });

  it("is null when the key is absent or empty", () => {
    expect(artifactFileFromSearch("")).toBeNull();
    expect(artifactFileFromSearch("?tab=events")).toBeNull();
    expect(artifactFileFromSearch("?file=")).toBeNull();
    expect(artifactFileFromSearch("?file=%20")).toBeNull();
  });
});

describe("artifactFileRunPath", () => {
  it("stays on the run route and round-trips through artifactFileFromSearch", () => {
    const path = artifactFileRunPath("01a1619d", "out/report v2.md");
    expect(path.startsWith("/runs/01a1619d?")).toBe(true);
    expect(artifactFileFromSearch(path.slice(path.indexOf("?")))).toBe("out/report v2.md");
  });

  it("encodes a run id that is not URL-safe", () => {
    expect(artifactFileRunPath("a/b", "x.md")).toBe("/runs/a%2Fb?file=x.md");
  });

  it("keeps every other key of the current query — a foreign-store run stays on its store", () => {
    expect(artifactFileRunPath("r", "x.md", "?store=%2Fhome%2Fu%2F.iterion&file=old.md")).toBe(
      "/runs/r?store=%2Fhome%2Fu%2F.iterion&file=x.md",
    );
  });
});

describe("runPathWithoutFile", () => {
  it("drops only the file key", () => {
    expect(runPathWithoutFile("r", "?store=x&file=a.md")).toBe("/runs/r?store=x");
    expect(runPathWithoutFile("r", "?file=a.md")).toBe("/runs/r");
    expect(runPathWithoutFile("r", "")).toBe("/runs/r");
  });
});

describe("isMarkdownType", () => {
  it("accepts the artifact endpoint's markdown type with its charset", () => {
    expect(isMarkdownType("text/markdown; charset=utf-8")).toBe(true);
    expect(isMarkdownType("text/markdown")).toBe(true);
    expect(isMarkdownType("TEXT/MARKDOWN")).toBe(true);
  });

  it("refuses every other text type", () => {
    expect(isMarkdownType("text/plain; charset=utf-8")).toBe(false);
    expect(isMarkdownType("application/json")).toBe(false);
    expect(isMarkdownType("text/markdownish")).toBe(false);
    expect(isMarkdownType("")).toBe(false);
  });
});
