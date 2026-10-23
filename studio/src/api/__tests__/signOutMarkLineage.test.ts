// @vitest-environment jsdom
// The pending sign-out mark is what one document hands the next — a reload, a
// new tab, the next person at the computer — and the document that settles it
// has read no identity of its own: it knows no lineage. So the lineage travels
// IN the mark, and the logout names what the mark names, never what this
// module happens to remember.
//
// A document is a copy of the client module over the one localStorage, which
// is what vi.resetModules() gives: new module state, same storage.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

type Client = typeof import("@/api/client");

let logout: (() => Response) | null;
let sent: string[];

async function freshDocument(): Promise<Client> {
  vi.resetModules();
  return import("@/api/client");
}

beforeEach(() => {
  localStorage.clear();
  sent = [];
  logout = () => new Response(null, { status: 204 });
  vi.stubGlobal("fetch", vi.fn(async (input: string | URL | Request, init?: RequestInit) => {
    const url = new URL(String(input), "https://iterion.invalid");
    sent.push(`${init?.method ?? "GET"} ${url.pathname}${url.search}`);
    if (url.pathname === "/api/auth/logout") return logout!();
    throw new Error(`unexpected ${url.pathname}`);
  }));
});

afterEach(() => {
  localStorage.clear();
  vi.unstubAllGlobals();
});

describe("a pending sign-out handed to the next document", () => {
  it("carries the lineage, which that document's logout presents", async () => {
    const first = await freshDocument();
    // What an adopted identity records (session_lineage on /auth/me).
    first.recordSessionLineage("L0");
    logout = () => new Response(JSON.stringify({ error: "identity unavailable" }), { status: 500 });
    expect(await first.signOutOfServer()).toBe("unconfirmed");
    const handedOver = localStorage.getItem(first.SIGN_OUT_PENDING_KEY);
    console.log("[mark] written by the sign-out:", handedOver);
    expect(handedOver).not.toBeNull();
    expect(handedOver).toContain("L0");

    // The next document reads that mark and nothing else: no identity, so no
    // lineage of its own.
    const second = await freshDocument();
    logout = () => new Response(null, { status: 204 });
    sent = [];
    expect(await second.settlePendingSignOut()).toBe("confirmed");
    console.log("[mark] the next document presented:", JSON.stringify(sent));
    expect(sent).toEqual(["POST /api/auth/logout?lineage=L0"]);
    expect(localStorage.getItem(second.SIGN_OUT_PENDING_KEY)).toBeNull();
  });

  it("stands resolved GONE, with nothing revoked, when the server says the browser holds another session", async () => {
    const first = await freshDocument();
    first.recordSessionLineage("L0");
    logout = () => new Response(JSON.stringify({ error: "identity unavailable" }), { status: 500 });
    expect(await first.signOutOfServer()).toBe("unconfirmed");

    const second = await freshDocument();
    // S2: the refresh token this browser presents belongs to another lineage.
    // Nothing was revoked and the cookies were kept — the fact is the header,
    // not the status.
    logout = () => new Response(null, { status: 204, headers: { "X-Iterion-Logout": "other-session" } });
    sent = [];
    expect(await second.settlePendingSignOut()).toBe("other-session");
    console.log("[mark] after a logout that revoked nothing:", JSON.stringify(sent), "mark:", localStorage.getItem(second.SIGN_OUT_PENDING_KEY));
    expect(sent).toEqual(["POST /api/auth/logout?lineage=L0"]);
    // The machine's OTHER-SESSION resolution: the record STANDS (the sign-out
    // it names was never confirmed), resolved GONE — the session it names is
    // not what this browser presents, so only the warning covers it (I2); it
    // is never silently dropped.
    const record = second.readRecord();
    expect(record).not.toBeNull();
    expect(record?.state).toBe("owed");
    expect(record?.lineage).toBe("L0");
    expect(second.heldnessOf(record!)).toBe("gone");
    // And nothing stays closed over the session this browser DOES hold: a
    // GONE record blocks no renewal and no identity read.
    expect(second.signOutInProgress()).toBe(false);
  });

  it("names no lineage when the session that was signed out named none", async () => {
    const first = await freshDocument();
    logout = () => new Response(JSON.stringify({ error: "identity unavailable" }), { status: 500 });
    expect(await first.signOutOfServer()).toBe("unconfirmed");
    const second = await freshDocument();
    logout = () => new Response(null, { status: 204 });
    sent = [];
    expect(await second.settlePendingSignOut()).toBe("confirmed");
    console.log("[mark/unnamed] presented:", JSON.stringify(sent));
    expect(sent).toEqual(["POST /api/auth/logout"]);
  });

  // A mark written by a build that stored a bare timestamp is a mark about no
  // named lineage: the logout ends what the browser holds, as that build did.
  it("reads a bare timestamp as a mark naming no lineage", async () => {
    const doc = await freshDocument();
    localStorage.setItem(doc.SIGN_OUT_PENDING_KEY, String(Date.now()));
    expect(doc.signOutPending()).toBe(true);
    logout = () => new Response(null, { status: 204 });
    expect(await doc.settlePendingSignOut()).toBe("confirmed");
    console.log("[mark/legacy] presented:", JSON.stringify(sent));
    expect(sent).toEqual(["POST /api/auth/logout"]);
  });
});
