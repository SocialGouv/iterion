import { describe, expect, it } from "vitest";

import {
  canLaunchFromConfirmedDisk,
  isPathWithinWorkDir,
  sourceForLaunch,
} from "./launchSource";

const local = { mode: "local", work_dir: "/workspace/project" } as const;

describe("launch source authority", () => {
  it("omits inline source for a confirmed local disk file", () => {
    expect(
      canLaunchFromConfirmedDisk({
        filePath: "bots/animals/main.bot",
        confirmedDiskPath: "/workspace/project/bots/animals/main.bot",
        serverInfo: local,
      }),
    ).toBe(true);
    expect(
      sourceForLaunch({
        filePath: "bots/animals/main.bot",
        source: "workflow stale:\n  entry: done\n",
        confirmedDiskPath: "/workspace/project/bots/animals/main.bot",
        serverInfo: local,
      }),
    ).toBeUndefined();
  });

  it.each([
    { filePath: "bots/animals/main.bot", confirmedDiskPath: undefined },
    { filePath: "bots/animals/main.bot", confirmedDiskPath: "/workspace/project/bots/other/main.bot" },
    { filePath: "../outside.bot", confirmedDiskPath: "/workspace/outside.bot" },
  ])("keeps inline source when disk authority is uncertain: %j", (input) => {
    expect(
      canLaunchFromConfirmedDisk({
        ...input,
        serverInfo: local,
      }),
    ).toBe(false);
    expect(
      sourceForLaunch({
        ...input,
        source: "workflow inline:\n  entry: done\n",
        serverInfo: local,
      }),
    ).toBe("workflow inline:\n  entry: done\n");
  });

  it("keeps inline source for cloud, unknown, embedded, and buffer launches", () => {
    const input = {
      filePath: "bots/animals/main.bot",
      source: "workflow inline:\n  entry: done\n",
      confirmedDiskPath: "/workspace/project/bots/animals/main.bot",
    };
    expect(sourceForLaunch({ ...input, serverInfo: { mode: "cloud", work_dir: "/workspace/project" } })).toBe(input.source);
    expect(sourceForLaunch({ ...input, serverInfo: null })).toBe(input.source);
    expect(sourceForLaunch({ ...input, confirmedDiskPath: null, serverInfo: local })).toBe(input.source);
    expect(sourceForLaunch({ ...input, filePath: "", serverInfo: local })).toBe(input.source);
  });
});

describe("work directory path boundary", () => {
  it.each([
    ["bots/animals/main.bot", true],
    ["/workspace/project/bots/animals/main.bot", true],
    ["bots\\animals\\main.bot", true],
    ["/workspace/project-other/main.bot", false],
    ["../outside/main.bot", false],
    ["/workspace/project", false],
    [".", false],
  ])("checks %s", (filePath, expected) => {
    expect(isPathWithinWorkDir(filePath, "/workspace/project")).toBe(expected);
  });

  it("fails closed for an unknown or relative work directory", () => {
    expect(isPathWithinWorkDir("/workspace/project/main.bot", undefined)).toBe(false);
    expect(isPathWithinWorkDir("main.bot", "project")).toBe(false);
  });
});
