// @vitest-environment jsdom

import { act, renderHook } from "@testing-library/react";
import { beforeEach, describe, expect, it } from "vitest";

import {
  ASSISTANT_ACTION_POLICIES_KEY,
  decideAssistantAction,
  readAssistantActionPolicy,
  useAssistantActionPolicy,
  writeAssistantActionPolicy,
} from "./assistantActions";

beforeEach(() => window.localStorage.clear());

describe("assistant action policies", () => {
  it("asks by default so a new action never silently expands autonomy", () => {
    expect(readAssistantActionPolicy("editor.apply")).toBe("ask");
    expect(readAssistantActionPolicy("editor.save")).toBe("ask");
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
});
