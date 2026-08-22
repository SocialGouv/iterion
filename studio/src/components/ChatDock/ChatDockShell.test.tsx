// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { ChatDockShell, type ChatDockShellProps } from "./ChatDockShell";
import { REFERENCE_MIME } from "@/lib/chatDock/dragReference";

afterEach(cleanup);

function renderShell(props: Partial<ChatDockShellProps> = {}) {
  const onDockChange = vi.fn();
  render(
    <ChatDockShell
      dock="closed"
      onDockChange={onDockChange}
      title="Assistant"
      dockedRightMode="self"
      {...props}
    >
      <div>body</div>
    </ChatDockShell>,
  );
  return { onDockChange };
}

describe("ChatDockShell dock states", () => {
  it("renders a bubble when closed and no body", () => {
    renderShell({ dock: "closed" });
    expect(screen.getByRole("button", { name: /open assistant/i })).toBeTruthy();
    expect(screen.queryByText("body")).toBeNull();
  });

  it("counts unread on the bubble's accessible name", () => {
    renderShell({ dock: "closed", unread: 3 });
    expect(screen.getByRole("button", { name: /open assistant \(3 new\)/i })).toBeTruthy();
  });

  it("springs open for a reference drag while closed", () => {
    const { onDockChange } = renderShell({
      dock: "closed",
      openOnReferenceDrag: true,
    });
    fireEvent.dragEnter(screen.getByRole("button", { name: /open assistant/i }), {
      dataTransfer: { types: [REFERENCE_MIME] },
    });
    expect(onDockChange).toHaveBeenCalledWith("floating");
  });

  it("renders a non-modal labelled dialog when floating", () => {
    renderShell({ dock: "floating" });
    const dialog = screen.getByRole("dialog", { name: "Assistant" });
    expect(dialog.getAttribute("aria-modal")).toBeNull();
    expect(screen.getByText("body")).toBeTruthy();
  });

  it("closes the floating panel on Escape", () => {
    const { onDockChange } = renderShell({ dock: "floating" });
    fireEvent.keyDown(screen.getByRole("dialog", { name: "Assistant" }), {
      key: "Escape",
    });
    expect(onDockChange).toHaveBeenCalledWith("closed");
  });

  it("renders the docked column itself in self mode", () => {
    renderShell({ dock: "docked-right", dockedRightMode: "self" });
    expect(screen.getByText("body")).toBeTruthy();
    expect(screen.getByText("Assistant")).toBeTruthy();
  });

  // The run console lays its own docked column out via ChatDockPanel, so
  // the shell must stay silent or the panel renders twice.
  it("renders nothing when docked-right is the host's job", () => {
    renderShell({ dock: "docked-right", dockedRightMode: "host" });
    expect(screen.queryByText("body")).toBeNull();
  });
});

// A `fixed` corner surface gets nothing from AppShell's padding
// reservation, so it has to step out of a reserved right edge itself —
// otherwise the run console's steering bubble sits under the assistant's
// docked column and takes no clicks.
describe("ChatDockShell right-edge reservation", () => {
  const rightOf = (el: Element) => (el as HTMLElement).style.right;

  it("leaves lanes alone when nothing is reserved", () => {
    renderShell({ dock: "closed", lane: 1 });
    expect(rightOf(screen.getByRole("button", { name: /open assistant/i }))).toBe(
      "80px",
    );
  });

  it("pushes a bubble out of a reserved band", () => {
    renderShell({ dock: "closed", lane: 1, rightInset: 380 });
    expect(rightOf(screen.getByRole("button", { name: /open assistant/i }))).toBe(
      "396px",
    );
  });

  // 448px already clears a 380px column — a reservation must not shove a
  // surface that never overlapped it.
  it("does not move a lane that already clears", () => {
    renderShell({ dock: "floating", lane: 1, rightInset: 380 });
    expect(rightOf(screen.getByRole("dialog", { name: "Assistant" }))).toBe("448px");
  });
});

describe("ChatDockShell chrome", () => {
  it("offers dock-right while floating and undock while docked", () => {
    const { onDockChange } = renderShell({ dock: "floating" });
    fireEvent.click(screen.getByRole("button", { name: /dock assistant to right side/i }));
    expect(onDockChange).toHaveBeenCalledWith("docked-right");

    cleanup();
    const docked = renderShell({ dock: "docked-right", dockedRightMode: "self" });
    fireEvent.click(screen.getByRole("button", { name: /undock to floating panel/i }));
    expect(docked.onDockChange).toHaveBeenCalledWith("floating");
  });

  it("minimises back to closed", () => {
    const { onDockChange } = renderShell({ dock: "floating" });
    fireEvent.click(screen.getByRole("button", { name: /minimise assistant/i }));
    expect(onDockChange).toHaveBeenCalledWith("closed");
  });
});
