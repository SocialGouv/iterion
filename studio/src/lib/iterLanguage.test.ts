import { describe, expect, it } from "vitest";
import { ITER_LANGUAGE_ID, iterLanguageConfig, iterTokensProvider } from "./iterLanguage";
import { tokenizeLines, typeAt } from "./monarchTokenize";

// Every assertion below runs monaco's REAL Monarch engine on the shipped
// definition (see monarchTokenize.ts), so it certifies what the editor
// paints — a rule-order check alone could not see a state that swallows a
// line, or a `#` painted as a comment inside text.
const paint = (lines: string[]) => tokenizeLines(ITER_LANGUAGE_ID, iterTokensProvider, lines);

describe("iter tokenizer", () => {
  it("paints a hash comment, a single hash included", () => {
    const [top, inline] = paint(["# top comment", 'agent a: # trailing']);
    expect(typeAt(top, 0)).toContain("comment");
    expect(typeAt(inline, 9)).toContain("comment");
    expect(typeAt(inline, 0)).toContain("keyword");
  });

  it("keeps a hash inside a backtick raw string as string text", () => {
    const [line] = paint(['  command: `echo "#1 ok"`']);
    expect(typeAt(line, 18)).toContain("string");
    expect(typeAt(line, 18)).not.toContain("comment");
  });

  it("keeps a prompt body's `# Heading` as text, and ends the body at the next declaration", () => {
    const lines = paint([
      "prompt p:",
      "  # Review checklist",
      "  Read {{vars.scope}} first.",
      "",
      "agent a:",
      "  # a real comment",
      '  model: "m"',
    ]);
    expect(typeAt(lines[1], 2)).not.toContain("comment");
    expect(typeAt(lines[1], 2)).toContain("string");
    expect(typeAt(lines[2], 7)).toContain("template");
    expect(typeAt(lines[4], 0)).toContain("keyword"); // `agent` — the body ended
    expect(typeAt(lines[5], 2)).toContain("comment"); // back in the block grammar
  });

  it("ends a prompt body declared inside a group at the group's next member", () => {
    const lines = paint([
      "group g:",
      "  prompt p:",
      "    # Heading inside a group",
      "    Body.",
      "  agent a:",
      '    model: "m"',
    ]);
    expect(typeAt(lines[2], 4)).not.toContain("comment");
    expect(typeAt(lines[4], 2)).toContain("keyword"); // `agent` at the prompt's own indent
    expect(typeAt(lines[5], 4)).toContain("keyword"); // `model`
  });

  it("keeps a shell comment inside a `|` block scalar as string text, and ends it at the next property", () => {
    const lines = paint([
      "tool t:",
      "  command: |",
      "    # not a DSL comment",
      "    echo hi",
      "  output: out",
    ]);
    expect(typeAt(lines[2], 4)).not.toContain("comment");
    expect(typeAt(lines[2], 4)).toContain("string");
    expect(typeAt(lines[4], 2)).toContain("keyword"); // `output` — the scalar ended
  });

  it("comments open on a single hash", () => {
    expect(iterLanguageConfig.comments?.lineComment).toBe("#");
  });
});
