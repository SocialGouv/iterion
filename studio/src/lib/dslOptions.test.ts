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

  it("HUMAN_INTERACTION_OPTIONS is a non-empty subset of the registry interaction enum", () => {
    const human = values(HUMAN_INTERACTION_OPTIONS);
    expect(human.length).toBeGreaterThan(0);
    for (const v of human) {
      expect(iterDslEnumValuesByProperty.interaction, v).toContain(v);
    }
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
