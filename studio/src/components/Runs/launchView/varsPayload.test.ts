import { describe, expect, it } from "vitest";

import type { VarField } from "@/api/types";

import { buildVarsPayload } from "./varsPayload";

const fields: VarField[] = [
  // Required string — no default, seeded "".
  { name: "feature_prompt", type: "string" },
  // Auto-managed default the runner expands at start.
  {
    name: "workspace_dir",
    type: "string",
    default: { kind: "string", raw: '"${PROJECT_DIR}"', str_val: "${PROJECT_DIR}" },
  },
  // Ordinary optional string with a default.
  {
    name: "scope",
    type: "string",
    default: { kind: "string", raw: '"whole repo"', str_val: "whole repo" },
  },
  // Bool without a declared default — effective default "false".
  { name: "post_to_board", type: "bool" },
  // Int with a default.
  { name: "max_passes", type: "int", default: { kind: "int", raw: "5", int_val: 5 } },
];

/** Mirror the LaunchView seeding: every field starts at its default string. */
const seeded: Record<string, string> = {
  feature_prompt: "",
  workspace_dir: "${PROJECT_DIR}",
  scope: "whole repo",
  post_to_board: "false",
  max_passes: "5",
};

describe("buildVarsPayload", () => {
  it("omits every untouched default (auto-managed and normal) — undefined payload", () => {
    expect(buildVarsPayload(fields, seeded)).toBeUndefined();
  });

  it("sends only the touched values", () => {
    expect(
      buildVarsPayload(fields, {
        ...seeded,
        feature_prompt: "add dark mode",
        post_to_board: "true",
      }),
    ).toEqual({ feature_prompt: "add dark mode", post_to_board: "true" });
  });

  it("sends an auto-managed var only when overridden away from its default", () => {
    expect(
      buildVarsPayload(fields, { ...seeded, workspace_dir: "/tmp/elsewhere" }),
    ).toEqual({ workspace_dir: "/tmp/elsewhere" });
  });

  it("an override typed back to the exact default is treated as untouched", () => {
    expect(
      buildVarsPayload(fields, { ...seeded, scope: "whole repo" }),
    ).toBeUndefined();
  });

  it("empty-string defaults stay omitted, but a cleared non-empty default is sent", () => {
    expect(buildVarsPayload(fields, { ...seeded, scope: "" })).toEqual({ scope: "" });
  });

  it("ignores values for undeclared fields (preset-only keys are server-applied)", () => {
    expect(
      buildVarsPayload(fields, { ...seeded, undeclared: "x" }),
    ).toBeUndefined();
  });

  it("skips fields with no form value at all", () => {
    expect(buildVarsPayload(fields, {})).toBeUndefined();
  });

  it("preset keys compare against the preset value, not the declared default", () => {
    const preset = { scope: "pkg/dsl only", max_passes: "3" };
    // Preset applied, untouched: values match the preset → omit (the
    // server applies the preset itself).
    expect(
      buildVarsPayload(fields, { ...seeded, ...preset }, preset),
    ).toBeUndefined();
    // Operator reverts a preset-covered field back to the declared
    // default: it must be SENT, or the server-side preset value wins.
    expect(
      buildVarsPayload(
        fields,
        { ...seeded, ...preset, scope: "whole repo" },
        preset,
      ),
    ).toEqual({ scope: "whole repo" });
    // Keys the preset doesn't cover keep the declared-default baseline.
    expect(
      buildVarsPayload(fields, { ...seeded, ...preset, workspace_dir: "/x" }, preset),
    ).toEqual({ workspace_dir: "/x" });
  });
});
