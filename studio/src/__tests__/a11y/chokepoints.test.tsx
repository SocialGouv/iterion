// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { useState } from "react";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { CrossCircledIcon } from "@radix-ui/react-icons";

import { clickableRowProps, pointerRowProps } from "@/lib/a11y";
import { Icon } from "@/components/ui/Icon";
import { LiveDot } from "@/components/ui/LiveDot";
import { ResizeSeparator } from "@/components/ui/ResizeSeparator";
import { VideoPreview } from "@/components/ui/VideoPreview";
import { CommittedTextField } from "@/components/Panels/forms/FormField";
import { StatusMessage } from "@/components/ui/StatusMessage";
import { setupMatchMedia, expectNoViolationsStrict } from "./axeHelpers";

// The a11y-chokepoints primitives (#2501 #2502 #2504 #2507 #2512
// #2520): the contract each shared primitive owns, tested at the
// class level so the per-call-site fixes cannot drift back.

setupMatchMedia();

describe("a11y / chokepoints", () => {
  afterEach(() => {
    cleanup();
  });

  it("Icon — a label pairs role=img and aria-label on the SAME svg (RGAA 1.1)", () => {
    const { container } = render(<Icon icon={CrossCircledIcon} label="Failed" />);
    const svg = container.querySelector("svg");
    expect(svg?.getAttribute("role")).toBe("img");
    expect(svg?.getAttribute("aria-label")).toBe("Failed");
  });

  it("Icon — no label renders the glyph decorative (RGAA 1.2)", () => {
    const { container } = render(<Icon icon={CrossCircledIcon} />);
    const svg = container.querySelector("svg");
    expect(svg?.getAttribute("aria-hidden")).toBe("true");
    expect(svg?.getAttribute("role")).toBeNull();
    expect(svg?.getAttribute("aria-label")).toBeNull();
  });

  it("LiveDot — the label is sr-only TEXT content, not an aria-label (RGAA 10.2)", () => {
    const { container } = render(<LiveDot tone="live" label="Run is active" />);
    const dot = container.firstElementChild as HTMLElement;
    // The old shape — aria-label on an empty span — paints no pixels
    // and vanishes with the stylesheets. The text child survives both.
    expect(dot.getAttribute("aria-label")).toBeNull();
    expect(dot.getAttribute("role")).toBe("status");
    expect(dot.querySelector(".sr-only")?.textContent).toBe("Run is active");
  });

  it("LiveDot — unlabelled stays aria-hidden", () => {
    const { container } = render(<LiveDot tone="info" />);
    expect((container.firstElementChild as HTMLElement).getAttribute("aria-hidden")).toBe(
      "true",
    );
  });

  it("StatusMessage injects text into a permanent role=status container (RGAA 7.5)", () => {
    const { rerender } = render(<StatusMessage message="" />);
    expect(screen.getByRole("status").textContent).toBe("");
    rerender(<StatusMessage message="Draft saved" />);
    expect(screen.getByRole("status").textContent).toBe("Draft saved");
  });

  it("required fields carry aria-required and a visible mention (RGAA 11.10)", () => {
    render(
      <CommittedTextField label="Workflow Name" required value="svc" onChange={() => {}} />,
    );
    const input = screen.getByLabelText(/Workflow Name/) as HTMLInputElement;
    expect(input.getAttribute("aria-required")).toBe("true");
    expect(screen.getByText("*")).toBeTruthy();
    expect(screen.getByText("(required)")).toBeTruthy();

    cleanup();
    render(
      <CommittedTextField label="Optional note" value="" onChange={() => {}} />,
    );
    expect(screen.getByLabelText(/Optional note/).getAttribute("aria-required")).toBeNull();
    expect(screen.queryByText("*")).toBeNull();
  });

  it("ResizeSeparator — full splitter contract on one axis", () => {
    // The primitive is stateless: the caller owns the value and feeds it
    // back through onResize. The harness mirrors that contract — a real
    // panel re-renders with the committed size after every resize — so
    // the assertions read the live aria-valuenow, not a frozen mock.
    function Harness({ onReset }: { onReset: () => void }) {
      const [value, setValue] = useState(300);
      return (
        <ResizeSeparator
          label="Resize panel"
          axes={{ x: { value, min: 220, max: 560, grow: "end", onResize: setValue } }}
          onReset={onReset}
        />
      );
    }
    const onReset = vi.fn();
    const { container } = render(<Harness onReset={onReset} />);
    const grip = () => container.firstElementChild as HTMLElement;
    expect(grip().getAttribute("role")).toBe("separator");
    expect(grip().getAttribute("tabindex")).toBe("0");
    expect(grip().getAttribute("aria-orientation")).toBe("vertical");
    expect(grip().getAttribute("aria-valuenow")).toBe("300");
    expect(grip().getAttribute("aria-valuemin")).toBe("220");
    expect(grip().getAttribute("aria-valuemax")).toBe("560");
    expect(grip().getAttribute("aria-valuetext")).toBe("300 pixels wide");

    // grow: "end" — ArrowRight enlarges, ArrowLeft shrinks, Shift is a
    // big step, Home/End jump to the bounds, and the primitive clamps.
    fireEvent.keyDown(grip(), { key: "ArrowRight" });
    expect(grip().getAttribute("aria-valuenow")).toBe("316");
    fireEvent.keyDown(grip(), { key: "ArrowLeft" });
    expect(grip().getAttribute("aria-valuenow")).toBe("300");
    fireEvent.keyDown(grip(), { key: "ArrowRight", shiftKey: true });
    expect(grip().getAttribute("aria-valuenow")).toBe("364");
    fireEvent.keyDown(grip(), { key: "Home" });
    expect(grip().getAttribute("aria-valuenow")).toBe("220");
    fireEvent.keyDown(grip(), { key: "End" });
    expect(grip().getAttribute("aria-valuenow")).toBe("560");
    fireEvent.keyDown(grip(), { key: "ArrowRight", shiftKey: true });
    expect(grip().getAttribute("aria-valuenow")).toBe("560");
    fireEvent.doubleClick(grip());
    expect(onReset).toHaveBeenCalledTimes(1);
  });

  it("ResizeSeparator — grow:start inverts the arrows (right-anchored panel)", () => {
    const onResize = vi.fn();
    const { container } = render(
      <ResizeSeparator
        label="Resize drawer"
        axes={{ x: { value: 400, min: 320, max: 700, grow: "start", onResize } }}
      />,
    );
    const grip = container.firstElementChild as HTMLElement;
    fireEvent.keyDown(grip, { key: "ArrowLeft" });
    expect(onResize).toHaveBeenLastCalledWith(416);
    fireEvent.keyDown(grip, { key: "ArrowRight" });
    expect(onResize).toHaveBeenLastCalledWith(384);
  });

  it("ResizeSeparator — a corner grip exposes both axes and skips Home/End", () => {
    const onWidth = vi.fn();
    const onHeight = vi.fn();
    const { container } = render(
      <ResizeSeparator
        label="Resize the assistant panel"
        axes={{
          x: { value: 420, min: 320, max: 1280, grow: "start", onResize: onWidth },
          y: { value: 520, min: 280, max: 800, grow: "start", onResize: onHeight },
        }}
      />,
    );
    const grip = container.firstElementChild as HTMLElement;
    expect(grip.getAttribute("aria-valuetext")).toBe(
      "420 pixels wide × 520 pixels high",
    );
    fireEvent.keyDown(grip, { key: "ArrowUp" });
    expect(onHeight).toHaveBeenLastCalledWith(536);
    fireEvent.keyDown(grip, { key: "ArrowLeft" });
    expect(onWidth).toHaveBeenLastCalledWith(436);
    // Home/End are ambiguous on two axes — neither axis moves.
    fireEvent.keyDown(grip, { key: "Home" });
    expect(onWidth).toHaveBeenCalledTimes(1);
    expect(onHeight).toHaveBeenCalledTimes(1);
  });

  it("VideoPreview — a captions URL becomes a <track>; none states so visibly (RGAA 4.3)", () => {
    const withTrack = render(
      <VideoPreview src="blob:video" captionsHref="/cap.vtt" label="demo.mp4" />,
    );
    const track = withTrack.container.querySelector("track");
    expect(track?.getAttribute("src")).toBe("/cap.vtt");
    expect(track?.getAttribute("kind")).toBe("captions");
    expect(withTrack.container.textContent).not.toContain("No caption track");

    const without = render(<VideoPreview src="blob:video" label="demo.mp4" />);
    expect(without.container.querySelector("track")).toBeNull();
    expect(without.container.textContent).toContain(
      "No caption track was provided",
    );
  });

  it("the chokepoint shapes stay axe-clean", async () => {
    const onResize = vi.fn();
    const root = render(
      <main>
        <Icon icon={CrossCircledIcon} label="Failed" />
        <LiveDot tone="live" label="Run is active" />
        <ResizeSeparator
          label="Resize panel"
          axes={{ x: { value: 300, min: 220, max: 560, grow: "end", onResize } }}
        />
      </main>,
    );
    await expectNoViolationsStrict(root.container, "chokepoints", [
      {
        rule: "focus-order-semantics",
        node: "role=separator",
        reason:
          "the window-splitter pattern: role=separator with aria-valuenow IS a widget (ARIA 1.2) and axe's role list predates it — the keyboard contract is pinned by 'ResizeSeparator — full splitter contract on one axis' in this file",
      },
    ]);
  });
});

