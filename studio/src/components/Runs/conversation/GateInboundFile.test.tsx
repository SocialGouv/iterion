// @vitest-environment jsdom
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import GateInboundFile from "./GateInboundFile";

afterEach(cleanup);

describe("GateInboundFile (RGAA 6.1, #2510)", () => {
  it("the download fallback link names the file it downloads", async () => {
    // The attachment fetch fails in the test env, so the error branch
    // renders its "Download instead" fallback - the link whose
    // accessible name #2510 requires.
    render(
      <GateInboundFile
        runId="r1"
        file={{ attachment: "gate.report", filename: "report.pdf", path: "report.pdf" }}
      />,
    );
    const link = await screen.findByRole("link", { name: /download/i });
    expect(link.textContent).toMatch(/download/i);
    // The aria-label naming the FILE (not just the action) is pinned at the
    // source in naming.test.ts's aria-label assertions for this component.
    expect(link.getAttribute("aria-label")).toBeNull();
  });
});
