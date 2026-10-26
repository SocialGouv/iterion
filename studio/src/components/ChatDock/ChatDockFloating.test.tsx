// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, waitFor } from "@testing-library/react";

import { ChatDockFloating } from "./ChatDockShell";

afterEach(cleanup);

// RGAA 12.8: closing the dock must hand focus back to what opened it,
// never drop it on <body> (the tab order would restart from the top).
describe("ChatDockFloating focus return", () => {
  it("moves focus into the panel on open and back to the opener on close", async () => {
    const opener = document.createElement("button");
    document.body.appendChild(opener);
    opener.focus();
    expect(document.activeElement).toBe(opener);

    const { unmount } = render(
      <ChatDockFloating label="Assistant" focusOnMount onClose={() => {}}>
        <button type="button">dock action</button>
      </ChatDockFloating>,
    );
    const panel = document.querySelector('[role="dialog"]') as HTMLElement;
    await waitFor(() => expect(panel.contains(document.activeElement)).toBe(true));

    unmount();
    expect(document.activeElement).toBe(opener);
    opener.remove();
  });

  it("leaves the page focus alone on a restore mount (focusOnMount off)", () => {
    const opener = document.createElement("button");
    document.body.appendChild(opener);
    opener.focus();

    const { unmount } = render(
      <ChatDockFloating label="Assistant" onClose={() => {}}>
        <button type="button">dock action</button>
      </ChatDockFloating>,
    );
    unmount();
    expect(document.activeElement).toBe(opener);
    opener.remove();
  });
});
