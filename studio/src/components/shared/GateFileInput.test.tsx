// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import GateFileInput from "./GateFileInput";

// An upload that never settles: the in-flight state stays observable.
vi.mock("@/api/runs", () => ({ uploadAttachment: vi.fn(() => new Promise(() => {})) }));

afterEach(cleanup);

describe("GateFileInput", () => {
  it("names its file input after the label — the dropzone's name does not reach the control (RGAA 11.1)", () => {
    render(<GateFileInput label="soundtrack" value={null} onChange={vi.fn()} />);
    // The dropzone (role=button) carries the same name; assert on the
    // INPUT's own aria-label, as the AttachmentFieldInput test does.
    const input = screen.getByLabelText("Upload soundtrack", { selector: "input" }) as HTMLInputElement;
    expect(input.getAttribute("aria-label")).toBe("Upload soundtrack");
  });

  it("marks a disabled dropzone for assistive tech and for its dimmed style (aria-disabled + data-disabled)", () => {
    // Same contract as Runs/AttachmentFieldInput: both attributes follow
    // the `disabled` prop alone; the dimming is `data-[disabled]:opacity-60`,
    // bound to the attribute, never a static opacity (RGAA 3.2) nor a
    // colour-only change (imperceptible state).
    render(<GateFileInput label="soundtrack" value={null} onChange={vi.fn()} disabled />);
    const zone = screen.getByRole("button", { name: "Upload soundtrack" });
    expect(zone.getAttribute("aria-disabled")).toBe("true");
    expect(zone.hasAttribute("data-disabled")).toBe(true);
    expect(zone.className).toMatch(/(^|\s)data-\[disabled\]:opacity-60(\s|$)/);
  });

  it("carries neither disabled attribute when enabled", () => {
    render(<GateFileInput label="soundtrack" value={null} onChange={vi.fn()} />);
    const zone = screen.getByRole("button", { name: "Upload soundtrack" });
    expect(zone.hasAttribute("aria-disabled")).toBe(false);
    expect(zone.hasAttribute("data-disabled")).toBe(false);
  });

  it("keeps an in-flight upload busy, not disabled — the progress text keeps its contrast", async () => {
    // Busy blocks the pointer and says so (aria-busy); it is not the
    // disabled state: dimming the zone here would put the live
    // "Uploading…" text at 3.3:1 in the light theme (RGAA 3.2).
    render(<GateFileInput label="soundtrack" value={null} onChange={vi.fn()} />);
    const input = screen.getByLabelText("Upload soundtrack", { selector: "input" });
    fireEvent.change(input, { target: { files: [new File(["x"], "a.mp3", { type: "audio/mpeg" })] } });
    const zone = screen.getByRole("button", { name: "Upload soundtrack" });
    await waitFor(() => expect(zone.getAttribute("aria-busy")).toBe("true"));
    expect(zone.hasAttribute("aria-disabled")).toBe(false);
    expect(zone.hasAttribute("data-disabled")).toBe(false);
    expect(zone.className).toMatch(/(^|\s)pointer-events-none(\s|$)/);
  });

  it("answers no key while disabled — the picker never opens (RGAA 7.1)", () => {
    render(<GateFileInput label="soundtrack" value={null} onChange={vi.fn()} disabled />);
    const input = screen.getByLabelText("Upload soundtrack", { selector: "input" });
    const spy = vi.spyOn(input, "click");
    const zone = screen.getByRole("button", { name: "Upload soundtrack" });
    expect(zone.getAttribute("tabindex")).toBe("-1");
    fireEvent.keyDown(zone, { key: "Enter" });
    fireEvent.keyDown(zone, { key: " " });
    expect(spy).not.toHaveBeenCalled();
  });

  it("answers no key while busy — an in-flight upload never reopens the picker (RGAA 7.1)", async () => {
    render(<GateFileInput label="soundtrack" value={null} onChange={vi.fn()} />);
    const input = screen.getByLabelText("Upload soundtrack", { selector: "input" });
    const spy = vi.spyOn(input, "click");
    fireEvent.change(input, { target: { files: [new File(["x"], "a.mp3", { type: "audio/mpeg" })] } });
    const zone = screen.getByRole("button", { name: "Upload soundtrack" });
    await waitFor(() => expect(zone.getAttribute("aria-busy")).toBe("true"));
    fireEvent.keyDown(zone, { key: "Enter" });
    expect(spy).not.toHaveBeenCalled();
  });
});
