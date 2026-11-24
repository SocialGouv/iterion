// @vitest-environment jsdom
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
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

describe("WizardForm required mention", () => {
  it("every decorative required asterisk is paired with an sr-only mention", () => {
    // The asterisk is aria-hidden: on its own it says nothing to assistive
    // tech. Each one must sit beside an sr-only "(required)" so the label's
    // accessible name carries the mention wherever that label renders.
    // (The flat chat-style render labels no question at all — a separate
    // gap; this pins the markers wherever labels DO render.)
    const src = readFileSync(resolve(process.cwd(), "src/components/ui/WizardForm.tsx"), "utf8");
    const asterisks = (src.match(/aria-hidden="true">\s*\*\s*<\/span>/g) ?? []).length;
    const mentions = (src.match(/sr-only">\(required\)<\/span>/g) ?? []).length;
    expect(asterisks).toBeGreaterThan(0);
    expect(mentions, "each required asterisk needs its sr-only (required) mention beside it").toBe(
      asterisks,
    );
  });
});
