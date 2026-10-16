import { describe, expect, it } from "vitest";

import type { InstallBotResult } from "@/api/bots";

import { importSuccessMessage } from "./importActions";

const imported = (extra: Partial<InstallBotResult> = {}): InstallBotResult => ({
  name: "mybot",
  source: "upload",
  installed_path: "/ws/.botz/mybot",
  skills: 1,
  presets: 2,
  ...extra,
});

const regenFailure =
  "botregistry: read catalog template /ws/bots/nexie/iterion-bot-catalog-static.md: too many levels of symbolic links";

describe("importSuccessMessage", () => {
  it("reports the stale catalog next to the import that succeeded", () => {
    const message = importSuccessMessage(imported({ catalog_error: regenFailure }));
    expect(message).toContain("Imported mybot");
    expect(message).toContain("/ws/bots/nexie");
    expect(message).toContain("iterion bots regen-catalog");
  });

  it("says nothing about the catalog when the refresh succeeded", () => {
    expect(importSuccessMessage(imported())).not.toContain("catalog");
  });
});
