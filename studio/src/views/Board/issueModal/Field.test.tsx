// @vitest-environment jsdom
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { TagInput } from "@/components/ui/TagInput";
import { Field } from "./Field";

afterEach(cleanup);

describe("Field", () => {
  it("names the control below it when given htmlFor (RGAA 11.1)", () => {
    render(
      <Field label="Labels" htmlFor="t-labels">
        <TagInput id="t-labels" value={[]} onChange={vi.fn()} />
      </Field>,
    );
    expect(
      (screen.getByLabelText("Labels") as HTMLInputElement).id,
    ).toBe("t-labels");
  });
});
