import { describe, expect, it } from "vitest";

import {
  asFileRef,
  formatJSONValue,
  gateInboundItems,
  isGatePlumbingKey,
  type GateInboundItem,
} from "./gateInbound";

// Asserts the payload produced exactly one item and hands it back —
// a silent empty list would otherwise pass every `item.x` assertion
// below as `undefined`.
function only(items: GateInboundItem[]): GateInboundItem {
  expect(items).toHaveLength(1);
  const [item] = items;
  if (!item) throw new Error("expected one inbound item");
  return item;
}

describe("isGatePlumbingKey", () => {
  it("rejects the reserved underscore family and ask_user_response", () => {
    for (const key of [
      "_queued_operator_messages",
      "_ask_user_options",
      "_ask_user_allow_free_text",
      "_permission",
      "_attachments",
      "ask_user_response",
      "acknowledge_recovery",
    ]) {
      expect(isGatePlumbingKey(key)).toBe(true);
    }
  });

  it("keeps authored mapping keys", () => {
    for (const key of ["plan", "diff", "review_notes", "mockup"]) {
      expect(isGatePlumbingKey(key)).toBe(false);
    }
  });
});

describe("gateInboundItems — plumbing is never shown", () => {
  it("drops reserved keys while keeping the author's payload", () => {
    const items = gateInboundItems(
      {
        plan: "Ship the thing",
        _queued_operator_messages: ["hurry up"],
        _permission: { tool: "Bash" },
        ask_user_response: "Which DB?",
      },
      null,
    );
    expect(items.map((i) => i.key)).toEqual(["plan"]);
  });

  it("drops values that carry nothing rather than rendering empty rows", () => {
    const items = gateInboundItems(
      {
        // `with {}` mappings the engine deliberately keeps as nil — a
        // valid mapping, but no content to review.
        pushback: null,
        previous: "",
        findings: [],
        meta: {},
        notes: "real content",
      },
      null,
    );
    expect(items.map((i) => i.key)).toEqual(["notes"]);
  });

  // The historical workaround — Nexie's `chat_instructions` is literally
  // `{{input.reply}}`, and 12 other catalog gates inline their input the
  // same way. Those values are already on screen as the instructions
  // markdown; repeating them below is the duplication this feature is
  // supposed to REMOVE.
  it("drops keys the instructions prompt already interpolates", () => {
    const items = gateInboundItems(
      { reply: "Nexie's whole answer", findings: { high: 2 } },
      null,
      ["reply"],
    );
    expect(items.map((i) => i.key)).toEqual(["findings"]);
  });

  it("keeps everything when the instructions interpolate nothing", () => {
    for (const consumed of [undefined, null, []]) {
      const items = gateInboundItems({ reply: "a", plan: "b" }, null, consumed);
      expect(items.map((i) => i.key)).toEqual(["plan", "reply"]);
    }
  });

  it("returns nothing for an absent or fully-reserved payload", () => {
    expect(gateInboundItems(undefined, null)).toEqual([]);
    expect(gateInboundItems({}, null)).toEqual([]);
    expect(gateInboundItems({ _permission: { tool: "Bash" } }, null)).toEqual([]);
  });
});

describe("gateInboundItems — kinds", () => {
  it("infers kinds from the value shape when no input schema is declared", () => {
    const items = gateInboundItems(
      {
        summary: "One short line",
        plan: "# Plan\n\nStep one\nStep two",
        findings: [{ id: 1 }],
        counts: { high: 2 },
        approved: true,
        score: 0.8,
      },
      null,
    );
    const byKey = Object.fromEntries(items.map((i) => [i.key, i.kind]));
    expect(byKey).toEqual({
      summary: "scalar",
      plan: "markdown",
      findings: "json",
      counts: "json",
      approved: "scalar",
      score: "scalar",
    });
    // Nothing was declared, so nothing claims to be typed.
    expect(items.every((i) => !i.typed)).toBe(true);
  });

  it("treats a long single-line string as prose, not a chip", () => {
    const long = "x".repeat(200);
    const item = only(gateInboundItems({ summary: long }, null));
    expect(item.kind).toBe("markdown");
  });

  it("honours a declared json field whose value arrived as a JSON string", () => {
    const item = only(
      gateInboundItems({ report: '{"high": 2, "low": 5}' }, [
        { name: "report", type: "json" },
      ]),
    );
    expect(item.kind).toBe("json");
    expect(item.typed).toBe(true);
    expect(item.value).toEqual({ high: 2, low: 5 });
  });

  it("leaves a declared json field that is not JSON as text", () => {
    const item = only(
      gateInboundItems({ report: "not json at all" }, [{ name: "report", type: "json" }]),
    );
    expect(item.kind).toBe("scalar");
    expect(item.value).toBe("not json at all");
  });

  it("orders declared fields first (author's reading order), then the rest alphabetically", () => {
    const items = gateInboundItems(
      { zeta: "z", plan: "p", alpha: "a", summary: "s" },
      [
        { name: "summary", type: "string" },
        { name: "plan", type: "string" },
      ],
    );
    expect(items.map((i) => i.key)).toEqual(["summary", "plan", "alpha", "zeta"]);
  });

  it("skips declared fields the payload does not carry", () => {
    const items = gateInboundItems({ plan: "p" }, [
      { name: "plan", type: "string" },
      { name: "mockup", type: "file" },
    ]);
    expect(items.map((i) => i.key)).toEqual(["plan"]);
  });
});

