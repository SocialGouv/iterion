// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { expectNoViolationsStrict } from "@/__tests__/a11y/axeHelpers";

import AttachmentFieldInput from "./AttachmentFieldInput";

afterEach(cleanup);

function mount(props: { disabled?: boolean; error?: string } = {}) {
  const onChange = vi.fn();
  const view = render(
    <main>
      <AttachmentFieldInput
        field={{ name: "logo", type: "image" }}
        value={props.error ? { file: new File(["x"], "big.png"), error: props.error } : null}
        onChange={onChange}
        serverLimits={null}
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

describe("AttachmentFieldInput", () => {
  it("names its file input after the field — a constant name would collide across fields (RGAA 11.1)", () => {
    const { input } = mount();
    expect(input.getAttribute("aria-label")).toBe("Upload logo");
    expect(input.tagName).toBe("INPUT");
    // ONE control carries the name. The zone used to carry it too, as a
    // role="button" wrapping this input: a control inside a control, named
    // twice (#2766).
    expect(screen.getAllByLabelText("Upload logo")).toHaveLength(1);
    expect(input.labels).toHaveLength(1);
  });

  it("leaves the drop zone no role of its own — the label opens the picker", () => {
    const { zone, input, onChange } = mount();
    expect(zone.getAttribute("role")).toBeNull();
    expect(zone.hasAttribute("tabindex")).toBe(false);
    fireEvent.keyDown(zone, { key: "Enter" });
    fireEvent.click(zone);
    expect(onChange).not.toHaveBeenCalled();
    // The one activation path, and it needs no JS: the label's own.
    const clicks = vi.fn();
    input.addEventListener("click", clicks);
    fireEvent.click(input.closest("label")!);
    expect(clicks).toHaveBeenCalledTimes(1);
  });

  it("marks a disabled dropzone for assistive tech and for its dimmed style", () => {
    // The dimming is `data-[disabled]:opacity-60`, bound to the attribute:
    // a static opacity-60 would dim the text's contrast on an enabled zone
    // too (RGAA 3.2), and a colour-only change left the state imperceptible.
    // The refusal itself is the input's — `aria-disabled` on the zone said
    // the state without holding it, and the keyboard reached the picker
    // anyway.
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

  it("hands a refused file's state to the control that holds it", () => {
    const { input } = mount({ error: "too large" });
    expect(input.getAttribute("aria-invalid")).toBe("true");
    expect(screen.getByRole("alert").textContent).toContain("too large");
  });

  it("Runs · AttachmentFieldInput — axe smoke, incompletes included", async () => {
    const { container } = mount();
    await expectNoViolationsStrict(container, "AttachmentFieldInput");
  });
});
