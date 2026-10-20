// @vitest-environment jsdom
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { FieldDialog } from "./Fields";

afterEach(cleanup);

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
