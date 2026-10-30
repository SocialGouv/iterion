// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { Router } from "wouter";
import { memoryLocation } from "wouter/memory-location";

import type { BotEntryWithSchema } from "@/api/bots";
import {
  CLAMP_TEXT_WAIVER,
  expectNoViolationsStrict,
  setupMatchMedia,
} from "@/__tests__/a11y/axeHelpers";
import { onAssistantAttachRequest } from "@/lib/chatDock/attachToAssistant";
import type { TypedReference } from "@/lib/chatDock/routeReference";

// The gallery's own read. One subscription on the first bot, not an empty
// list: the trigger badge only renders above zero, and an empty answer would
// leave that markup out of the audit.
vi.mock("@/api/triggers", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/api/triggers")>();
  return {
    ...actual,
    listTriggers: () =>
      Promise.resolve([
        {
          id: "sub-1",
          bot_id: "rgaa-audit",
          invocation: "forge" as const,
          match: { kinds: ["issue"], actions: ["labeled"] },
        },
      ]),
  };
});

import BotsView from "./index";
import { useBotsStore } from "@/store/bots";
import { useServerInfoStore } from "@/store/serverInfo";

setupMatchMedia();

const BOTS: BotEntryWithSchema[] = [
  {
    name: "rgaa-audit",
    display_name: "Acci",
    description: "Audits a surface against RGAA",
    path: "/w/.botz/rgaa-audit",
    rel_path: ".botz/rgaa-audit",
    category: "verify",
    tags: ["a11y", "security"],
    triggers: ["issue_labeled"],
  },
  {
    name: "revi",
    display_name: "Revi",
    description: "Reviews a pull request",
    path: "/w/bots/revi",
    rel_path: "bots/revi",
    category: "verify",
    tags: ["security"],
  },
];

function mountGallery(): HTMLElement {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  const loc = memoryLocation({ path: "/bots" });
  return render(
    <QueryClientProvider client={qc}>
      <Router hook={loc.hook}>
        <main>
          <BotsView />
        </main>
      </Router>
    </QueryClientProvider>,
  ).container;
}

// Both stores are module singletons: what the seeding below overwrites is
// put back, so the file cannot leave a no-op loader behind for a sibling
// suite the day vitest's per-file isolation is turned off.
const BOTS_STATE = useBotsStore.getState();
const SERVER_STATE = useServerInfoStore.getState();

beforeEach(() => {
  // Seeded, not fetched: the view's effect calls refetch() when bots are
  // already there, and the store's loader is the API's business, not this
  // net's. `fetch`/`refetch` are replaced so no request is attempted.
  useBotsStore.setState({
    bots: BOTS,
    loading: false,
    error: null,
    discoveryErrors: [],
    fetch: () => Promise.resolve(),
    refetch: () => Promise.resolve(),
  });
  useServerInfoStore.setState({
    info: {
      mode: "local",
      marketplace_enabled: true,
      triggers_enabled: true,
    } as never,
  });
});

afterEach(() => {
  cleanup();
  useBotsStore.setState(BOTS_STATE);
  useServerInfoStore.setState(SERVER_STATE);
});

describe("a11y / Bots gallery", () => {
  it("has no violation over a populated catalog", async () => {
    const container = mountGallery();
    // Non-vacuity: the cards are what the net is over. A gallery that
    // rendered its empty state would pass this audit and prove nothing.
    // ClampText renders its text twice (visible + its sr-only description),
    // so the count is not the assertion — the presence of the cards is.
    expect((await screen.findAllByText("Acci")).length).toBeGreaterThan(0);
    expect(screen.getAllByText("Revi").length).toBeGreaterThan(0);
    // The trigger badge is the one piece of card markup that needs the
    // subscription read, so the audit's scope is asserted, not assumed.
    expect(await screen.findByText(/^1 trigger$/)).toBeTruthy();
    // The cards' clamped title/path/description blocks are ClampText's
    // nodes, waived on the primitive's own attribute so a roleless focusable
    // the GALLERY adds still fails, in this same entry.
    await expectNoViolationsStrict(container, "BotsView", [CLAMP_TEXT_WAIVER]);
  });

  it("has no violation on the empty catalog", async () => {
    useBotsStore.setState({ bots: [] });
    const container = mountGallery();
    // Same rule as above: the empty state is what this entry is over, and a
    // view that rendered nothing at all would pass without it. Awaited, so
    // the trigger read cannot land while axe is walking the tree.
    expect(
      await screen.findByText("No bots discovered in this workspace"),
    ).toBeTruthy();
    await expectNoViolationsStrict(container, "BotsView (empty)");
  });
});

describe("a11y / Bots gallery drag-to-assistant (#2770)", () => {
  it("today each card's grip is a drag-only affordance, and no control offers the attach", async () => {
    mountGallery();
    // Await the cards — the grips render with them, not before.
    await screen.findAllByText("Acci");
    // The current state, pinned honestly: each card's grip is draggable but
    // not focusable and claims no operation (role="img" is a text
    // alternative, not a widget), and no named control elsewhere on the card
    // performs the attach. SC 2.1.1 / 2.5.7 are exactly what #2770 still
    // owes — this test is meant to FAIL the day the placement lands, and to
    // be rewritten as the flip of the skipped one below.
    for (const name of [
      "Drag Acci onto the assistant",
      "Drag Revi onto the assistant",
    ]) {
      const grip = screen.getByRole("img", { name });
      expect(grip.getAttribute("draggable")).toBe("true");
      expect(grip.hasAttribute("tabindex")).toBe(false);
      expect(grip.tagName).not.toBe("BUTTON");
    }
    expect(
      screen.queryByRole("button", {
        name: /attach .+ (to|onto) the assistant/i,
      }),
    ).toBeNull();
  });

  // #2770 — enable with the placement Jo picks: the grip becoming a button,
  // or a named control elsewhere on the card. Written against the ticket's
  // "Attach <label> to the assistant" wording — adjust the name pattern to
  // the landed label, nothing else. Either placement must wire the control
  // to the shared attachReferenceToAssistant handler the drop path uses.
  it.skip("each card offers a keyboard-operable Attach control wired to the shared handler", async () => {
    const received: TypedReference[] = [];
    const off = onAssistantAttachRequest((r) => received.push(r));
    try {
      mountGallery();
      await screen.findAllByText("Acci");
      const controls = screen.getAllByRole("button", {
        name: /^Attach .+ (to|onto) the assistant$/,
      });
      expect(controls).toHaveLength(2);
      const [first] = controls;
      if (!first) throw new Error("two cards, two attach controls");
      first.focus();
      expect(document.activeElement).toBe(first);
      // A native control turns Enter/Space into click — the keyboard path IS
      // the click handler.
      fireEvent.keyDown(first, { key: "Enter" });
      fireEvent.click(first);
      // The bot reference is self-labelling: the chip names the path, and
      // the expected value below follows the first card's rel_path.
      expect(received).toEqual([
        { kind: "bot", ref: "bot/.botz/rgaa-audit", label: ".botz/rgaa-audit" },
      ]);
    } finally {
      off();
    }
  });
});
