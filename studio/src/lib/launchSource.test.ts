import { describe, expect, it } from "vitest";

import {
  canLaunchFromConfirmedDisk,
  isPathWithinWorkDir,
  launchCarriesProgram,
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

describe("a launch that names its program", () => {
  const cloud = { mode: "cloud", work_dir: "" } as const;

  it("reads a path as a program only where the server can read one", () => {
    const path = "bots/animals/main.bot";
    // Local, and unknown-yet mode: the server reads the file itself, so the
    // path alone names the program — refusing it would refuse the legitimate
    // confirmed-disk launch, which sends no source at all.
    expect(launchCarriesProgram({ filePath: path, source: null, serverInfo: local })).toBe(true);
    expect(launchCarriesProgram({ filePath: path, source: null })).toBe(true);
    expect(launchCarriesProgram({ filePath: path, source: null, serverInfo: null })).toBe(true);
    // Cloud: the pod has no operator filesystem. A bare path there is either
    // refused by name or resolved to a catalog bot's OWN bytes — neither is
    // the program whose fields this form showed.
    expect(launchCarriesProgram({ filePath: path, source: null, serverInfo: cloud })).toBe(false);
    expect(launchCarriesProgram({ filePath: path, source: "", serverInfo: cloud })).toBe(false);
  });

  it("reads the inline render as a program in every mode", () => {
    const source = "workflow inline:\n  entry: done\n";
    expect(launchCarriesProgram({ filePath: "", source, serverInfo: cloud })).toBe(true);
    expect(launchCarriesProgram({ filePath: "", source, serverInfo: local })).toBe(true);
    // A cloud ?file= launch always carries its source (sourceForLaunch omits
    // it only for a confirmed LOCAL disk file), so the path never costs it
    // the gate.
    expect(
      launchCarriesProgram({ filePath: "bots/animals/main.bot", source, serverInfo: cloud }),
    ).toBe(true);
  });

  it("refuses a launch that names nothing at all", () => {
    expect(launchCarriesProgram({ filePath: "", source: null, serverInfo: local })).toBe(false);
    expect(launchCarriesProgram({ filePath: "", source: "", serverInfo: null })).toBe(false);
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
