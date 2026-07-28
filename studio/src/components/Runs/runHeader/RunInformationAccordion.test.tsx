// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import RunInformationAccordion from "./RunInformationAccordion";

afterEach(cleanup);

function disclosure(container: HTMLElement) {
  const details = container.querySelector("details");
  const summary = screen.getByText("Run information").closest("summary");
  if (!details || !summary) {
    throw new Error("Run information disclosure was not rendered");
  }
  return { details, summary };
}

describe("RunInformationAccordion", () => {
  it("is closed by default and expands from its summary", () => {
    const { container } = render(
      <RunInformationAccordion>
        <div>Children and notes</div>
      </RunInformationAccordion>,
    );
    const { details, summary } = disclosure(container);

    expect(details.open).toBe(false);

    fireEvent.click(summary);
    expect(details.open).toBe(true);
  });

  it("keeps its contents mounted across an open/close cycle", () => {
    const { container } = render(
      <RunInformationAccordion>
        <textarea aria-label="Add a run note" defaultValue="draft note" />
      </RunInformationAccordion>,
    );
    const { details, summary } = disclosure(container);
    const draft = screen.getByLabelText("Add a run note");

    fireEvent.click(summary);
    fireEvent.click(summary);

    expect(details.open).toBe(false);
    expect(screen.getByLabelText("Add a run note")).toBe(draft);
  });
});
