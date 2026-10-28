// @vitest-environment jsdom
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { FieldDialog } from "./Fields";
import { setupMatchMedia, expectNoViolationsStrict } from "@/__tests__/a11y/axeHelpers";

setupMatchMedia();

afterEach(cleanup);

// Radix renders the dialog in a portal, outside render()'s container.
function mountDialog(type: "enum" | "text"): HTMLElement {
  render(
    <FieldDialog
      mode="edit"
      field={{
        name: "size",
        display: "Size",
        type,
        enum_values: type === "enum" ? ["s", "m"] : undefined,
        required: false,
      }}
      existingNames={["other"]}
      busy={false}
      onCancel={() => {}}
      onSubmit={vi.fn()}
    />,
  );
  return screen.getByRole("dialog");
}

describe("FieldDialog", () => {
  it("associates the visible Enum values heading with the tag field (RGAA 11.1)", () => {
    render(
      <FieldDialog
        mode="edit"
        field={{
          name: "size",
          display: "Size",
          type: "enum",
          enum_values: ["s"],
          required: false,
        }}
        existingNames={[]}
        busy={false}
        onCancel={() => {}}
        onSubmit={vi.fn()}
      />,
    );
    const field = screen.getByLabelText("Enum values") as HTMLInputElement;
    expect(field.id).toBe("field-enum-values");
  });
});

describe("a11y / board FieldDialog", () => {
  // The enum shape renders the one control the text shape does not (the tag
  // field and its removable values), so both are audited rather than one.
  it("has no violation editing an enum field", async () => {
    await expectNoViolationsStrict(mountDialog("enum"), "FieldDialog (enum)");
  });

  it("has no violation editing a plain text field", async () => {
    await expectNoViolationsStrict(mountDialog("text"), "FieldDialog (text)");
  });
});
