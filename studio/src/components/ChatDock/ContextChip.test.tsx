// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { referenceForRoute } from "@/lib/chatDock/routeReference";

import ContextChip from "./ContextChip";

afterEach(cleanup);

const boardRef = referenceForRoute("/board");

describe("ContextChip", () => {
  it("names what the assistant is assumed to be looking at", () => {
    render(
      <ContextChip
        reference={boardRef}
        dismissed={false}
        onDismiss={vi.fn()}
        onRestore={vi.fn()}
      />,
    );
    expect(screen.getByText("Looking at")).toBeTruthy();
    expect(screen.getByText("Board")).toBeTruthy();
  });

  it("renders nothing when the route points at nothing", () => {
    const { container } = render(
      <ContextChip
        reference={null}
        dismissed={false}
        onDismiss={vi.fn()}
        onRestore={vi.fn()}
      />,
    );
    expect(container.firstChild).toBeNull();
  });

  it("is dismissible", () => {
    const onDismiss = vi.fn();
    render(
      <ContextChip
        reference={boardRef}
        dismissed={false}
        onDismiss={onDismiss}
        onRestore={vi.fn()}
      />,
    );
    fireEvent.click(
      screen.getByRole("button", { name: /stop using board as context/i }),
    );
    expect(onDismiss).toHaveBeenCalled();
  });

  // Dismissing must not be a one-way door — otherwise the only way back
  // is a page reload.
  it("offers a way back once dismissed", () => {
    const onRestore = vi.fn();
    render(
      <ContextChip
        reference={boardRef}
        dismissed
        onDismiss={vi.fn()}
        onRestore={onRestore}
      />,
    );
    expect(screen.queryByText("Looking at")).toBeNull();
    fireEvent.click(
      screen.getByRole("button", { name: /use this page as context/i }),
    );
    expect(onRestore).toHaveBeenCalled();
  });
});
