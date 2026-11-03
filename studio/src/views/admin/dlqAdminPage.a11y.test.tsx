// @vitest-environment jsdom
//
// The dead-letter console repeats THREE identical words per parked message —
// Inspect / Replay / Delete — and axe has no rule for two controls answering
// to one name (#2591). It is also out of both nets today: .ultra11yrc.json's
// 21-page sample excludes the cloud-only admin screens, so this is the only
// engine that judges it.
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { Route, Router } from "wouter";
import { memoryLocation } from "wouter/memory-location";

import type { DLQListResponse, DLQMessage, DLQPeekResponse } from "@/api/dlq";
import {
  expectNoViolationsStrict,
  expectUniqueAccessibleNames,
  setupMatchMedia,
} from "@/__tests__/a11y/axeHelpers";

let mockUser: { is_super_admin?: boolean } | null = null;
let mockInfo: { mode?: string } | null = null;
vi.mock("@/auth/AuthContext", () => ({ useAuth: () => ({ user: mockUser }) }));
vi.mock("@/store/serverInfo", () => ({
  useServerInfoStore: (sel: (s: { info: unknown }) => unknown) => sel({ info: mockInfo }),
}));

const listDLQ = vi.fn<() => Promise<DLQListResponse>>();
const peekDLQ = vi.fn<() => Promise<DLQPeekResponse>>();
vi.mock("@/api/dlq", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/api/dlq")>();
  return { ...actual, listDLQ: () => listDLQ(), peekDLQ: () => peekDLQ() };
});

import DLQAdminPage from "./DLQAdminPage";
import { ADMIN_BASE } from "./adminRoutes";

setupMatchMedia();

const message = (seq: number, runID: string): DLQMessage => ({
  seq,
  run_id: runID,
  tenant_id: "t-1",
  reason: "max deliveries exceeded",
  num_delivered: "5",
  parked_at: "2026-11-02T22:00:00Z",
  size_bytes: 4096,
});

const PARKED_41 = message(41, "0199aaaa-1111");
const MESSAGES = [PARKED_41, message(42, "0199bbbb-2222")];

// The screen in its own place: inside <Router base="/studio"> and the
// console's nested route, so the AdminNav it renders resolves its links the
// way it does in the app (mountScreen, adminScreens.test.tsx).
function mountPage(): HTMLElement {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  const loc = memoryLocation({ path: `/studio${ADMIN_BASE}/dlq` });
  return render(
    <QueryClientProvider client={qc}>
      <Router hook={loc.hook} base="/studio">
        <Route path={ADMIN_BASE} nest>
          <main>
            <DLQAdminPage />
          </main>
        </Route>
      </Router>
    </QueryClientProvider>,
  ).container;
}

beforeEach(() => {
  listDLQ.mockReset();
  peekDLQ.mockReset();
  listDLQ.mockResolvedValue({ messages: MESSAGES, next_cursor: 0 });
  peekDLQ.mockResolvedValue({ message: PARKED_41, payload: { node: "review" } });
  mockUser = { is_super_admin: true };
  mockInfo = { mode: "cloud" };
});
afterEach(() => {
  cleanup();
  mockUser = null;
  mockInfo = null;
});

describe("DLQ console naming (RGAA 11.1)", () => {
  it("names every row's three actions after the message they act on", async () => {
    mountPage();
    await screen.findByRole("table", { name: "Dead-lettered queue messages" });
    for (const name of [
      "Inspect message 41",
      "Replay message 41",
      "Delete message 41",
      "Inspect message 42",
      "Replay message 42",
      "Delete message 42",
    ]) {
      expect(screen.getAllByRole("button", { name })).toHaveLength(1);
    }
  });

  it("keeps the visible word first, so the name contains the label (SC 2.5.3)", async () => {
    mountPage();
    await screen.findByRole("table", { name: "Dead-lettered queue messages" });
    expect(screen.getByRole("button", { name: "Inspect message 41" }).textContent).toBe(
      "Inspect",
    );
    expect(screen.getByRole("button", { name: "Replay message 42" }).textContent).toBe("Replay");
  });

  it("follows its own label when the inspect toggle flips", async () => {
    mountPage();
    await screen.findByRole("table", { name: "Dead-lettered queue messages" });
    fireEvent.click(screen.getByRole("button", { name: "Inspect message 41" }));
    const toggle = await screen.findByRole("button", { name: "Hide payload of message 41" });
    // A name that stopped matching the visible word would break SC 2.5.3 in
    // exactly the state nobody looks at.
    expect(toggle.textContent).toBe("Hide payload");
    expect(screen.getByRole("button", { name: "Inspect message 42" })).toBeTruthy();
  });

  it("leaves no two row controls sharing an accessible name", async () => {
    const container = mountPage();
    await screen.findByRole("table", { name: "Dead-lettered queue messages" });
    expectUniqueAccessibleNames(container, "DLQ console · rows", {
      roles: [{ role: "button", matching: [/ message \d+$/] }],
      count: 6,
    });
  });
});

describe("DLQ console axe", () => {
  it("DLQAdminPage — parked messages, axe smoke, incompletes included", async () => {
    const container = mountPage();
    await screen.findByRole("table", { name: "Dead-lettered queue messages" });
    await expectNoViolationsStrict(container, "DLQAdminPage");
  });

  it("DLQAdminPage — an expanded payload row", async () => {
    const container = mountPage();
    await screen.findByRole("table", { name: "Dead-lettered queue messages" });
    fireEvent.click(screen.getByRole("button", { name: "Inspect message 41" }));
    await screen.findByRole("button", { name: "Hide payload of message 41" });
    await expectNoViolationsStrict(container, "DLQAdminPage (payload open)");
  });

  it("DLQAdminPage — the super-admin gate short-circuit", async () => {
    mockUser = { is_super_admin: false };
    const container = mountPage();
    await screen.findByText("Super-admin only.");
    await expectNoViolationsStrict(container, "DLQAdminPage (gated)");
  });
});
