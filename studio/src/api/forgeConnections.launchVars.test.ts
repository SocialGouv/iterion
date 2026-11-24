// The wire of the two launch_vars gestures (#2173). The remedy the server
// names is useless if the flag never leaves the browser, and the refusal's
// keys are useless if the reader parses the prose instead of the envelope —
// so both are asserted against the shapes pkg/server actually writes.
import { afterEach, describe, expect, it, vi } from "vitest";

import { apiErrorFrom } from "./client";
import {
  LAUNCH_VARS_DROP_CODE,
  PROVISION_DIVERGED_CODE,
  PROVISION_DIVERGED_RETRY_CODE,
  launchVarsRefusalOf,
  updateForgeRepoBots,
} from "./forgeConnections";

vi.mock("@/lib/scope", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/scope")>()),
  apiBase: () => "/api",
}));
afterEach(() => vi.unstubAllGlobals());

const bodyOf = (call: [string, RequestInit]) =>
  JSON.parse(String(call[1].body)) as Record<string, unknown>;

describe("updateForgeRepoBots — the replace gesture on the wire", () => {
  it("sends launch_vars_replace only when the gesture was made", async () => {
    const request = vi.fn(async () => new Response("{}", { status: 200 }));
    vi.stubGlobal("fetch", request);

    await updateForgeRepoBots("t1", "i1", undefined, undefined, { gate_enabled: "false" });
    await updateForgeRepoBots(
      "t1",
      "i1",
      undefined,
      undefined,
      { gate_enabled: "false" },
      undefined,
      true,
    );

    const calls = request.mock.calls as unknown as [string, RequestInit][];
    expect(calls).toHaveLength(2);
    // An ordinary write must not carry the key at all: a `false` on the wire
    // reads as a posture the operator never took.
    expect("launch_vars_replace" in bodyOf(calls[0]!)).toBe(false);
    expect(bodyOf(calls[1]!).launch_vars_replace).toBe(true);
  });
});

describe("launchVarsRefusalOf — the kind and the keys come from the envelope", () => {
  it("reads the drop refusal's keys from launch_vars_keys, not from the message", () => {
    const refusal = launchVarsRefusalOf(
      apiErrorFrom(400, {
        message: "forge: launch_vars would drop pinned key(s) [a b] — echo them …",
        errorCode: LAUNCH_VARS_DROP_CODE,
        body: { error_code: LAUNCH_VARS_DROP_CODE, launch_vars_keys: ["a", "b"] },
      }),
    );
    expect(refusal).toEqual({
      kind: "drop",
      keys: ["a", "b"],
      message: "forge: launch_vars would drop pinned key(s) [a b] — echo them …",
    });
  });

  it("separates the retry-convergent divergence from the deterministic one", () => {
    for (const [code, kind] of [
      [PROVISION_DIVERGED_CODE, "diverged"],
      [PROVISION_DIVERGED_RETRY_CODE, "retry"],
    ] as const) {
      const refusal = launchVarsRefusalOf(
        apiErrorFrom(500, {
          message: "internal server error",
          errorCode: code,
          body: { error_code: code, retryable: kind === "retry" },
        }),
      );
      expect(refusal?.kind).toBe(kind);
      // A 500's payload is floored to its code: no key list travels, and the
      // reader must not pretend one did.
      expect(refusal?.keys).toEqual([]);
    }
  });

  it("is null for any other failure", () => {
    expect(launchVarsRefusalOf(new Error("boom"))).toBeNull();
    expect(
      launchVarsRefusalOf(apiErrorFrom(409, { message: "busy", errorCode: "provision_busy" })),
    ).toBeNull();
  });
});
