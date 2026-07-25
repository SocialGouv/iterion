import { describe, expect, it } from "vitest";

import type { VarField } from "@/api/types";

import { classifyVar, isAutoManagedDefault, isAutoManagedVar } from "./varClassify";

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

describe("isAutoManagedDefault", () => {
  const cases: Array<[string, boolean]> = [
    ["${PROJECT_DIR}", true],
    ["${PROJECT_SCRATCH_DIR}", true],
    ["${PROJECT_DIR}/report.md", true],
    ["prefix ${PROJECT_SCRATCH_DIR}/out", true],
    ["${PROJECT_DIRX}", false],
    ["${OTHER_DIR}", false],
    ["$PROJECT_DIR", false], // no braces — not the runner placeholder
    ["plain value", false],
    ["", false],
  ];
  it.each(cases)("%s → %s", (def, expected) => {
    expect(isAutoManagedDefault(def)).toBe(expected);
  });
});

describe("isAutoManagedVar", () => {
  it("matches a string default carrying the placeholder", () => {
    expect(isAutoManagedVar(strVar("workspace_dir", "${PROJECT_DIR}"))).toBe(true);
  });

  it("matches a raw-only default (no str_val, e.g. omitempty encoding)", () => {
    const f: VarField = {
      name: "report_path",
      type: "string",
      default: { kind: "string", raw: '"${PROJECT_SCRATCH_DIR}/report.md"' },
    };
    expect(isAutoManagedVar(f)).toBe(true);
  });

  it("does not match a var without a default", () => {
    expect(isAutoManagedVar(strVar("feature_prompt"))).toBe(false);
  });

  it("does not match an ordinary default", () => {
    expect(isAutoManagedVar(strVar("scope", "whole repo"))).toBe(false);
  });
});

describe("classifyVar", () => {
  it("auto-managed defaults classify as auto", () => {
    expect(classifyVar(strVar("workspace_dir", "${PROJECT_DIR}"))).toBe("auto");
  });

  it("required vars (no default) classify as primary", () => {
    expect(classifyVar(strVar("feature_prompt"))).toBe("primary");
  });

  it("optional vars with a plain default classify as advanced", () => {
    expect(classifyVar(strVar("scope", "whole repo"))).toBe("advanced");
  });

  it("bool vars are never primary (effective default false)", () => {
    const f: VarField = { name: "post_to_board", type: "bool" };
    expect(classifyVar(f)).toBe("advanced");
  });
});
