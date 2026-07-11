import { describe, expect, it } from "vitest";

import { deriveSlug, isValidSlug, isValidVarName } from "./slug";

describe("deriveSlug", () => {
  it("lowercases and turns spaces/underscores into dashes", () => {
    expect(deriveSlug("My Review Bot")).toBe("my-review-bot");
    expect(deriveSlug("docs_refresh helper")).toBe("docs-refresh-helper");
  });

  it("strips invalid characters and collapses dash runs", () => {
    expect(deriveSlug("Hello, World! (v2)")).toBe("hello-world-v2");
    expect(deriveSlug("a  --  b")).toBe("a-b");
  });

  it("folds accents instead of dropping the letters", () => {
    expect(deriveSlug("Résumé Bot")).toBe("resume-bot");
  });

  it("trims leading digits/dashes so the slug starts with a letter", () => {
    expect(deriveSlug("123 fixer")).toBe("fixer");
    expect(deriveSlug("--lead")).toBe("lead");
  });

  it("trims trailing dashes and caps at 64 chars", () => {
    expect(deriveSlug("tail-  ")).toBe("tail");
    const long = `bot ${"x".repeat(100)}`;
    const slug = deriveSlug(long);
    expect(slug.length).toBeLessThanOrEqual(64);
    expect(isValidSlug(slug)).toBe(true);
  });

  it("can produce an invalid (empty or too-short) slug the form flags", () => {
    expect(deriveSlug("")).toBe("");
    expect(deriveSlug("!!!")).toBe("");
    expect(isValidSlug(deriveSlug("a"))).toBe(false); // 1 char < min 2
    expect(isValidSlug("")).toBe(false);
  });

  it("accepts already-valid slugs unchanged", () => {
    expect(deriveSlug("sec-audit-source")).toBe("sec-audit-source");
    expect(isValidSlug("sec-audit-source")).toBe(true);
  });
});

describe("isValidVarName", () => {
  it("accepts snake_case names", () => {
    expect(isValidVarName("feature_prompt")).toBe(true);
    expect(isValidVarName("_private")).toBe(true);
    expect(isValidVarName("v2")).toBe(true);
  });
  it("rejects invalid names", () => {
    expect(isValidVarName("")).toBe(false);
    expect(isValidVarName("2fast")).toBe(false);
    expect(isValidVarName("Camel")).toBe(false);
    expect(isValidVarName("with-dash")).toBe(false);
    expect(isValidVarName("with space")).toBe(false);
  });
});
