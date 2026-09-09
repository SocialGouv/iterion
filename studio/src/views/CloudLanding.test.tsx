// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";

import CloudLanding from "@/views/CloudLanding";
import { useServerInfoStore } from "@/store/serverInfo";
import type { ServerInfo } from "@/api/types";

// Stand in for the real product page. What is under test is the split
// itself — whether CloudLanding reaches CloudHome through a chunk boundary
// or inlines it — so the far side only has to be identifiable. Mounting the
// real one here would also drag in @lobehub/icons, which vitest's ESM
// externalization cannot resolve (the vite build resolves it fine).
vi.mock("@/views/CloudHome", async () => {
  const { createElement } = await import("react");
  return { default: () => createElement("main", null, "product page") };
});

describe("CloudLanding", () => {
  afterEach(() => cleanup());

  // App.tsx imports this module EAGERLY — PublicTopBar renders on
  // /marketplace, outside the lazy route tree — so a static
  // `import CloudHome` ships the anonymous-only marketing page (its own
  // stylesheet plus ~50 icon modules) inside the entry chunk that every
  // authenticated operator downloads on first paint. `lazy()` is what keeps
  // it out, and a revert to a static import mounts the page synchronously,
  // failing the first assertion below.
  //
  // Deliberately ONE case: `lazy()` memoises the resolved module on the
  // object built at import time, so a sibling case that awaited the chunk
  // first would leave this one unable to observe the pending state — and the
  // guard would then pass on a static import purely from test order.
  it("defers the product page to its own chunk instead of rendering it on first paint", async () => {
    useServerInfoStore.setState({ info: { mode: "cloud" } as ServerInfo });

    render(<CloudLanding />);

    // The page owns the <main> landmark, and none of it is mounted yet —
    // only the Suspense fallback's spinner.
    expect(screen.queryByRole("main")).toBeNull();
    expect(screen.getByRole("status")).toBeTruthy();

    // ...and it arrives once the chunk resolves.
    expect(await screen.findByRole("main")).toBeTruthy();
  });
});