// The two row shapes lib/a11y offers (#2766). A clickable row is either the
// control itself — and then it holds nothing focusable — or a pointer
// convenience whose keyboard path is a real control inside it. The pair
// exists so the third shape, a role="button" row holding a button, has no
// call site left to be written at.
describe("a11y / row activation shapes", () => {
  afterEach(() => {
    cleanup();
  });

  it("clickableRowProps — a row with nothing focusable inside IS the control", () => {
    const onActivate = vi.fn();
    const { container } = render(
      <div {...clickableRowProps(onActivate)}>
        <span>only text here</span>
      </div>,
    );
    const row = container.firstElementChild as HTMLElement;
    expect(row.getAttribute("role")).toBe("button");
    expect(row.tabIndex).toBe(0);
    fireEvent.keyDown(row, { key: "Enter" });
    fireEvent.keyDown(row, { key: " " });
    expect(onActivate).toHaveBeenCalledTimes(2);
    // A key has no cursor to anchor anything at, and the activation says so
    // rather than reporting (0, 0) — which is a place on the screen.
    expect(onActivate).toHaveBeenLastCalledWith({ pointer: null });
    fireEvent.click(row, { clientX: 12, clientY: 34 });
    expect(onActivate).toHaveBeenLastCalledWith({ pointer: { x: 12, y: 34 } });
  });

  it("pointerRowProps — the row is no control: no role, no tab stop, no key path", () => {
    const onActivate = vi.fn();
    const { container } = render(
      <div {...pointerRowProps(onActivate)}>
        <button type="button">the keyboard path</button>
      </div>,
    );
    const row = container.firstElementChild as HTMLElement;
    expect(row.getAttribute("role")).toBeNull();
    expect(row.hasAttribute("tabindex")).toBe(false);
    fireEvent.keyDown(row, { key: "Enter" });
    fireEvent.keyDown(row, { key: " " });
    expect(onActivate).not.toHaveBeenCalled();
    fireEvent.click(row, { clientX: 7, clientY: 9 });
    expect(onActivate).toHaveBeenCalledWith({ pointer: { x: 7, y: 9 } });
  });

  it("both shapes refuse every path while disabled", () => {
    const widgetRow = vi.fn();
    const pointerRow = vi.fn();
    render(
      <div>
        <div data-testid="widget" {...clickableRowProps(widgetRow, undefined, { disabled: true })} />
        <div data-testid="pointer" {...pointerRowProps(pointerRow, { disabled: true })} />
      </div>,
    );
    const widget = screen.getByTestId("widget");
    expect(widget.tabIndex).toBe(-1);
    fireEvent.click(widget);
    fireEvent.keyDown(widget, { key: "Enter" });
    expect(widgetRow).not.toHaveBeenCalled();
    fireEvent.click(screen.getByTestId("pointer"));
    expect(pointerRow).not.toHaveBeenCalled();
  });

  it("the shape they replace is what axe calls nested-interactive", async () => {
    // The positive control for the pair: the net still sees the defect the
    // surfaces were cleaned of, so a green suite means the rows changed —
    // not that the net stopped looking for this.
    const { container } = render(
      <main>
        <div {...clickableRowProps(() => {})}>
          <button type="button">Resume</button>
        </div>
      </main>,
    );
    await expect(expectNoViolationsStrict(container, "a row holding a button")).rejects.toThrow(
      /nested-interactive/,
    );
  });

  it("a pointer row with its inner control is axe-clean", async () => {
    const { container } = render(
      <main>
        <div {...pointerRowProps(() => {})}>
          <button type="button">Open the thing</button>
        </div>
      </main>,
    );
    await expectNoViolationsStrict(container, "pointer row");
  });
});
