// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";

import type { PipelineBoardCard } from "@/api/pipelineBoards";
import { setupMatchMedia } from "@/__tests__/a11y/axeHelpers";

// Same stubs as PipelineCardDetails.test: ProducedElements pulls react-query
// and SequentialReviews reaches into the run store.
vi.mock("./ProducedElements", () => ({
  ProducedElements: () => <div data-testid="produced" />,
}));
vi.mock("@/components/Runs/conversation/HumanPromptForm", () => ({
  default: () => <div data-testid="human-prompt" />,
}));
vi.mock("wouter", () => ({
  Link: ({
    href,
    children,
    ...rest
  }: React.AnchorHTMLAttributes<HTMLAnchorElement> & { href: string }) => (
    <a href={href} {...rest}>
      {children}
    </a>
  ),
}));

import PipelineCardDetails from "./PipelineCardDetails";
import {
  DRAWER_WIDTH_DEFAULT,
  DRAWER_WIDTH_KEY,
  DRAWER_WIDTH_MIN,
  DRAWER_WIDTH_STEP,
  DRAWER_WIDTH_STEP_LARGE,
} from "./cardDrawerWidth";

setupMatchMedia();

const VIEWPORT = 1920;

function makeCard(partial: Partial<PipelineBoardCard> = {}): PipelineBoardCard {
  return {
    id: "card",
    kind: "task",
    column_id: "opened",
    title: "Compose the intro",
    executed_nodes: 0,
    total_nodes: 0,
    tree_executed_nodes: 0,
    tree_total_nodes: 0,
    created_at: "2026-07-15T09:00:00Z",
    updated_at: "2026-07-15T10:00:00Z",
    entry_input: { topic: "jazz" },
    ...partial,
  };
}

function mount(overrides: { onClose?: () => void } = {}) {
  const onClose = overrides.onClose ?? vi.fn();
  const utils = render(
    <PipelineCardDetails card={makeCard()} onClose={onClose} onRefetch={() => {}} />,
  );
  return { ...utils, onClose };
}

const drawer = () => screen.getByRole("dialog");
const handle = () => screen.getByRole("separator", { name: "Resize details drawer" });
const widthOf = () => parseInt(drawer().style.width, 10);

beforeEach(() => {
  window.localStorage.clear();
  Object.defineProperty(window, "innerWidth", {
    value: VIEWPORT,
    configurable: true,
    writable: true,
  });
});

afterEach(cleanup);

