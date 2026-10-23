// @vitest-environment jsdom
// A kind-"delivery" resolution (the in-app password change) travels THROUGH
// the record: the machine persists the resolution in it (resolved_gone_at),
// the write fires the record's storage event, and every tab that reads the
// record next resolves it GONE — including a tab that missed the delivery
// itself, whose renewals and identity loads the OWED+HELD gate would
// otherwise block forever. The delivery is not a confirmation and not an
// abandonment: the record stays OWED, resolved GONE, for its warning until
// its expiry. A kind-"sign-in" delivery keeps moving an owed record to
// ABANDONED (the user chose a sign-in; I2 keeps the notice).
//
// The two "tabs" are fresh module instances (vi.resetModules between them):
// the machine's per-tab state — the held lineage, this tab's own
// replacement stamp — lives in module scope, exactly as one copy per
// document does in the browser.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { SIGN_OUT_PENDING_KEY as KEY } from "@/api/signOutMachine";
import type { SignOutRecord } from "@/api/signOutMachine";

const stored = (): SignOutRecord | null => {
  const raw = localStorage.getItem(KEY);
  return raw === null ? null : (JSON.parse(raw) as SignOutRecord);
};

// freshTab re-imports the machine as a new document would load it: its own
// module state, reading the shared record from storage.
async function freshTab(): Promise<typeof import("@/api/signOutMachine")> {
  vi.resetModules();
  return import("@/api/signOutMachine");
}

// browserWithOwedRecord is a tab that asked to sign out lineage L1 and whose
// ask is unconfirmed: the record stands OWED, naming L1.
async function browserWithOwedRecord() {
  const tabA = await freshTab();
  tabA.recordSessionIdentity({ lineage: "L1", account: "a" });
  tabA.signOutRequested();
  expect(stored()?.state).toBe("owed");
  expect(stored()?.lineage).toBe("L1");
  return tabA;
}

beforeEach(() => {
  localStorage.clear();
});

afterEach(() => {
  localStorage.clear();
  vi.resetModules();
});

describe("a kind-delivery resolution reaches every tab", () => {
  it("writes the resolution into the record, so the storage event tells the other tabs", async () => {
    const tabA = await browserWithOwedRecord();
    const before = localStorage.getItem(KEY);
    // The in-app password change: the delivery replaced the jar, the record's
    // session is gone from it.
    tabA.resolveDelivered("delivery");
    tabA.recordSessionIdentity({ lineage: "L2", account: "a" });
    // The record MOVED: the resolution is in it, where the storage event —
    // and so every other tab — can read it. The record stays OWED: the
    // delivery is not a confirmation and not an abandonment.
    const after = stored();
    expect(after).not.toBeNull();
    expect(localStorage.getItem(KEY)).not.toBe(before);
    expect(after?.state).toBe("owed");
    expect(after?.lineage).toBe("L1");
    expect(after?.resolved_gone_at).toEqual(expect.any(Number));
  });

  it("a tab that missed the delivery resolves the record GONE at its next read and renews again", async () => {
    const tabA = await browserWithOwedRecord();
    tabA.resolveDelivered("delivery");
    // Tab B was suspended through the whole incident: it read L1 as its
    // lineage before the sign-out, and processed neither the sign-out's
    // event nor the delivery's. Its own module state still says L1.
    const tabB = await freshTab();
    tabB.recordSessionIdentity({ lineage: "L1", account: "a" });
    // The storage event lands (the delivery's write fired it): the tab
    // re-reads the record and re-renders from it (I6).
    tabB.externalChange();
    const record = tabB.readRecord();
    expect(record?.state).toBe("owed");
    expect(tabB.heldnessOf(record!)).toBe("gone");
    // The gate every renewal and identity load consults: open — the stale
    // tab converges instead of standing still.
    expect(tabB.signOutInProgress()).toBe(false);
    // A record the tab cannot present is not presented again either.
    await expect(tabB.present()).resolves.toBe("none");
  });

  it("a record whose session expiry is known still expires at that bound, resolution or not", async () => {
    const tabA = await browserWithOwedRecord();
    tabA.resolveDelivered("delivery");
    const stale = stored()!;
    stale.expires_at = "2020-01-01T00:00:00.000Z";
    localStorage.setItem(KEY, JSON.stringify(stale));
    // A fresh tab converges on the resolution AND drops the record past its
    // own bound: nothing live is being hidden (I1's exception).
    const tabB = await freshTab();
    tabB.recordSessionIdentity({ lineage: "L1", account: "a" });
    tabB.expireStale();
    expect(stored()).toBeNull();
    expect(tabB.readRecord()).toBeNull();
  });
});

