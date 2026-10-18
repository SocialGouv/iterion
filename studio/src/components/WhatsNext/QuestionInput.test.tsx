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
});
