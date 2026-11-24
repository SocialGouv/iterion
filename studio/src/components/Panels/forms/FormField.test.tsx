// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { CommittedTextField, PromptPickerField, TagListField } from "./FormField";

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

describe("PromptPickerField", () => {
  it("gives each empty slot's pencil a distinct name from its own label", () => {
    render(
      <>
        <PromptPickerField
          label="System Prompt"
          value=""
          onChange={vi.fn()}
          options={[]}
          onCreate={vi.fn()}
          onEdit={vi.fn()}
          body=""
        />
        <PromptPickerField
          label="User Prompt"
          value=""
          onChange={vi.fn()}
          options={[]}
          onCreate={vi.fn()}
          onEdit={vi.fn()}
          body=""
        />
      </>,
    );
    expect(screen.getByRole("button", { name: "Edit system prompt" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Edit user prompt" })).toBeTruthy();
  });

  it("names the pencil and the collapsed preview from the selected prompt", () => {
    render(
      <PromptPickerField
        label="System Prompt"
        value="greet"
        onChange={vi.fn()}
        options={[{ value: "greet", label: "greet" }]}
        onCreate={vi.fn()}
        onEdit={vi.fn()}
        body={"You are polite.\nAnswer briefly.\nStay short.\nTail line."}
      />,
    );
    const pencil = screen.getByRole("button", { name: "Edit prompt greet" });
    expect(pencil.getAttribute("aria-label")).toBe("Edit prompt greet");

    // The preview button's name carries the visible preview text (label in
    // name) and the prompt name (unique per slot) — not the bare tooltip.
    const preview = screen.getByRole("button", { name: /You are polite/ });
    const previewName = preview.getAttribute("aria-label") ?? "";
    expect(previewName).not.toBe("Click to edit in large editor");
    expect(previewName).toContain("You are polite.");
    expect(previewName).toContain("greet");
  });
});

describe("CommittedTextField", () => {
  it("qualifies the confirm button with the field label when dirty", () => {
    render(<CommittedTextField label="Name" value="init" onChange={vi.fn()} />);
    const input = screen.getByLabelText("Name") as HTMLInputElement;
    fireEvent.change(input, { target: { value: "renamed" } });
    expect(screen.getByRole("button", { name: "Confirm Name edit" })).toBeTruthy();
  });
});
