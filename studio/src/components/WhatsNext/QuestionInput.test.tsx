// @vitest-environment jsdom
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import QuestionInput from "./QuestionInput";

afterEach(cleanup);

describe("QuestionInput", () => {
  it("labels the Other choice and its free-text field", () => {
    render(
      <QuestionInput
        question={{
          id: "gate",
          label: "Proceed with the rollout?",
          kind: "radio",
          required: false,
          options: [
            { label: "Yes", value: "yes" },
            { label: "No", value: "no" },
          ],
          allow_other: true,
        }}
        value={""}
        onChange={vi.fn()}
      />,
    );
    const other = screen.getByLabelText("Other") as HTMLInputElement;
    expect(other.type).toBe("radio");
    const label = screen.getByText("Other");
    expect((label as HTMLLabelElement).htmlFor).toBe(other.id);
    expect(
      screen.getByRole("textbox", { name: "Other answer" }),
    ).toBeTruthy();
  });

  it("groups its radio and checkbox options under the question (RGAA 11.5/11.6)", () => {
    const question = {
      id: "gate",
      label: "Proceed with the rollout?",
      kind: "radio" as const,
      required: false,
      options: [
        { label: "Yes", value: "yes" },
        { label: "No", value: "no" },
      ],
    };
    render(<QuestionInput question={question} value={""} onChange={vi.fn()} />);
    const group = screen.getByRole("group", { name: "Proceed with the rollout?" });
    expect(group.querySelector("legend")?.textContent).toBe("Proceed with the rollout?");

    cleanup();
    render(
      <QuestionInput question={{ ...question, kind: "checkbox" }} value={[]} onChange={vi.fn()} />,
    );
    const checkboxGroup = screen.getByRole("group", { name: "Proceed with the rollout?" });
    expect(checkboxGroup.querySelector("legend")?.textContent).toBe("Proceed with the rollout?");
  });

  it("puts inputId on the select it renders (RGAA 11.1)", () => {
    render(
      <QuestionInput
        question={{
          id: "env",
          label: "Target environment",
          kind: "select",
          required: false,
          options: [
            { label: "Prod", value: "prod" },
            { label: "Staging", value: "staging" },
          ],
        }}
        value={""}
        onChange={vi.fn()}
        inputId="q-env"
      />,
    );
    expect((screen.getByRole("combobox") as HTMLSelectElement).id).toBe("q-env");
  });

  it("puts inputId on the free-text field it renders (RGAA 11.1)", () => {
    render(
      <QuestionInput
        question={{
          id: "notes",
          label: "Operator notes",
          kind: "free_text",
          required: false,
        }}
        value={""}
        onChange={vi.fn()}
        inputId="q-notes"
      />,
    );
    expect((screen.getByRole("textbox") as HTMLTextAreaElement).id).toBe("q-notes");
  });
});
