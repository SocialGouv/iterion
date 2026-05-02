import { describe, expect, it } from "vitest";
import { highlightPromptBody, type HighlightChunk } from "./promptHighlight";

const concatPreserved = (src: string, chunks: HighlightChunk[]) =>
  chunks.map((c) => c.text).join("") === src;

describe("highlightPromptBody", () => {
  it("returns [] for an empty string", () => {
    expect(highlightPromptBody("")).toEqual([]);
  });

  it("returns a single text chunk when there is nothing to highlight", () => {
    expect(highlightPromptBody("just plain prose")).toEqual([
      { kind: "text", text: "just plain prose" },
    ]);
  });

  it("highlights {{...}} as ref", () => {
    const out = highlightPromptBody("hello {{vars.x}} world");
    expect(out).toEqual([
      { kind: "text", text: "hello " },
      { kind: "ref", text: "{{vars.x}}" },
      { kind: "text", text: " world" },
    ]);
  });

  it("highlights ${...} as envvar", () => {
    const out = highlightPromptBody("model=${ANTHROPIC_MODEL} ok");
    expect(out).toEqual([
      { kind: "text", text: "model=" },
      { kind: "envvar", text: "${ANTHROPIC_MODEL}" },
      { kind: "text", text: " ok" },
    ]);
  });

  it("interleaves multiple ref/envvar tokens with no empty text gaps", () => {
    const src = "${ENV} and {{outputs.a.b}}";
    const out = highlightPromptBody(src);
    expect(out).toEqual([
      { kind: "envvar", text: "${ENV}" },
      { kind: "text", text: " and " },
      { kind: "ref", text: "{{outputs.a.b}}" },
    ]);
    expect(concatPreserved(src, out)).toBe(true);
  });

  it("handles adjacent refs without inserting empty text chunks", () => {
    const out = highlightPromptBody("{{a}}{{b}}");
    expect(out).toEqual([
      { kind: "ref", text: "{{a}}" },
      { kind: "ref", text: "{{b}}" },
    ]);
  });

  it("falls back to plain text for unterminated {{", () => {
    const src = "foo {{ bar baz";
    const out = highlightPromptBody(src);
    expect(out).toEqual([{ kind: "text", text: "foo {{ bar baz" }]);
    expect(concatPreserved(src, out)).toBe(true);
  });

  it("falls back to plain text for unterminated ${", () => {
    const src = "value=${UNTERMINATED";
    const out = highlightPromptBody(src);
    expect(out).toEqual([{ kind: "text", text: "value=${UNTERMINATED" }]);
  });

  it("recognises ## line comments", () => {
    const out = highlightPromptBody("## a note\nbody");
    expect(out).toEqual([
      { kind: "comment", text: "## a note" },
      { kind: "text", text: "\nbody" },
    ]);
  });

  it("only treats ## as comment, not # alone", () => {
    const out = highlightPromptBody("#solo not a comment");
    expect(out).toEqual([{ kind: "text", text: "#solo not a comment" }]);
  });

  it("preserves the input when concatenated across complex inputs", () => {
    const src = "## hi\nplease use {{vars.feature}} with ${SHELL} and a literal {{ unclosed";
    const out = highlightPromptBody(src);
    expect(concatPreserved(src, out)).toBe(true);
  });
});
