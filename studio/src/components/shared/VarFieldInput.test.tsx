// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";

import type { VarField } from "@/api/types";

import VarFieldInput, { defaultStringFor } from "./VarFieldInput";

afterEach(cleanup);

const enumField = (over: Partial<VarField> = {}): VarField => ({
  name: "mode",
  type: "string",
  enum: ["fast", "thorough"],
  default: { kind: "string", raw: '"fast"', str_val: "fast" },
  ...over,
});

const selectEl = () => screen.getByRole("combobox") as HTMLSelectElement;

describe("VarFieldInput enum select", () => {
  it("renders a select with the enum values verbatim", () => {
    render(<VarFieldInput field={enumField()} value="fast" onChange={() => {}} />);
    const opts = screen.getAllByRole("option") as HTMLOptionElement[];
    expect(opts.map((o) => o.value)).toEqual(["fast", "thorough"]);
    expect(opts.map((o) => o.textContent)).toEqual(["fast", "thorough"]);
  });

  it("preselects the current (default) value", () => {
    render(<VarFieldInput field={enumField()} value="thorough" onChange={() => {}} />);
    expect(selectEl().value).toBe("thorough");
  });

  it("propagates a picked option via onChange", () => {
    const onChange = vi.fn();
    render(<VarFieldInput field={enumField()} value="fast" onChange={onChange} />);
    fireEvent.change(selectEl(), { target: { value: "thorough" } });
    expect(onChange).toHaveBeenCalledWith("thorough");
  });

  it("keeps an out-of-list value visible as a disabled '(invalid: x)' option", () => {
    // Stale preset / query param: the operator must SEE the bad value,
    // not have the form silently snap to another one.
    render(<VarFieldInput field={enumField()} value="legacy" onChange={() => {}} />);
    expect(selectEl().value).toBe("legacy");
    const invalid = screen.getByRole("option", {
      name: "(invalid: legacy)",
    }) as HTMLOptionElement;
    expect(invalid.disabled).toBe(true);
    expect(invalid.selected).toBe(true);
    // The real choices are still offered.
    expect(
      (screen.getAllByRole("option") as HTMLOptionElement[]).map((o) => o.value),
    ).toEqual(["legacy", "fast", "thorough"]);
  });

  it("renders a disabled placeholder when the value is empty (no default)", () => {
    render(
      <VarFieldInput field={enumField({ default: undefined })} value="" onChange={() => {}} />,
    );
    expect(selectEl().value).toBe("");
    const placeholder = screen.getByRole("option", {
      name: "Select a value…",
    }) as HTMLOptionElement;
    expect(placeholder.disabled).toBe(true);
  });

  it("wins over a forced promptLike (launch-hint prominence): no textarea", () => {
    render(
      <VarFieldInput
        field={enumField({ name: "prompt", default: undefined })}
        value=""
        onChange={() => {}}
        promptLike
      />,
    );
    // Even a prompt-named, default-less var forced prominent renders the
    // select — never the prompt textarea — once it carries an enum.
    expect(screen.getByRole("combobox")).toBeTruthy();
    expect(document.querySelector("textarea")).toBeNull();
  });

  it("forwards id/required/invalid to the select", () => {
    render(
      <VarFieldInput
        field={enumField()}
        value="legacy"
        onChange={() => {}}
        id="var-mode"
        required
        invalid
      />,
    );
    const sel = selectEl();
    expect(sel.id).toBe("var-mode");
    expect(sel.getAttribute("aria-required")).toBe("true");
    expect(sel.getAttribute("aria-invalid")).toBe("true");
  });

  it("leaves non-enum string vars unchanged (input / prompt textarea)", () => {
    // With a default → single-line input, exactly as before.
    const { unmount } = render(
      <VarFieldInput field={enumField({ enum: undefined })} value="fast" onChange={() => {}} />,
    );
    expect(screen.getByRole("textbox")).toBeTruthy();
    expect(screen.queryByRole("combobox")).toBeNull();
    unmount();
    // Prompt-like (no default) → textarea, exactly as before.
    render(
      <VarFieldInput
        field={enumField({ enum: undefined, default: undefined })}
        value=""
        onChange={() => {}}
      />,
    );
    expect(document.querySelector("textarea")).toBeTruthy();
    expect(screen.queryByRole("combobox")).toBeNull();
  });

  it("ignores an empty enum list (renders the plain input)", () => {
    render(<VarFieldInput field={enumField({ enum: [] })} value="fast" onChange={() => {}} />);
    expect(screen.getByRole("textbox")).toBeTruthy();
    expect(screen.queryByRole("combobox")).toBeNull();
  });
});

describe("defaultStringFor", () => {
  it("returns empty string when no default literal", () => {
    expect(defaultStringFor({ name: "x", type: "string" })).toBe("");
  });

  it("returns 'false' for bool var without default", () => {
    expect(defaultStringFor({ name: "x", type: "bool" })).toBe("false");
  });

  // Regression: a string default of "" was being pre-filled as the literal
  // two-character string `""` because the Go encoder omits empty `str_val`,
  // leaving only `raw: "\"\""` for the studio to fall back on. Dispatching on
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
    // zero-value, so int_val gets dropped and the studio must not fall
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
