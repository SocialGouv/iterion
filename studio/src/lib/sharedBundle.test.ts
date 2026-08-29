import { describe, expect, it } from "vitest";

import {
  isSharedBundleFilePath,
  parseSharedBundleFilePath,
} from "./sharedBundle";

describe("shared bundle editor paths", () => {
  it("recognises materialised dependency files", () => {
    expect(parseSharedBundleFilePath(".botz/shared-planner/main.bot")).toEqual({
      name: "shared-planner",
      relativePath: "main.bot",
    });
    expect(
      isSharedBundleFilePath("./.botz/shared-planner/workflows/child.bot"),
    ).toBe(true);
  });

  it("does not mark ordinary workspace bots read-only", () => {
    expect(isSharedBundleFilePath("bots/shared-planner/main.bot")).toBe(false);
    expect(isSharedBundleFilePath("nested/.botz/shared-planner/main.bot")).toBe(
      false,
    );
    expect(isSharedBundleFilePath(null)).toBe(false);
  });
});