describe("pipeline card drawer — resizable width", () => {
  it("opens at the historical 28rem width and exposes a splitter", () => {
    mount();
    expect(widthOf()).toBe(DRAWER_WIDTH_DEFAULT);
    const h = handle();
    expect(h.getAttribute("aria-orientation")).toBe("vertical");
    expect(h.getAttribute("tabindex")).toBe("0");
    expect(h.getAttribute("aria-valuenow")).toBe(String(DRAWER_WIDTH_DEFAULT));
    expect(h.getAttribute("aria-valuemin")).toBe(String(DRAWER_WIDTH_MIN));
    expect(h.getAttribute("aria-valuetext")).toBe(`${DRAWER_WIDTH_DEFAULT} pixels`);
  });

  it("widens on ArrowLeft and narrows on ArrowRight (the drawer is right-anchored)", () => {
    mount();
    fireEvent.keyDown(handle(), { key: "ArrowLeft" });
    expect(widthOf()).toBe(DRAWER_WIDTH_DEFAULT + DRAWER_WIDTH_STEP);
    fireEvent.keyDown(handle(), { key: "ArrowRight" });
    expect(widthOf()).toBe(DRAWER_WIDTH_DEFAULT);
  });

  it("takes a bigger step with Shift and jumps to the bounds with Home/End", () => {
    mount();
    fireEvent.keyDown(handle(), { key: "ArrowLeft", shiftKey: true });
    expect(widthOf()).toBe(DRAWER_WIDTH_DEFAULT + DRAWER_WIDTH_STEP_LARGE);
    fireEvent.keyDown(handle(), { key: "Home" });
    expect(widthOf()).toBe(DRAWER_WIDTH_MIN);
    fireEvent.keyDown(handle(), { key: "End" });
    // The ceiling is capped by the viewport, never past it.
    expect(widthOf()).toBeLessThanOrEqual(VIEWPORT);
    expect(widthOf()).toBeGreaterThan(DRAWER_WIDTH_DEFAULT);
  });

  it("leaves other keys alone so Escape still closes the drawer", () => {
    const { onClose } = mount();
    fireEvent.keyDown(handle(), { key: "a" });
    expect(widthOf()).toBe(DRAWER_WIDTH_DEFAULT);
    fireEvent.keyDown(window, { key: "Escape" });
    expect(onClose).toHaveBeenCalled();
  });

  it("resizes by dragging the handle, and only persists once the drag ends", () => {
    mount();
    fireEvent.pointerDown(handle(), { clientX: 1500 });
    fireEvent.pointerMove(window, { clientX: 1300 });
    // Dragging left widens by exactly the travelled distance.
    expect(widthOf()).toBe(DRAWER_WIDTH_DEFAULT + 200);
    // A drag emits hundreds of moves — nothing hits localStorage until pointerup.
    expect(window.localStorage.getItem(DRAWER_WIDTH_KEY)).toBeNull();

    fireEvent.pointerUp(window);
    expect(window.localStorage.getItem(DRAWER_WIDTH_KEY)).toBe(
      String(DRAWER_WIDTH_DEFAULT + 200),
    );
  });

  it("stops widening at the viewport instead of pushing its handle off-screen", () => {
    mount();
    fireEvent.pointerDown(handle(), { clientX: 1900 });
    fireEvent.pointerMove(window, { clientX: -5000 });
    fireEvent.pointerUp(window);
    expect(widthOf()).toBeLessThanOrEqual(VIEWPORT);
  });

  it("survives a reload — the persisted width is restored on the next mount", () => {
    mount();
    fireEvent.keyDown(handle(), { key: "ArrowLeft", shiftKey: true });
    const resized = widthOf();
    expect(window.localStorage.getItem(DRAWER_WIDTH_KEY)).toBe(String(resized));

    cleanup();
    mount();
    expect(widthOf()).toBe(resized);
  });

  it("double-clicking the handle resets to the default width", () => {
    mount();
    fireEvent.keyDown(handle(), { key: "End" });
    expect(widthOf()).not.toBe(DRAWER_WIDTH_DEFAULT);
    fireEvent.doubleClick(handle());
    expect(widthOf()).toBe(DRAWER_WIDTH_DEFAULT);
    expect(window.localStorage.getItem(DRAWER_WIDTH_KEY)).toBe(
      String(DRAWER_WIDTH_DEFAULT),
    );
  });

  it("re-clamps when the viewport shrinks below the persisted width", () => {
    mount();
    fireEvent.keyDown(handle(), { key: "End" });
    expect(widthOf()).toBeGreaterThan(800);

    Object.defineProperty(window, "innerWidth", { value: 800, configurable: true });
    fireEvent.resize(window);
    expect(widthOf()).toBe(800);
  });
});

describe("pipeline card drawer — portal chrome", () => {
  it("portals the scrim + panel to document.body, above the board", () => {
    const { container } = mount();
    // Nothing renders inline: the board tree is under overflow:hidden.
    expect(container.innerHTML).toBe("");
    expect(document.body.contains(drawer())).toBe(true);
    expect(drawer().getAttribute("aria-modal")).toBe("true");
    expect(document.body.style.overflow).toBe("hidden");
  });

  it("the scrim ignores the click that opened it, then closes on the next one", () => {
    vi.useFakeTimers();
    try {
      const onClose = vi.fn();
      render(
        <PipelineCardDetails card={makeCard()} onClose={onClose} onRefetch={() => {}} />,
      );
      const scrim = document.querySelector('[role="presentation"]');
      expect(scrim).toBeTruthy();
      fireEvent.click(scrim!);
      expect(onClose).not.toHaveBeenCalled();

      act(() => {
        vi.runAllTimers();
      });
      fireEvent.click(scrim!);
      expect(onClose).toHaveBeenCalledTimes(1);
    } finally {
      vi.useRealTimers();
    }
  });
});
