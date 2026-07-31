import { describe, expect, it } from "vitest";

import { isReservedQuestionKey } from "@/lib/askUserOptions";

import { REVIEW_MEDIA_QUESTION_KEY, reviewMediaFromQuestions } from "./ReviewMedia";

describe("reviewMediaFromQuestions", () => {
  it("reads the attached captures in order", () => {
    expect(
      reviewMediaFromQuestions({
        [REVIEW_MEDIA_QUESTION_KEY]: [
          { path: "human-review/a1/01-overview.png", caption: "Overview" },
          { path: "human-review/a1/02-capture-01.png" },
        ],
      }),
    ).toEqual([
      { path: "human-review/a1/01-overview.png", caption: "Overview" },
      { path: "human-review/a1/02-capture-01.png" },
    ]);
  });

  it("returns nothing when the node attached no media", () => {
    expect(reviewMediaFromQuestions({})).toEqual([]);
    expect(reviewMediaFromQuestions(undefined)).toEqual([]);
  });

  // `questions` is bot-authored, so a malformed entry must not take down the
  // block the operator needs to answer — the well-formed ones still render.
  it("drops malformed entries instead of throwing", () => {
    expect(
      reviewMediaFromQuestions({
        [REVIEW_MEDIA_QUESTION_KEY]: [
          null,
          "human-review/a1/loose.png",
          { caption: "no path" },
          { path: 42 },
          { path: "" },
          { path: "human-review/a1/good.png", caption: 7 },
        ],
      }),
    ).toEqual([{ path: "human-review/a1/good.png" }]);
  });

  it("ignores a non-array payload", () => {
    expect(reviewMediaFromQuestions({ [REVIEW_MEDIA_QUESTION_KEY]: "nope" })).toEqual([]);
  });

  // The captures are evidence to look at, never a field to fill in.
  it("is reserved, so no form renders it as an answerable input", () => {
    expect(isReservedQuestionKey(REVIEW_MEDIA_QUESTION_KEY)).toBe(true);
  });
});
