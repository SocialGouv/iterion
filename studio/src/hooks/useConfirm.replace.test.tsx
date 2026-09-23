// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { useConfirm } from "./useConfirm";

afterEach(cleanup);

// This hook holds ONE resolver slot. Until the drawer gained a gate that
// fires AUTONOMOUSLY from an effect, the two call sites were both
// user-initiated behind a modal and could not overlap. One can now pre-empt
// the other — and a displaced caller left awaiting for ever is a discard
// path that never completes.
describe("useConfirm when a second question replaces the first", () => {
  it("settles the displaced caller instead of stranding it", async () => {
    const answers: (boolean | "pending")[] = ["pending", "pending"];
    function Host() {
      const { confirm, dialog } = useConfirm();
      return (
        <>
          <button
            onClick={() => {
              // Both in one tick: the modal hides everything behind it, so
              // the pre-emption cannot be driven by a second click — which is
              // also how it happens in the drawer, where the second question
              // comes from an effect rather than from the author.
              void confirm({ title: "first", message: "m" }).then((v) => (answers[0] = v));
              void confirm({ title: "second", message: "m" }).then((v) => (answers[1] = v));
            }}
          >
            ask
          </button>
          {dialog}
        </>
      );
    }
    render(<Host />);
    fireEvent.click(screen.getByRole("button", { name: "ask" }));
    await screen.findByText("second");
    expect(screen.queryByText("first")).toBeNull();

    // The first caller must not hang: "do not discard" is the answer that
    // loses nothing for every caller of this hook.
    await waitFor(() => expect(answers[0]).toBe(false));
    expect(answers[1]).toBe("pending");
  });
});
