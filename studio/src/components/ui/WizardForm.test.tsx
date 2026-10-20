// @vitest-environment jsdom
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { WizardForm } from "./WizardForm";
import type { FormSpec } from "@/lib/whats-next/questionForm";

const SPEC: FormSpec = {
  questions: [
    {
      id: "env",
      label: "Target environment",
      kind: "select",
      required: false,
      options: [
        { label: "Prod", value: "prod" },
        { label: "Staging", value: "staging" },
      ],
    },
    {
      id: "notes",
      label: "Operator notes",
      kind: "free_text",
      required: false,
    },
  ],
};

afterEach(cleanup);

describe("WizardForm", () => {
  it("associates the current step's visible label with its control (RGAA 11.1)", () => {
    render(<WizardForm spec={SPEC} onSubmit={vi.fn()} />);
    const select = screen.getByLabelText("Target environment") as HTMLSelectElement;
    expect(select.id.endsWith("-env")).toBe(true);
  });

  it("associates every visible label with its control in flat mode (RGAA 11.1)", () => {
    render(<WizardForm spec={SPEC} onSubmit={vi.fn()} mode="flat" />);
    const select = screen.getByLabelText("Target environment") as HTMLSelectElement;
    expect(select.id.endsWith("-env")).toBe(true);
    const notes = screen.getByLabelText("Operator notes") as HTMLTextAreaElement;
    expect(notes.id.endsWith("-notes")).toBe(true);
  });
});
