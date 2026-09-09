import { describe, expect, it } from "vitest";
import { iterLanguageConfig, iterTokensProvider } from "./iterLanguage";

type Rule = [RegExp, unknown];

const rulesOf = (state: string): Rule[] =>
  (iterTokensProvider.tokenizer as Record<string, unknown[]>)[state] as Rule[];

const indexOfSource = (rules: Rule[], source: string) =>
  rules.findIndex(([re]) => re.source === source);

describe("iter tokenizer", () => {
  it("opens a raw string before it can read a hash as a comment", () => {
    // `command: \`echo "#1"\`` — the hash is shell, not a comment. Monarch
    // tries the rules in order, so the backtick opener must precede the
    // comment rule or the rest of the line is painted as a comment.
    const root = rulesOf("root");
    const raw = indexOfSource(root, "`");
    const comment = indexOfSource(root, "#.*$");
    expect(raw).toBeGreaterThanOrEqual(0);
    expect(comment).toBeGreaterThanOrEqual(0);
    expect(raw).toBeLessThan(comment);
  });

  it("keeps a hash inside a raw string as string text until the closing backtick", () => {
    const raw = rulesOf("rawString");
    expect(raw).toBeDefined();
    // Nothing in the raw-string state may yield a comment token.
    for (const [, action] of raw) {
      expect(JSON.stringify(action)).not.toContain("comment");
    }
    // Only the closing backtick pops the state.
    const closer = raw.find(([re]) => re.source === "`");
    expect(closer).toBeDefined();
    expect(JSON.stringify(closer?.[1])).toContain("@pop");
  });

  it("comments open on a single hash", () => {
    expect(iterLanguageConfig.comments?.lineComment).toBe("#");
  });
});
