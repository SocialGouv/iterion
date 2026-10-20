// @vitest-environment jsdom
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import AttachmentFieldInput from "./AttachmentFieldInput";

afterEach(cleanup);

describe("AttachmentFieldInput", () => {
  it("names its file input after the field — a constant name would collide across fields (RGAA 11.1)", () => {
    render(
      <AttachmentFieldInput
        field={{ name: "logo", type: "image" }}
        value={null}
        onChange={vi.fn()}
        serverLimits={null}
      />,
    );
    // The dropzone (role=button) carries the same name; only the INPUT's own
    // aria-label names the file control itself, so assert on it directly —
    // getByLabelText alone would match the dropzone and pass either way.
    const input = screen.getByLabelText("Upload logo", {
      selector: "input",
    }) as HTMLInputElement;
    expect(input.getAttribute("aria-label")).toBe("Upload logo");
  });
});
