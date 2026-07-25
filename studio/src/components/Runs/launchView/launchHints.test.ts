import { describe, expect, it } from "vitest";

import type { VarField } from "@/api/types";

import { applyLaunchHints } from "./launchHints";

function strVar(name: string, def?: string): VarField {
  return {
    name,
    type: "string",
    default:
      def === undefined
        ? undefined
        : { kind: "string", raw: JSON.stringify(def), str_val: def },
  };
}

function names(fields: VarField[]): string[] {
  return fields.map((f) => f.name);
}

// A representative bot: two heuristic primaries (required, no default),
// three optional-with-default vars, one runner-resolved auto var.
const FIELDS: VarField[] = [
  strVar("feature_prompt"), // heuristic primary
  strVar("app_prompt", ""), // has a default → heuristic advanced
  strVar("scope", "whole repo"), // heuristic advanced
  strVar("target_branch"), // heuristic primary
  strVar("report_path", "${PROJECT_DIR}/report.md"), // auto
  strVar("max_retries", "3"), // heuristic advanced
];

describe("applyLaunchHints — no hints", () => {
  it.each([[undefined], [null], [{}]])("hints=%s keeps the heuristic buckets", (hints) => {
    const b = applyLaunchHints(FIELDS, hints as never);
    expect(names(b.primary)).toEqual(["feature_prompt", "target_branch"]);
    expect(names(b.advanced)).toEqual(["app_prompt", "scope", "max_retries"]);
    expect(names(b.auto)).toEqual(["report_path"]);
    expect(b.hintedPrimary.size).toBe(0);
  });
});

describe("applyLaunchHints — primary forcing", () => {
  it("forces hinted names into primary, in hinted order, before heuristic primaries", () => {
    const b = applyLaunchHints(FIELDS, { primary: ["scope", "app_prompt"] });
    expect(names(b.primary)).toEqual([
      "scope",
      "app_prompt",
      "feature_prompt",
      "target_branch",
    ]);
    // Forced names leave their heuristic bucket.
    expect(names(b.advanced)).toEqual(["max_retries"]);
    expect(names(b.auto)).toEqual(["report_path"]);
    expect([...b.hintedPrimary]).toEqual(["scope", "app_prompt"]);
  });

  it("keeps the heuristic primaries' relative order after the hinted ones", () => {
    const b = applyLaunchHints(FIELDS, { primary: ["max_retries"] });
    expect(names(b.primary)).toEqual(["max_retries", "feature_prompt", "target_branch"]);
  });

  it("can force an auto-managed var into primary", () => {
    const b = applyLaunchHints(FIELDS, { primary: ["report_path"] });
    expect(names(b.primary)[0]).toBe("report_path");
    expect(b.auto).toEqual([]);
  });

  it("a name both hinted and heuristically primary appears once, at its hinted position", () => {
    const b = applyLaunchHints(FIELDS, { primary: ["target_branch", "app_prompt"] });
    expect(names(b.primary)).toEqual(["target_branch", "app_prompt", "feature_prompt"]);
  });

  it("a name repeated inside the hint list appears once, at its first position", () => {
    const b = applyLaunchHints(FIELDS, { primary: ["scope", "app_prompt", "scope"] });
    expect(names(b.primary)).toEqual([
      "scope",
      "app_prompt",
      "feature_prompt",
      "target_branch",
    ]);
  });

  it("ignores unknown names silently", () => {
    const b = applyLaunchHints(FIELDS, { primary: ["nope", "app_prompt", "also_nope"] });
    expect(names(b.primary)).toEqual(["app_prompt", "feature_prompt", "target_branch"]);
    expect(b.hintedPrimary.has("nope")).toBe(false);
  });

  it("does not change requiredness — the hinted field keeps its default", () => {
    const b = applyLaunchHints(FIELDS, { primary: ["app_prompt"] });
    const hinted = b.primary.find((f) => f.name === "app_prompt");
    // Same field object, default intact: isVarRequired still sees an
    // optional var — hints never touch validation.
    expect(hinted).toBe(FIELDS[1]);
    expect(hinted?.default).toBeDefined();
  });
});

describe("applyLaunchHints — hidden filtering", () => {
  it("removes hidden names from every bucket", () => {
    const b = applyLaunchHints(FIELDS, {
      hidden: ["feature_prompt", "scope", "report_path"],
    });
    expect(names(b.primary)).toEqual(["target_branch"]);
    expect(names(b.advanced)).toEqual(["app_prompt", "max_retries"]);
    expect(b.auto).toEqual([]);
  });

  it("ignores unknown hidden names silently", () => {
    const b = applyLaunchHints(FIELDS, { hidden: ["ghost"] });
    expect(names(b.primary)).toEqual(["feature_prompt", "target_branch"]);
    expect(names(b.advanced)).toEqual(["app_prompt", "scope", "max_retries"]);
    expect(names(b.auto)).toEqual(["report_path"]);
  });

  it("hidden wins over primary for the same name — never rendered", () => {
    const b = applyLaunchHints(FIELDS, {
      primary: ["scope", "app_prompt"],
      hidden: ["scope"],
    });
    expect(names(b.primary)).toEqual(["app_prompt", "feature_prompt", "target_branch"]);
    expect(names(b.advanced)).toEqual(["max_retries"]);
    expect(b.hintedPrimary.has("scope")).toBe(false);
  });
});
