// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { CopyButton } from "./CopyButton";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

function stubClipboard() {
  const writeText = vi.fn(async () => {});
  Object.defineProperty(navigator, "clipboard", {
    configurable: true,
    value: { writeText },
  });
  return writeText;
}

// `describes` exists because a row of buttons all announcing "copy" is one
// control to a screen reader (#2591). It qualifies the NAME only, and it
// qualifies the resting label: a name that changes while the control is
// focused is re-announced, and "Copied of plan" is not a sentence.
describe("CopyButton naming", () => {
  it("qualifies the accessible name, never the tooltip", () => {
    render(<CopyButton value="x" describes="of plan" />);
    const button = screen.getByRole("button", { name: "copy of plan" });
    expect(button.textContent).toBe("copy");
    expect(button.getAttribute("title")).toBe("copy");
  });

  it("keeps the name steady across the copied state", async () => {
    const writeText = stubClipboard();
    render(<CopyButton value="payload" describes="of plan" />);
    const button = screen.getByRole("button", { name: "copy of plan" });
    fireEvent.click(button);
    await waitFor(() => expect(writeText).toHaveBeenCalledWith("payload"));
    // The eye sees the confirmation; the name does not move with it.
    await waitFor(() => expect(button.textContent).toBe("copied"));
    expect(button.getAttribute("aria-label")).toBe("copy of plan");
    expect(button.getAttribute("title")).toBe("copied");
  });

  it("says just the label when nothing qualifies it", () => {
    render(<CopyButton value="x" />);
    const button = screen.getByRole("button", { name: "copy" });
    expect(button.getAttribute("title")).toBe("copy");
  });
});
