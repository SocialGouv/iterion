// @vitest-environment jsdom
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import AttachmentFieldInput from "./AttachmentFieldInput";

afterEach(cleanup);

describe("AttachmentFieldInput", () => {
  it("names its file input — the sr-only control has no visible label to lean on (RGAA 11.1)", () => {
    render(
      <AttachmentFieldInput
        field={{ name: "logo", type: "image" }}
        value={null}
        onChange={vi.fn()}
        serverLimits={null}
      />,
    );
    expect(screen.getByLabelText("Upload logo")).toBeTruthy();
  });
});