describe("asFileRef", () => {
  it("reads the descriptor the resume path writes", () => {
    expect(
      asFileRef(
        {
          attachment: "gate.mockup",
          path: "/run/iterion/attachments/gate.mockup/sketch.png",
          filename: "sketch.png",
          mime: "image/png",
          size: 2048,
        },
        false,
      ),
    ).toEqual({
      attachment: "gate.mockup",
      path: "/run/iterion/attachments/gate.mockup/sketch.png",
      filename: "sketch.png",
      mime: "image/png",
      size: 2048,
    });
  });

  it("derives a filename from the path when the descriptor omits it", () => {
    const ref = asFileRef({ attachment: "gate.track", path: "/tmp/a/theme.mp3" }, false);
    expect(ref?.filename).toBe("theme.mp3");
  });

  it("reads a bare path string ONLY when the schema declares the field a file", () => {
    expect(asFileRef("docs/plan.md", true)).toEqual({
      path: "docs/plan.md",
      filename: "plan.md",
    });
    // A prose field is not a path just because it has no spaces.
    expect(asFileRef("docs/plan.md", false)).toBeNull();
  });

  it("does not mistake an ordinary object payload for a file", () => {
    expect(asFileRef({ high: 2, low: 5 }, false)).toBeNull();
    expect(asFileRef([1, 2, 3], false)).toBeNull();
    expect(asFileRef(null, true)).toBeNull();
    expect(asFileRef("   ", true)).toBeNull();
  });

  // A bare `attachment` key is not proof of a descriptor: misreading a
  // structured payload as a file swaps the data the gate exists to show
  // for a 404 banner. Real descriptors always carry corroboration
  // (filename+mime+size+sha256 from the promotion path, path from the
  // engine), so requiring it costs nothing.
  it("requires corroboration before reading an object as a file descriptor", () => {
    expect(
      asFileRef({ attachment: "screenshot.png", note: "the triage item" }, false),
    ).toBeNull();
    // …and such a payload stays visible, as JSON.
    const [item] = gateInboundItems(
      { finding: { attachment: "screenshot.png", note: "the triage item" } },
      null,
    );
    expect(item?.kind).toBe("json");

    // Corroborated by a descriptor field…
    expect(
      asFileRef({ attachment: "gate.shot", mime: "image/png" }, false)?.attachment,
    ).toBe("gate.shot");
    // …or by the declared schema type.
    expect(asFileRef({ attachment: "gate.shot" }, true)?.attachment).toBe("gate.shot");
  });

  it("classifies an upload descriptor as a file item end to end", () => {
    const item = only(
      gateInboundItems({ mockup: { attachment: "gate.mockup", filename: "sketch.png" } }, [
        { name: "mockup", type: "file" },
      ]),
    );
    expect(item.kind).toBe("file");
    expect(item.file?.attachment).toBe("gate.mockup");
  });
});

describe("formatJSONValue", () => {
  it("pretty-prints", () => {
    expect(formatJSONValue({ a: 1 })).toBe('{\n  "a": 1\n}');
  });

  it("never throws on a cyclic value", () => {
    const cyclic: Record<string, unknown> = {};
    cyclic.self = cyclic;
    expect(() => formatJSONValue(cyclic)).not.toThrow();
  });
});
