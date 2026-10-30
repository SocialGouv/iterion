// @vitest-environment jsdom
import { describe, expect, it, vi } from "vitest";

import {
  ASSISTANT_ATTACH_EVENT,
  attachReferenceToAssistant,
  onAssistantAttachRequest,
} from "./attachToAssistant";
import {
  REFERENCE_MIME,
  readReferenceDrop,
} from "./dragReference";
import { mintReference, type TypedReference } from "./routeReference";

// The shared attach action #2770 hangs both its paths on: the drop handler
// routes through it, and the keyboard/single-pointer control will call it
// directly. These tests pin the channel itself; the per-surface scaffolds
// (runList.a11y, bots.a11y, DetailHeader) pin the affordances.

function minted(kind: "run" | "node" | "bot", id: string, label: string): TypedReference {
  const reference = mintReference(kind, id, label);
  if (!reference) throw new Error(`${kind}/${id} must mint in this fixture`);
  return reference;
}

function fakeDataTransfer(payload: unknown): DataTransfer {
  const store = new Map<string, string>([
    [REFERENCE_MIME, JSON.stringify(payload)],
  ]);
  return {
    getData: (type: string) => store.get(type) ?? "",
  } as unknown as DataTransfer;
}

describe("attachReferenceToAssistant", () => {
  it("delivers an attached reference to every subscriber, synchronously", () => {
    const received: TypedReference[] = [];
    const offA = onAssistantAttachRequest((r) => received.push(r));
    const offB = onAssistantAttachRequest((r) => received.push(r));
    try {
      const reference = minted("run", "019f1234", "nightly");
      attachReferenceToAssistant(reference);
      // Two subscribers, one dispatch each — the ChatDock is one subscriber
      // in production, and a second consumer must not starve it.
      expect(received).toEqual([reference, reference]);
    } finally {
      offA();
      offB();
    }
  });

  it("carries exactly what the drop path reads off a drop payload", () => {
    // The ChatDock's onDrop feeds readReferenceDrop's output straight into
    // attachReferenceToAssistant — the round trip through the event must not
    // lose or reshape the reference.
    const dropped = readReferenceDrop(
      fakeDataTransfer({ kind: "node", id: "run-1/plan", label: "Plan" }),
    );
    if (!dropped) throw new Error("a legitimate payload must read back");
    const received: TypedReference[] = [];
    const off = onAssistantAttachRequest((r) => received.push(r));
    try {
      attachReferenceToAssistant(dropped);
      expect(received).toEqual([
        { kind: "node", ref: "node/run-1/plan", label: "Plan" },
      ]);
    } finally {
      off();
    }
  });

  it("ignores a forged event whose detail is not a reference", () => {
    const listener = vi.fn();
    const off = onAssistantAttachRequest(listener);
    try {
      for (const detail of [
        undefined,
        null,
        "run/019f1234",
        { kind: "run" },
        { ref: "run/019f1234", label: "x" },
        { kind: "run", ref: 42, label: "x" },
      ]) {
        window.dispatchEvent(new CustomEvent(ASSISTANT_ATTACH_EVENT, { detail }));
      }
      expect(listener).not.toHaveBeenCalled();
    } finally {
      off();
    }
  });

  it("stops delivering once unsubscribed", () => {
    const listener = vi.fn();
    const off = onAssistantAttachRequest(listener);
    off();
    attachReferenceToAssistant(minted("bot", "bots/revi", "Revi"));
    expect(listener).not.toHaveBeenCalled();
  });
});
