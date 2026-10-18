// @vitest-environment jsdom
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { TagListField } from "./FormField";

afterEach(cleanup);

describe("TextField", () => {
  it("associates the FieldRow label with a reference-aware field too", async () => {
    const { TextField } = await import("./FormField");
    render(
      <TextField
        label="Value"
        value=""
        onChange={vi.fn()}
        multiline
        refContext={{ kind: "edge-with" } as never}
      />,
    );
    const input = screen.getByLabelText("Value") as HTMLInputElement;
    expect(input.tagName).toBe("TEXTAREA");
    const label = screen.getByText("Value");
    expect((label as HTMLLabelElement).htmlFor).toBe(input.id);
  });
});

describe("TextField", () => {
  it("associates the label with a single-line reference-aware field", async () => {
    const { TextField } = await import("./FormField");
    render(
      <TextField
        label="Value"
        value=""
        onChange={vi.fn()}
        help="Type {{ to autocomplete."
        refContext={{ kind: "edge-with" } as never}
      />,
    );
    const input = screen.getByLabelText(/^Value/) as HTMLInputElement;
    expect(input.tagName).toBe("INPUT");
    const label = input.labels?.[0];
    expect(label, "the input carries a label element").toBeTruthy();
    expect(label?.htmlFor).toBe(input.id);
    expect(input.getAttribute("aria-describedby") ?? "").toMatch(/-help$/);
  });
});

describe("TagListField", () => {
  it("associates the FieldRow label with the tag input", () => {
    render(<TagListField label="Triggers" values={[]} onChange={vi.fn()} />);
    const input = screen.getByLabelText("Triggers");
    expect(input.tagName).toBe("INPUT");
    const label = screen.getByText("Triggers");
    expect((label as HTMLLabelElement).htmlFor).toBe(input.id);
  });
});
