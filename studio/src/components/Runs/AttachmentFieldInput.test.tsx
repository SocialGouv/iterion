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

  it("marks a disabled dropzone for assistive tech and for its dimmed style (aria-disabled + data-disabled)", () => {
    // The dimming is `data-[disabled]:opacity-60`, bound to the attribute:
    // a static opacity-60 would dim the text's contrast on an enabled zone
    // too (RGAA 3.2), and a colour-only change left the state imperceptible.
    render(
      <AttachmentFieldInput
        field={{ name: "logo", type: "image" }}
        value={null}
        onChange={vi.fn()}
        serverLimits={null}
        disabled
      />,
    );
    const zone = screen.getByRole("button", { name: "Drop an image or click to browse Upload logo" });
    expect(zone.getAttribute("aria-disabled")).toBe("true");
    expect(zone.hasAttribute("data-disabled")).toBe(true);
    expect(zone.className).toMatch(/(^|\s)data-\[disabled\]:opacity-60(\s|$)/);
  });

  it("carries neither disabled attribute when enabled", () => {
    render(
      <AttachmentFieldInput field={{ name: "logo", type: "image" }} value={null} onChange={vi.fn()} serverLimits={null} />,
    );
    const zone = screen.getByRole("button", { name: "Drop an image or click to browse Upload logo" });
    expect(zone.hasAttribute("aria-disabled")).toBe(false);
    expect(zone.hasAttribute("data-disabled")).toBe(false);
  });
});