describe("a kind-sign-in delivery over an owed record", () => {
  it("still moves the record to ABANDONED — the notice, not the resolution", async () => {
    const tabA = await browserWithOwedRecord();
    const before = localStorage.getItem(KEY);
    tabA.resolveDelivered("sign-in");
    // The user chose a sign-in: the record moves to ABANDONED (the write is
    // observable by the other tabs), which is a named state with its notice
    // (I2) — never a silent removal.
    const after = stored();
    expect(localStorage.getItem(KEY)).not.toBe(before);
    expect(after?.state).toBe("abandoned");
    expect(after?.abandoned_at).toEqual(expect.any(Number));
    expect(after?.resolved_gone_at).toBeNull();
  });

  it("the abandoned record's notice stands in a tab that missed the sign-in", async () => {
    const tabA = await browserWithOwedRecord();
    tabA.resolveDelivered("sign-in");
    tabA.recordSessionIdentity({ lineage: "L2", account: "a" });
    const tabB = await freshTab();
    tabB.recordSessionIdentity({ lineage: "L2", account: "a" });
    tabB.externalChange();
    // The record stands (I2): the notice renders from it in every tab, and
    // it never blocks the new session's renewals.
    expect(tabB.readRecord()?.state).toBe("abandoned");
    expect(tabB.signOutInProgress()).toBe(false);
  });
});

// The bound the identity reads feed: what an /auth/me answer naming the
// session's own expiry (session_lineage + expires_at) writes into the record,
// and what a read naming no bound (today's wire) leaves behind.
describe("the record's bound as the identity reads fed it", () => {
  it("carries the bound an identity read named, through the delivery resolution", async () => {
    const tabA = await freshTab();
    tabA.recordSessionIdentity({ lineage: "L1", account: "a", expiresAt: "2027-06-01T00:00:00.000Z" });
    tabA.signOutRequested();
    expect(stored()?.expires_at).toBe("2027-06-01T00:00:00.000Z");
    // The resolution keeps the record's bound: the notices name the date and
    // the expiry rule still has a bound to act on.
    tabA.resolveDelivered("delivery");
    expect(stored()?.expires_at).toBe("2027-06-01T00:00:00.000Z");
  });

  it("expireStale drops a resolved record past the bound the identity read fed", async () => {
    const tabA = await freshTab();
    tabA.recordSessionIdentity({
      lineage: "L1",
      account: "a",
      expiresAt: new Date(Date.now() - 1000).toISOString(),
    });
    tabA.signOutRequested();
    tabA.resolveDelivered("delivery");
    expect(stored()?.resolved_gone_at).toEqual(expect.any(Number));
    // The bound alone decides — nothing live is being hidden (I1's exception).
    tabA.expireStale();
    expect(stored()).toBeNull();
    expect(tabA.readRecord()).toBeNull();
  });

  it("a read naming no bound leaves the record without one: it sticks, unpresentable", async () => {
    const tab = await freshTab();
    tab.recordSessionIdentity({ lineage: "L1", account: "a" });
    tab.signOutRequested();
    tab.resolveDelivered("delivery");
    // The degraded wire: no bound, so the expiry rule never fires and the
    // record stands — the warning stands with it, and the machine never
    // presents the record or blocks the held session's renewals.
    expect(stored()?.expires_at).toBeNull();
    tab.expireStale();
    expect(stored()).not.toBeNull();
    expect(tab.signOutInProgress()).toBe(false);
    await expect(tab.present()).resolves.toBe("none");
  });
});
