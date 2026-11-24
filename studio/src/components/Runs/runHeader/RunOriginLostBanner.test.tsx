// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import RunOriginLostBanner from "./RunOriginLostBanner";

afterEach(cleanup);

describe("the run origin-lost banner", () => {
  it("says the run is gone, in French, and offers a reload", () => {
    const onReload = vi.fn();
    render(<RunOriginLostBanner state="lost" onReload={onReload} />);
    const alert = screen.getByRole("alert");
    expect(alert.textContent).toContain("Ce run a été remplacé ou supprimé");
    expect(alert.closest("[lang]")?.getAttribute("lang")).toBe("fr");
    fireEvent.click(screen.getByRole("button", { name: "Recharger" }));
    expect(onReload).toHaveBeenCalledTimes(1);
  });

  it.each(["open", "connecting", "reconnecting", "closed", "idle"] as const)(
    "renders nothing while the socket is %s",
    (state) => {
      const { container } = render(<RunOriginLostBanner state={state} />);
      expect(container.firstChild).toBeNull();
    },
  );
});
