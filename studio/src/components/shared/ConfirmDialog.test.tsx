// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";

import ConfirmDialog from "./ConfirmDialog";

afterEach(cleanup);

function setup(extra?: Record<string, unknown>) {
  const onConfirm = vi.fn();
  const onCancel = vi.fn();
  render(
    <ConfirmDialog
      open
      title="Delete node?"
      message="This removes the node from the graph."
      confirmLabel="Delete"
      confirmVariant="danger"
      onConfirm={onConfirm}
      onCancel={onCancel}
      {...extra}
    />,
  );
  return {
    onConfirm,
    onCancel,
    cancel: screen.getByRole("button", { name: "Cancel" }),
    confirm: screen.getByRole("button", { name: "Delete" }),
  };
}

// Focus trapping and focus restore are owned by Radix Dialog (ui/Dialog);
// these tests assert the ConfirmDialog contract layered on top: initial
// focus on Cancel, Escape → onCancel, labelled modal semantics, and the
// action wiring / button order.
describe("ConfirmDialog", () => {
  it("moves focus to Cancel (least-destructive) on open", () => {
    const { cancel } = setup();
    expect(document.activeElement).toBe(cancel);
  });

  it("Escape calls onCancel", () => {
    const { onCancel } = setup();
    // Radix's escape handler hangs off the document tree, not window —
    // dispatch from the focused element like a real keypress would.
    fireEvent.keyDown(document.activeElement ?? document.body, {
      key: "Escape",
    });
    expect(onCancel).toHaveBeenCalledTimes(1);
  });

  it("exposes a labelled dialog with the title as accessible name", () => {
    setup();
    // Radix conveys modality by aria-hiding the rest of the page rather
    // than aria-modal; the labelled dialog role is the contract here.
    const dialog = screen.getByRole("dialog", { name: "Delete node?" });
    expect(dialog).toBeTruthy();
  });

  it("confirm button calls onConfirm", () => {
    const { onConfirm, confirm } = setup();
    fireEvent.click(confirm);
    expect(onConfirm).toHaveBeenCalledTimes(1);
  });

  it("cancel button calls onCancel", () => {
    const { onCancel, cancel } = setup();
    fireEvent.click(cancel);
    expect(onCancel).toHaveBeenCalledTimes(1);
  });

  it("renders the secondary action between Cancel and confirm", () => {
    const onSecondary = vi.fn();
    setup({
      secondaryAction: { label: "Keep copy", onClick: onSecondary },
    });
    const labels = screen.getAllByRole("button").map((b) => b.textContent);
    expect(labels.indexOf("Cancel")).toBeLessThan(labels.indexOf("Keep copy"));
    expect(labels.indexOf("Keep copy")).toBeLessThan(labels.indexOf("Delete"));
    fireEvent.click(screen.getByRole("button", { name: "Keep copy" }));
    expect(onSecondary).toHaveBeenCalledTimes(1);
  });
});
