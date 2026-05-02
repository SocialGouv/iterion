import { describe, expect, it } from "vitest";
import type { VarField, Literal } from "@/api/types";
import { isPromptLikeVar, suggestRows } from "./promptVarHeuristics";

const stringLit = (s: string): Literal => ({ kind: "string", raw: `"${s}"`, str_val: s });

const v = (name: string, type: VarField["type"] = "string", def?: Literal): VarField => ({
  name,
  type,
  default: def,
});

describe("isPromptLikeVar", () => {
  it("matches the _prompt suffix", () => {
    expect(isPromptLikeVar(v("system_prompt"))).toBe(true);
    expect(isPromptLikeVar(v("user_PROMPT"))).toBe(true);
  });

  it("matches the _description suffix", () => {
    expect(isPromptLikeVar(v("feature_description"))).toBe(true);
    expect(isPromptLikeVar(v("bug_Description"))).toBe(true);
  });

  it("matches the exact-name list (case-insensitive)", () => {
    expect(isPromptLikeVar(v("prompt"))).toBe(true);
    expect(isPromptLikeVar(v("PROMPT"))).toBe(true);
    expect(isPromptLikeVar(v("description"))).toBe(true);
    expect(isPromptLikeVar(v("instructions"))).toBe(true);
  });

  it("flags string vars without a default as prompt-like", () => {
    expect(isPromptLikeVar(v("workspace_dir"))).toBe(true);
    expect(isPromptLikeVar(v("feature"))).toBe(true);
  });

  it("does not flag string vars with a non-empty default", () => {
    expect(isPromptLikeVar(v("workspace_dir", "string", stringLit(".")))).toBe(false);
    expect(isPromptLikeVar(v("language", "string", stringLit("en")))).toBe(false);
  });

  it("treats an empty-string default as no default (still prompt-like)", () => {
    expect(isPromptLikeVar(v("foo", "string", stringLit("")))).toBe(true);
    expect(isPromptLikeVar(v("foo", "string", { kind: "string", raw: '""' }))).toBe(true);
  });

  it("ignores non-string types regardless of name", () => {
    expect(isPromptLikeVar(v("system_prompt", "int"))).toBe(false);
    expect(isPromptLikeVar(v("description", "json"))).toBe(false);
    expect(isPromptLikeVar(v("foo", "string[]"))).toBe(false);
    expect(isPromptLikeVar(v("flag", "bool"))).toBe(false);
    expect(isPromptLikeVar(v("count", "float"))).toBe(false);
  });

  it("name suffix wins even when a default is provided", () => {
    // A var named *_prompt with a default is still a prompt body.
    expect(isPromptLikeVar(v("system_prompt", "string", stringLit("default text")))).toBe(true);
  });
});

describe("suggestRows", () => {
  it("gives more rows when the suffix rule fires", () => {
    expect(suggestRows(v("system_prompt"))).toBeGreaterThanOrEqual(8);
    expect(suggestRows(v("feature_description"))).toBeGreaterThanOrEqual(8);
  });

  it("gives more rows for the exact-name list", () => {
    expect(suggestRows(v("prompt"))).toBeGreaterThanOrEqual(8);
    expect(suggestRows(v("instructions"))).toBeGreaterThanOrEqual(8);
  });

  it("falls back to a moderate row count for plain string vars", () => {
    expect(suggestRows(v("workspace_dir"))).toBeGreaterThanOrEqual(6);
    expect(suggestRows(v("workspace_dir"))).toBeLessThan(8);
  });
});
