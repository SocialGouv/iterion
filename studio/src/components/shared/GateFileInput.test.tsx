// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { expectNoViolationsStrict } from "@/__tests__/a11y/axeHelpers";

import GateFileInput from "./GateFileInput";

// An upload that never settles: the in-flight state stays observable.
vi.mock("@/api/runs", () => ({ uploadAttachment: vi.fn(() => new Promise(() => {})) }));

afterEach(cleanup);

function mount(props: { disabled?: boolean } = {}) {
  const onChange = vi.fn();
  const view = render(
    <main>
      <GateFileInput
        label="soundtrack"
        value={null}
        onChange={onChange}
        disabled={props.disabled}
      />
    </main>,
  );
  // Addressed structurally, never by its name: how many elements carry
  // that name is one of the things under test. The zone is the box around
  // the label that owns the input.
  const input = view.container.querySelector('input[type="file"]') as HTMLInputElement;
  const zone = input.closest("label")!.parentElement as HTMLElement;
  return { ...view, onChange, input, zone };
}

const soundtrack = () => new File(["x"], "a.mp3", { type: "audio/mpeg" });

describe("GateFileInput", () => {
  it("names its file input after the label, once (RGAA 11.1)", () => {
    const { input } = mount();
    expect(input.getAttribute("aria-label")).toBe("Upload soundtrack");
    // The zone carried the same name while being a role="button" around
    // this input: a control inside a control, named twice (#2766).
    expect(screen.getAllByLabelText("Upload soundtrack")).toHaveLength(1);
    expect(input.labels).toHaveLength(1);
  });

  it("leaves the drop zone no role of its own — the label opens the picker", () => {
    const { zone, onChange } = mount();
    expect(zone.getAttribute("role")).toBeNull();
    expect(zone.hasAttribute("tabindex")).toBe(false);
    fireEvent.keyDown(zone, { key: "Enter" });
    fireEvent.keyDown(zone, { key: " " });
    fireEvent.click(zone);
    expect(onChange).not.toHaveBeenCalled();
  });

  it("marks a disabled dropzone for assistive tech and for its dimmed style", () => {
    // Same contract as Runs/AttachmentFieldInput: the dimming is
    // `data-[disabled]:opacity-60`, bound to the attribute, never a static
    // opacity (RGAA 3.2) nor a colour-only change (imperceptible state) —
    // and the refusal is the input's own, not an aria-disabled that says
    // the state while the keyboard still reaches the picker.
    const { zone, input } = mount({ disabled: true });
    expect(input.disabled).toBe(true);
    expect(zone.hasAttribute("data-disabled")).toBe(true);
    expect(zone.className).toMatch(/(^|\s)data-\[disabled\]:opacity-60(\s|$)/);
  });

  it("carries neither disabled attribute when enabled", () => {
    const { zone, input } = mount();
    expect(input.disabled).toBe(false);
    expect(zone.hasAttribute("data-disabled")).toBe(false);
  });

  it("keeps an in-flight upload busy, not disabled — the progress text keeps its contrast", async () => {
    // Busy blocks every path and says so (aria-busy); it is not the
    // disabled state: dimming the zone here would put the live
    // "Uploading…" text at 3.3:1 in the light theme (RGAA 3.2).
    const { zone, input } = mount();
    fireEvent.change(input, { target: { files: [soundtrack()] } });
    await waitFor(() => expect(zone.getAttribute("aria-busy")).toBe("true"));
    expect(zone.hasAttribute("data-disabled")).toBe(false);
    expect(zone.className).toMatch(/(^|\s)pointer-events-none(\s|$)/);
  });

  it("opens nothing while disabled — the picker is unreachable (RGAA 7.1)", () => {
    // The input is the only way in, so its own refusal is the whole
    // refusal: the zone is no tab stop to press Enter on, and the label
    // of a disabled control activates nothing.
    const { input } = mount({ disabled: true });
    expect(input.disabled).toBe(true);
    const clicks = vi.fn();
    input.addEventListener("click", clicks);
    fireEvent.click(input.closest("label")!);
    expect(clicks).not.toHaveBeenCalled();
  });

  it("opens nothing while busy — an in-flight upload never reopens the picker (RGAA 7.1)", async () => {
    const { zone, input } = mount();
    fireEvent.change(input, { target: { files: [soundtrack()] } });
    await waitFor(() => expect(zone.getAttribute("aria-busy")).toBe("true"));
    expect(input.disabled).toBe(true);
    const clicks = vi.fn();
    input.addEventListener("click", clicks);
    fireEvent.click(input.closest("label")!);
    expect(clicks).not.toHaveBeenCalled();
  });

  it("Gate · GateFileInput — axe smoke, incompletes included", async () => {
    const { container } = mount();
    await expectNoViolationsStrict(container, "GateFileInput");
  });
});
