import { describe, expect, it } from "vitest";
import { iterDslEnumValuesByProperty } from "./iterDsl.generated";
import {
  AWAIT_OPTIONS,
  HUMAN_INTERACTION_OPTIONS,
  INTERACTION_OPTIONS,
  REASONING_EFFORT_OPTIONS,
  SESSION_OPTIONS,
} from "./dslOptions";

// dslOptions.ts derives the value sets of its registry-enum-backed groups
// from iterDslEnumValuesByProperty (regenerated from pkg/dsl/spec by
// `task dsl:gen`, gated by `task dsl:check`). These assertions are the
// drift net: a registry word added, renamed or removed must redden here
// instead of silently desyncing the canvas dropdowns — the failure that
// #1888 reported (SESSION_OPTIONS missing inherit_if_available/persist,
// INTERACTION_OPTIONS missing review/async/human_or_host, AWAIT_OPTIONS
// offering a `none` the parser refuses).

const values = (options: { value: string }[]) => options.map((o) => o.value);
const sorted = (xs: readonly string[]) => [...xs].sort();

describe("dslOptions registry-derived groups", () => {
  it("SESSION_OPTIONS carries exactly the registry session enum", () => {
    expect(sorted(values(SESSION_OPTIONS))).toEqual(sorted(iterDslEnumValuesByProperty.session));
  });

  it("INTERACTION_OPTIONS carries exactly the registry interaction enum", () => {
    expect(sorted(values(INTERACTION_OPTIONS))).toEqual(sorted(iterDslEnumValuesByProperty.interaction));
  });

  it("AWAIT_OPTIONS carries exactly the registry await enum — and no `none`", () => {
    expect(sorted(values(AWAIT_OPTIONS))).toEqual(sorted(iterDslEnumValuesByProperty.await));
    expect(values(AWAIT_OPTIONS)).not.toContain("none");
  });

  it("REASONING_EFFORT_OPTIONS carries exactly the registry enum behind its (default) sentinel", () => {
    expect(sorted(values(REASONING_EFFORT_OPTIONS).filter((v) => v !== ""))).toEqual(
      sorted(iterDslEnumValuesByProperty.reasoning_effort),
    );
  });

  it("HUMAN_INTERACTION_OPTIONS is exactly the curated human-node subset", () => {
    // Only `async` is refused on a human node (C240); review/human_or_host
    // are omitted pending form support. Pinning the exact set reddens a
    // careless edit — e.g. adding async, which the compiler would refuse.
    expect(sorted(values(HUMAN_INTERACTION_OPTIONS))).toEqual(["human", "llm", "llm_or_human"]);
    for (const v of values(HUMAN_INTERACTION_OPTIONS)) {
      expect(iterDslEnumValuesByProperty.interaction, v).toContain(v);
    }
  });
});

describe("dslOptions curated orders", () => {
  it("SESSION_OPTIONS keeps fresh first and covers every registry word in place", () => {
    // Order-sensitive: a new registry word appends sorted at the end and
    // reddens here until the curation names its place.
    expect(values(SESSION_OPTIONS)).toEqual([
      "fresh",
      "inherit",
      "inherit_if_available",
      "fork",
      "artifacts_only",
      "persist",
    ]);
  });

  it("REASONING_EFFORT_OPTIONS reads ascending behind its (default) sentinel, no-reasoning first", () => {
    expect(values(REASONING_EFFORT_OPTIONS)).toEqual(["", "none", "low", "medium", "high", "xhigh", "max", "ultracode"]);
  });
});

describe("dslOptions curated labels", () => {
  // Labels that describe BEHAVIOUR are pinned: a swap between two of them
  // would actively mislead (pause semantics, orchestration consent), while
  // the set-equality tests above stay green.
  const labelOf = (options: { value: string; label: string }[], value: string) =>
    options.find((o) => o.value === value)?.label;

  it("pins the reasoning-effort labels that name a different tier", () => {
    expect(labelOf(REASONING_EFFORT_OPTIONS, "")).toBe("(default)");
    expect(labelOf(REASONING_EFFORT_OPTIONS, "ultracode")).toBe("ultracode (xhigh + orchestration)");
  });

  it("pins the human-node interaction labels that describe pause behaviour", () => {
    expect(labelOf(HUMAN_INTERACTION_OPTIONS, "human")).toBe("human (always pause)");
    expect(labelOf(HUMAN_INTERACTION_OPTIONS, "llm")).toBe("llm (auto-answer)");
    expect(labelOf(HUMAN_INTERACTION_OPTIONS, "llm_or_human")).toBe("llm_or_human (escalation)");
    expect(labelOf(INTERACTION_OPTIONS, "llm_or_human")).toBe("llm_or_human (escalation)");
  });

  it("labels are present and non-empty on every derived option", () => {
    for (const group of [
      AWAIT_OPTIONS,
      SESSION_OPTIONS,
      INTERACTION_OPTIONS,
      HUMAN_INTERACTION_OPTIONS,
      REASONING_EFFORT_OPTIONS,
    ]) {
      for (const option of group) {
        expect(option.label.trim(), `label of ${JSON.stringify(option.value)}`).not.toBe("");
      }
    }
  });
});
