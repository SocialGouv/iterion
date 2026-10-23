// @vitest-environment jsdom
// The banner a GONE record renders: the browser no longer holds the session
// the record names, so `present()` answers "none" without a wire call — a
// Retry button there is a silent no-op. What a GONE record offers instead is
// shaped by its state:
//   - OWED+GONE: an acknowledge ("Got it — the session stays active") that
//     moves the record to ABANDONED — the machine's named end for a record
//     this browser can no longer present. It presents nothing and ends
//     nothing; the notice STAYS (I2: an abandonment is a named state with a
//     visible notice).
//   - ABANDONED+GONE: no action at all — the notice stands; there is nothing
//     left to do.
//   - anything not GONE: the Retry button, which presents the logout again.
//
// The useAuth stand-in wires the component to the REAL machine exactly as
// AuthContext does: the record read on every change of it (I6), the heldness
// derived from it. The records are written through the machine's own
// transitions, so the expiry they name is the one an identity read fed —
// with the bound when the reader sends it, null on today's degraded wire.
import { useSyncExternalStore } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

const wire = vi.hoisted(() => ({ retrySignOut: vi.fn(async () => "none" as const) }));

vi.mock("@/auth/AuthContext", async () => {
  const machine = await import("@/api/signOutMachine");
  return {
    useAuth: () => {
      useSyncExternalStore(machine.subscribe, machine.snapshotVersion);
      const record = machine.readRecord();
      return {
        signOutRecord: record ? { record, heldness: machine.heldnessOf(record) } : null,
        retrySignOut: wire.retrySignOut,
      };
    },
  };
});

import SignOutUnconfirmed from "@/components/shared/SignOutUnconfirmed";
import {
  SIGN_OUT_PENDING_KEY,
  resolveDelivered,
  recordSessionIdentity,
  signOutRequested,
  type SignOutRecord,
} from "@/api/signOutMachine";

const FED_BOUND = "2027-06-01T00:00:00.000Z";

const stored = (): SignOutRecord | null => {
  const raw = localStorage.getItem(SIGN_OUT_PENDING_KEY);
  return raw === null ? null : (JSON.parse(raw) as SignOutRecord);
};

// aDeliveredRecord asks to sign out the lineage the identity read named and
// lands the delivery that replaced the jar: the record stands OWED, resolved
// GONE — the degraded state every tab of an in-app password change comes to.
function aDeliveredRecord(expiresAt?: string): void {
  recordSessionIdentity({ lineage: "L1", account: "a@example.test", expiresAt });
  signOutRequested();
  resolveDelivered("delivery");
}

beforeEach(() => {
  localStorage.clear();
  wire.retrySignOut.mockClear();
  // The machine's record must never reach the wire through this suite: the
  // acknowledge and the shapes around it are local decisions.
  vi.stubGlobal("fetch", vi.fn(async () => {
    throw new Error("this suite drives no wire call");
  }));
});

afterEach(() => {
  cleanup();
  localStorage.clear();
  vi.unstubAllGlobals();
});

describe("the actions a GONE sign-out record offers", () => {
  it("offers the acknowledge instead of a retry, and acknowledging names the abandonment", async () => {
    aDeliveredRecord();
    render(<SignOutUnconfirmed />);
    expect(screen.getByRole("alert").textContent).toContain("Sign-out not confirmed");
    // A GONE record cannot be presented: the retry would be a silent no-op.
    expect(screen.queryByRole("button", { name: "Retry sign-out" })).toBeNull();
    const ack = screen.getByRole("button", { name: "Got it — the session stays active" });
    fireEvent.click(ack);
    // The acknowledge is the machine's abandon: the record MOVES to its named
    // abandoned state — never a silent removal (I2) — and no wire call runs.
    const record = stored();
    expect(record?.state).toBe("abandoned");
    expect(record?.abandoned_at).toEqual(expect.any(Number));
    expect(record?.lineage).toBe("L1");
    expect(wire.retrySignOut).not.toHaveBeenCalled();
    expect(vi.mocked(fetch).mock.calls).toHaveLength(0);
    // The notice stands under the abandoned title.
    await waitFor(() =>
      expect(screen.getByRole("alert").textContent).toContain(
        "Sign-out abandoned — that session is still active",
      ),
    );
    expect(screen.getByRole("alert").textContent).toContain("stays active");
    // Nothing is left to do from this browser: the acknowledge is gone too.
    expect(screen.queryByRole("button", { name: "Got it — the session stays active" })).toBeNull();
  });

  it("offers no action on an abandoned record this browser cannot present", () => {
    recordSessionIdentity({ lineage: "L1", account: "a@example.test" });
    signOutRequested();
    // The user went on through a sign-in, and the delivery that followed
    // resolved the record GONE — the abandoned+gone shape a later tab reads.
    // Nothing is left to do: no retry, no acknowledge — the notice alone.
    const record = stored()!;
    localStorage.setItem(
      SIGN_OUT_PENDING_KEY,
      JSON.stringify({ ...record, state: "abandoned", abandoned_at: Date.now(), resolved_gone_at: Date.now() }),
    );
    render(<SignOutUnconfirmed />);
    expect(screen.getByRole("alert").textContent).toContain(
      "Sign-out abandoned — that session is still active",
    );
    expect(screen.queryByRole("button", { name: "Retry sign-out" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Got it — the session stays active" })).toBeNull();
    expect(screen.getByRole("alert").textContent).toContain("You went on without this sign-out");
    // The notice does not point at a retry this browser cannot offer: the
    // way on is named instead.
    expect(screen.getByRole("alert").textContent).toContain(
      "Nothing here ends it — sign in and sign out again, or retry from the browser that holds it.",
    );
    expect(screen.getByRole("alert").textContent).not.toContain(
      "Retry the sign-out before you leave a shared computer.",
    );
  });

  it("still offers the retry on a record this browser holds", () => {
    recordSessionIdentity({ lineage: "L1", account: "a@example.test" });
    signOutRequested();
    render(<SignOutUnconfirmed />);
    expect(screen.getByRole("button", { name: "Retry sign-out" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Got it — the session stays active" })).toBeNull();
  });

  it("names the bound an identity read fed, and 'its expiry' where the read had none", () => {
    // The bound travels when the identity read carries it (the /auth/me
    // answer naming the session's own expiry): the notice names the date.
    aDeliveredRecord(FED_BOUND);
    render(<SignOutUnconfirmed />);
    expect(screen.getByRole("alert").textContent).toContain(`until ${FED_BOUND}`);
    expect(stored()?.expires_at).toBe(FED_BOUND);
    expect(screen.getByRole("button", { name: "Got it — the session stays active" })).toBeTruthy();
    cleanup();
    localStorage.clear();
    // Today's wire names no bound: the notice falls back to "its expiry",
    // and the degraded record still gets the acknowledge — a fixed
    // forever-warning with a dead button is what this replaces.
    aDeliveredRecord();
    render(<SignOutUnconfirmed />);
    expect(screen.getByRole("alert").textContent).toContain("until its expiry");
    expect(stored()?.expires_at).toBeNull();
    expect(screen.getByRole("button", { name: "Got it — the session stays active" })).toBeTruthy();
  });
});
