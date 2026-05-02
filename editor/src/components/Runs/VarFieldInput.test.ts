import { describe, expect, it } from "vitest";
import { defaultStringFor } from "./VarFieldInput";

describe("defaultStringFor", () => {
  it("returns empty string when no default literal", () => {
    expect(defaultStringFor({ name: "x", type: "string" })).toBe("");
  });

  it("returns 'false' for bool var without default", () => {
    expect(defaultStringFor({ name: "x", type: "bool" })).toBe("false");
  });

  // Regression: a string default of "" was being pre-filled as the literal
  // two-character string `""` because the Go encoder omits empty `str_val`,
  // leaving only `raw: "\"\""` for the editor to fall back on. Dispatching on
  // `kind` instead trusts the type tag over the presence of value fields.
  it("returns '' for an empty-string default (str_val omitted by encoder)", () => {
    expect(
      defaultStringFor({
        name: "scope_notes",
        type: "string",
        default: { kind: "string", raw: '""' },
      }),
    ).toBe("");
  });

  it("returns the str_val for a non-empty string default", () => {
    expect(
      defaultStringFor({
        name: "model",
        type: "string",
        default: { kind: "string", raw: '"opus"', str_val: "opus" },
      }),
    ).toBe("opus");
  });

  it("preserves env-var template strings verbatim", () => {
    expect(
      defaultStringFor({
        name: "workspace_dir",
        type: "string",
        default: { kind: "string", raw: '"${PROJECT_DIR}"', str_val: "${PROJECT_DIR}" },
      }),
    ).toBe("${PROJECT_DIR}");
  });

  it("formats int defaults", () => {
    expect(
      defaultStringFor({
        name: "max_iter",
        type: "int",
        default: { kind: "int", raw: "5", int_val: 5 },
      }),
    ).toBe("5");
  });

  it("returns '' for an int default of 0 (int_val omitted by encoder)", () => {
    // Mirror of the empty-string regression: int 0 is also the omitempty
    // zero-value, so int_val gets dropped and the editor must not fall
    // back to raw or a misleading sentinel.
    expect(
      defaultStringFor({
        name: "offset",
        type: "int",
        default: { kind: "int", raw: "0" },
      }),
    ).toBe("");
  });

  it("formats float defaults", () => {
    expect(
      defaultStringFor({
        name: "temp",
        type: "float",
        default: { kind: "float", raw: "0.7", float_val: 0.7 },
      }),
    ).toBe("0.7");
  });

  it("formats bool defaults", () => {
    expect(
      defaultStringFor({
        name: "verbose",
        type: "bool",
        default: { kind: "bool", raw: "true", bool_val: true },
      }),
    ).toBe("true");
    expect(
      defaultStringFor({
        name: "verbose",
        type: "bool",
        default: { kind: "bool", raw: "false", bool_val: false },
      }),
    ).toBe("false");
  });

  it("returns 'false' for a bool default whose bool_val=false is omitted", () => {
    // bool false is also an omitempty zero value; the kind-based dispatch
    // must default to "false" rather than the raw token.
    expect(
      defaultStringFor({
        name: "verbose",
        type: "bool",
        default: { kind: "bool", raw: "false" },
      }),
    ).toBe("false");
  });
});
