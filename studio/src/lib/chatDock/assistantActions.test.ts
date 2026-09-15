// @vitest-environment jsdom

import { act, renderHook } from "@testing-library/react";
import { beforeEach, describe, expect, it } from "vitest";

import {
  ASSISTANT_ACTION_POLICIES_KEY,
  decideAssistantAction,
  parseAssistantActionRequestsDetailed,
  parseAssistantActionRequests,
  readAssistantActionPolicy,
  useAssistantActionPolicy,
  writeAssistantActionPolicy,
} from "./assistantActions";

beforeEach(() => window.localStorage.clear());

describe("assistant action policies", () => {
  it("asks by default so a new action never silently expands autonomy", () => {
    expect(readAssistantActionPolicy("editor.apply")).toBe("ask");
    expect(readAssistantActionPolicy("editor.save")).toBe("ask");
    expect(readAssistantActionPolicy("run.rewind")).toBe("ask");
    expect(readAssistantActionPolicy("dependency.bots.update")).toBe("ask");
    expect(readAssistantActionPolicy("dependency.bots.localize")).toBe("ask");
    expect(readAssistantActionPolicy("workspace.handoff.complete")).toBe("allow");
  });

  it("persists each action independently and rejects corrupt values", () => {
    writeAssistantActionPolicy("editor.apply", "allow");
    expect(readAssistantActionPolicy("editor.apply")).toBe("allow");
    expect(readAssistantActionPolicy("editor.save")).toBe("ask");

    window.localStorage.setItem(
      ASSISTANT_ACTION_POLICIES_KEY,
      JSON.stringify({ "editor.apply": "surprise" }),
    );
    expect(readAssistantActionPolicy("editor.apply")).toBe("ask");
  });

  it("turns policy plus explicit consent into a host decision", () => {
    expect(decideAssistantAction("deny", true)).toBe("deny");
    expect(decideAssistantAction("ask", true)).toBe("confirm");
    expect(decideAssistantAction("explicit", false)).toBe("confirm");
    expect(decideAssistantAction("explicit", true)).toBe("auto");
    expect(decideAssistantAction("allow", false)).toBe("auto");
  });

  it("updates mounted consumers in the same browser tab", () => {
    const { result } = renderHook(() =>
      useAssistantActionPolicy("editor.apply"),
    );
    expect(result.current).toBe("ask");
    act(() => writeAssistantActionPolicy("editor.apply", "explicit"));
    expect(result.current).toBe("explicit");
  });

  it("accepts only host-known action ids and bounds one turn", () => {
    const requests = parseAssistantActionRequests(
      [
        {
          id: "board.issue.transition",
          intent: "explicit",
          args: { issue_id: "abc", to: "ready" },
        },
        { id: "host.shell", intent: "explicit", args: { command: "no" } },
      ],
      "run:node:1",
    );
    expect(requests).toEqual([
      {
        key: "run:node:1:0",
        id: "board.issue.transition",
        intent: "explicit",
        args: { issue_id: "abc", to: "ready" },
      },
    ]);
  });

  it("accepts run.rewind from the closed action catalogue", () => {
    expect(
      parseAssistantActionRequests(
        [
          {
            id: "run.rewind",
            intent: "explicit",
            args: { run_id: "run-1", auto: true },
          },
        ],
        "run:node:2",
      ),
    ).toEqual([
      {
        key: "run:node:2:0",
        id: "run.rewind",
        intent: "explicit",
        args: { run_id: "run-1", auto: true },
      },
    ]);
  });

  it("reports every malformed entry while preserving valid wire order", () => {
    const parsed = parseAssistantActionRequestsDetailed(
      [
        { id: "run.resume", intent: "explicit", args: { run_id: "r" } },
        { id: "run.Resume", intent: "explicit" },
        { id: "run.watch", intent: "suggested", args: { run_id: "r" } },
        { id: "run.pause", intent: "maybe" },
      ],
      "run:node:3",
    );
    expect(parsed.requests.map(({ id }) => id)).toEqual([
      "run.resume",
      "run.watch",
    ]);
    expect(parsed.rejected).toEqual([
      { index: 1, id: "run.Resume", reason: "unknown_action_id" },
      { index: 3, id: "run.pause", reason: "invalid_intent" },
    ]);
  });

  it("rejects non-arrays and bounds oversized lists without aliases", () => {
    expect(
      parseAssistantActionRequestsDetailed(
        '{"id":"run.resume"}',
        "run:node:4",
      ).rejected,
    ).toEqual([{ index: 0, id: "object", reason: "actions_not_array" }]);

    const parsed = parseAssistantActionRequestsDetailed(
      Array.from({ length: 9 }, () => ({ id: "run.resume" })),
      "run:node:5",
    );
    expect(parsed.requests).toHaveLength(8);
    expect(parsed.rejected).toContainEqual({
      index: 8,
      id: "array",
      reason: "too_many_actions",
    });
  });
});
