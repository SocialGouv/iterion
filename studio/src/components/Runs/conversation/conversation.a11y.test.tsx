// @vitest-environment jsdom
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { expectNoViolationsStrict } from "@/__tests__/a11y/axeHelpers";

import GateInboundPayload from "./GateInboundPayload";

afterEach(cleanup);

// A gate's inbound payload renders one block per field, each with its own
// Copy JSON — the icon-only kind, whose name is all a screen reader gets. The
// name carries the field (#2591's surface triage); the icon is unchanged.
function mountPayload() {
  return render(
    <main>
      <GateInboundPayload
        runId="run-1"
        questions={{
          plan: { steps: ["migrate", "verify"] },
          budget: { tokens: 1200 },
        }}
      />
    </main>,
  );
}

describe("gate inbound payload naming", () => {
  it("names each field's Copy JSON after the field it copies", () => {
    mountPayload();
    expect(
      screen.getByRole("button", { name: "Copy JSON of plan" }),
    ).toBeTruthy();
    expect(
      screen.getByRole("button", { name: "Copy JSON of budget" }),
    ).toBeTruthy();
  });

  it("leaves no two controls sharing an accessible name", () => {
    mountPayload();
    // Matched by the real accessible-name algorithm, one button per name,
    // and no other button on the surface.
    const expected = ["Copy JSON of plan", "Copy JSON of budget"];
    for (const name of expected) {
      expect(screen.getAllByRole("button", { name })).toHaveLength(1);
    }
    expect(screen.getAllByRole("button")).toHaveLength(expected.length);
  });
});

describe("gate inbound payload axe", () => {
  it("Runs · conversation gate payload — axe smoke, incompletes included", async () => {
    const { container } = mountPayload();
    await expectNoViolationsStrict(container, "GateInboundPayload");
  });
});
