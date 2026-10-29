// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { expectNoViolationsStrict } from "@/__tests__/a11y/axeHelpers";

import { FileDropZone } from "./FileDropZone";

afterEach(cleanup);

const file = (name = "a.mp3") => new File(["x"], name, { type: "audio/mpeg" });

function mount(props: Partial<Parameters<typeof FileDropZone>[0]> = {}) {
  const onPick = vi.fn();
  const view = render(
    <main>
      <FileDropZone name="Upload soundtrack" accept="audio/*" onPick={onPick} {...props}>
        <p>Drop a file or click to browse</p>
      </FileDropZone>
    </main>,
  );
  // Addressed structurally, never by its name: the number of elements
  // carrying that name is one of the things under test.
  const input = view.container.querySelector('input[type="file"]') as HTMLInputElement;
  const zone = input.closest("label")!.parentElement as HTMLElement;
  return { ...view, onPick, input, zone };
}

// The shape the zone used to have (#2766): role="button" + tabindex around
// a focusable sr-only file input — a control inside a control, announced
// twice under one name, with two activation paths to one picker.
describe("FileDropZone", () => {
  it("announces ONE named control, and it is the input", () => {
    const { input } = mount();
    // Two elements carrying `Upload soundtrack` is the defect: the zone
    // claimed the name the input already had.
    expect(screen.getAllByLabelText("Upload soundtrack")).toHaveLength(1);
    expect(input.tagName).toBe("INPUT");
    expect(input.type).toBe("file");
    // The enclosing <label> is what activates it — no role, no tabindex and
    // no key handler needed, because the input has all three natively.
    expect(input.labels).toHaveLength(1);
    expect(input.closest("label")).not.toBeNull();
  });

  it("the label activates the input — the whole activation path, no JS", () => {
    const { input } = mount();
    const clicks = vi.fn();
    input.addEventListener("click", clicks);
    fireEvent.click(input.closest("label")!);
    expect(clicks).toHaveBeenCalledTimes(1);
  });

  it("an inert zone's label activates nothing — the platform refuses it", () => {
    for (const state of [{ disabled: true }, { busy: true }]) {
      cleanup();
      const { input } = mount(state);
      const clicks = vi.fn();
      input.addEventListener("click", clicks);
      fireEvent.click(input.closest("label")!);
      expect(clicks, JSON.stringify(state)).not.toHaveBeenCalled();
    }
  });

  it("leaves the zone itself no control at all", () => {
    const { zone, onPick } = mount();
    expect(zone.getAttribute("role")).toBeNull();
    expect(zone.hasAttribute("tabindex")).toBe(false);
    expect(zone.getAttribute("aria-label")).toBeNull();
    fireEvent.keyDown(zone, { key: "Enter" });
    fireEvent.keyDown(zone, { key: " " });
    fireEvent.click(zone);
    expect(onPick).not.toHaveBeenCalled();
  });

  it("shows the sr-only input's focus on the box, which has the pixels", () => {
    // A control no pixel shows still has to show its focus (RGAA 10.7);
    // the global :focus-visible outline lands on a clipped element.
    const { zone } = mount();
    expect(zone.className).toMatch(/(^|\s)focus-within:ring-1(\s|$)/);
    expect(zone.className).toMatch(/(^|\s)focus-within:ring-accent(\s|$)/);
  });

  it("hands over the picked file and clears the input, so the same file re-picks", () => {
    const { input, onPick } = mount();
    const picked = file();
    fireEvent.change(input, { target: { files: [picked] } });
    expect(onPick).toHaveBeenCalledWith(picked);
    expect(input.value).toBe("");
  });

  it("takes a dropped file", () => {
    const { zone, onPick } = mount();
    const dropped = file("dragged.mp3");
    fireEvent.dragOver(zone, { dataTransfer: { files: [dropped] } });
    fireEvent.drop(zone, { dataTransfer: { files: [dropped] } });
    expect(onPick).toHaveBeenCalledWith(dropped);
  });

  it("disabled is inert AND dimmed — both paths refuse, the attribute dims", () => {
    const { zone, input, onPick } = mount({ disabled: true });
    // The input itself refuses: a zone kept inert by pointer-events-none
    // alone stays operable by keyboard.
    expect(input.disabled).toBe(true);
    expect(zone.hasAttribute("data-disabled")).toBe(true);
    expect(zone.className).toMatch(/(^|\s)data-\[disabled\]:opacity-60(\s|$)/);
    expect(zone.className).toMatch(/(^|\s)pointer-events-none(\s|$)/);
    fireEvent.drop(zone, { dataTransfer: { files: [file()] } });
    expect(onPick).not.toHaveBeenCalled();
  });

  it("busy is inert but NOT dimmed — the in-flight text keeps its contrast", () => {
    const { zone, input, onPick } = mount({ busy: true });
    expect(input.disabled).toBe(true);
    expect(zone.getAttribute("aria-busy")).toBe("true");
    expect(zone.hasAttribute("data-disabled")).toBe(false);
    expect(zone.className).toMatch(/(^|\s)pointer-events-none(\s|$)/);
    fireEvent.drop(zone, { dataTransfer: { files: [file()] } });
    expect(onPick).not.toHaveBeenCalled();
  });

  it("carries the refusal on the control that holds the value", () => {
    const { zone, input } = mount({ invalid: true });
    expect(input.getAttribute("aria-invalid")).toBe("true");
    expect(zone.className).toMatch(/(^|\s)border-danger(\s|$)/);
  });

  it("neither state attribute shows up when idle", () => {
    const { zone, input } = mount();
    expect(input.disabled).toBe(false);
    expect(zone.hasAttribute("aria-busy")).toBe(false);
    expect(zone.hasAttribute("data-disabled")).toBe(false);
    expect(zone.hasAttribute("aria-invalid")).toBe(false);
  });

  it("FileDropZone — axe smoke, incompletes included", async () => {
    const { container } = mount({ invalid: true });
    await expectNoViolationsStrict(container, "FileDropZone");
  });
});
