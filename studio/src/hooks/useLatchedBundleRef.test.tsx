// @vitest-environment jsdom
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { useLatchedBundleRef, type BundleRef } from "./useLatchedBundleRef";

afterEach(cleanup);

function Host({ parsed, open }: { parsed: BundleRef | null; open: boolean }) {
  const ref = useLatchedBundleRef(parsed, open);
  // Mirrors Toolbar.tsx's conditional mount: losing the ref unmounts the
  // drawer, which is what skips its discard gate.
  return <div data-testid="out">{ref ? `${ref.teamID}/${ref.slug}` : "unmounted"}</div>;
}

const DEMO: BundleRef = { teamID: "team-1", slug: "demo", rel: "main.bot" };

describe("useLatchedBundleRef", () => {
  it("keeps the drawer mounted when the editor leaves the bundle path", () => {
    const view = render(<Host parsed={DEMO} open />);
    expect(screen.getByTestId("out").textContent).toBe("team-1/demo");
    // File → New / Import / "Start blank": currentFilePath stops being a
    // botsource:// path, so parseBotSourceEditorPath answers null.
    view.rerender(<Host parsed={null} open />);
    expect(screen.getByTestId("out").textContent).toBe("team-1/demo");
  });

  it("lets it go once the drawer is closed", () => {
    const view = render(<Host parsed={DEMO} open />);
    view.rerender(<Host parsed={null} open={false} />);
    expect(screen.getByTestId("out").textContent).toBe("unmounted");
  });

  it("never holds a stale bundle over a live one", () => {
    const view = render(<Host parsed={DEMO} open />);
    const other: BundleRef = { teamID: "team-1", slug: "other", rel: "main.bot" };
    view.rerender(<Host parsed={other} open />);
    expect(screen.getByTestId("out").textContent).toBe("team-1/other");
  });

  it("stays unmounted when there was never a bundle", () => {
    render(<Host parsed={null} open />);
    expect(screen.getByTestId("out").textContent).toBe("unmounted");
  });
});
