import { describe, expect, it } from "vitest";

import type { InstallBotResult } from "@/api/bots";

import { installedMessage, uninstalledMessage, uploadedMessage } from "./toastMessages";

const cause =
  "botregistry: read catalog template /ws/bots/nexie/iterion-bot-catalog-static.md: too many levels of symbolic links";

function installResult(over: Partial<InstallBotResult> = {}): InstallBotResult {
  return {
    name: "shared-planner",
    source: "https://example.test/bots.git",
    installed_path: "/ws/.botz/shared-planner",
    skills: 0,
    presets: 0,
    ...over,
  };
}

describe("installedMessage", () => {
  it("confirms the install and where the bundle landed", () => {
    const msg = installedMessage(installResult(), false);
    expect(msg).toBe("Installed shared-planner → /ws/.botz/shared-planner");
  });

  it("says Updated on a forced install", () => {
    expect(installedMessage(installResult(), true)).toContain("Updated shared-planner");
  });

  it("carries the stale catalogue the install left behind", () => {
    const msg = installedMessage(installResult({ catalog_error: cause }), false);
    expect(msg).toContain(cause);
    expect(msg).toContain("iterion bots regen-catalog");
  });
});

describe("uninstalledMessage", () => {
  it("confirms the removal", () => {
    expect(uninstalledMessage("shared-planner", {})).toBe("Uninstalled shared-planner");
  });

  it("carries the stale catalogue the removal left behind", () => {
    const msg = uninstalledMessage("shared-planner", { catalog_error: cause });
    expect(msg).toContain(cause);
    expect(msg).toContain("iterion bots regen-catalog");
  });
});

describe("uploadedMessage", () => {
  it("confirms the upload and where the bundle landed", () => {
    expect(uploadedMessage(installResult({ name: "packed" }))).toBe(
      "Imported packed → /ws/.botz/shared-planner",
    );
  });

  it("carries the stale catalogue the upload left behind", () => {
    const msg = uploadedMessage(installResult({ catalog_error: cause }));
    expect(msg).toContain(cause);
    expect(msg).toContain("iterion bots regen-catalog");
  });
});
