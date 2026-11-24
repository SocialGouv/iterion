// How a registry bot is launched is one decision (botLaunch), read by every
// affordance that offers it: in cloud by slug — whatever tier serves the bot
// — and locally by its workspace file (#3416).
import { describe, expect, it } from "vitest";

import type { BotEntryWithSchema } from "@/api/bots";

import { botLaunch, botLaunchFormRefusal, botLaunchHref } from "./botPaths";

const stored: BotEntryWithSchema = { name: "nestor", path: "", origin: "tenant", is_bundle: true };
const bundle: BotEntryWithSchema = {
  name: "feed-watch",
  path: "/w/bots/feed-watch",
  rel_path: "bots/feed-watch",
  is_bundle: true,
};
const loose: BotEntryWithSchema = { name: "smoke", path: "/w/bots/smoke.bot", rel_path: "bots/smoke.bot" };

describe("botLaunch", () => {
  it("launches every listed bot by slug in cloud, with or without a workspace path", () => {
    expect(botLaunch(stored, true)).toEqual({ by: "bot", bot: "nestor" });
    expect(botLaunch(bundle, true)).toEqual({ by: "bot", bot: "feed-watch" });
    expect(botLaunch(loose, true)).toEqual({ by: "bot", bot: "smoke" });
  });

  it("launches by the workspace file locally, and not at all without one", () => {
    expect(botLaunch(bundle, false)).toEqual({ by: "file", file: "bots/feed-watch/main.bot" });
    expect(botLaunch(loose, false)).toEqual({ by: "file", file: "bots/smoke.bot" });
    expect(botLaunch(stored, false)).toBeNull();
  });
});

describe("botLaunchHref", () => {
  it("is the launch form's URL for either way", () => {
    expect(botLaunchHref({ by: "bot", bot: "feed watch" })).toBe("/runs/new?bot=feed%20watch");
    expect(botLaunchHref({ by: "file", file: "bots/x/main.bot" })).toBe(
      "/runs/new?file=bots%2Fx%2Fmain.bot",
    );
  });
});

// The launch FORM shows the bot's program, which the server serves only to a
// caller another route already serves it to. The studio reads that rule off
// the entry's origin and the session, so an affordance never leads to a form
// the server refuses (rva round 1, S3).
describe("botLaunchFormRefusal", () => {
  const platform: BotEntryWithSchema = { name: "revi", path: "", origin: "platform", is_bundle: true };
  const baked: BotEntryWithSchema = { ...bundle, origin: "catalog" };
  const nobody = { editsTeamBots: false };
  const editor = { editsTeamBots: true };

  it("holds a team's own bot to those who may edit the team's bots", () => {
    expect(botLaunchFormRefusal(stored, true, nobody)).toMatch(/a team's own bot is readable by a bot editor, a team admin or an owner/);
    expect(botLaunchFormRefusal(stored, true, editor)).toBeNull();
  });

  // A platform override is what the platform serves every tenant, as the
  // baked catalog is (#3416): its form opens for every signed-in member.
  it("opens a platform override's form to everyone, as a baked bot's", () => {
    expect(botLaunchFormRefusal(platform, true, nobody)).toBeNull();
    expect(botLaunchFormRefusal(platform, true, editor)).toBeNull();
  });

  it("opens the baked catalog's form to everyone", () => {
    expect(botLaunchFormRefusal(baked, true, nobody)).toBeNull();
    // An entry that names no origin is one the studio knows nothing of: the
    // server's answer is the only one that can refuse it.
    expect(botLaunchFormRefusal(bundle, true, nobody)).toBeNull();
  });

  it("says how the bot can still be launched, by its own id", () => {
    const refusal = botLaunchFormRefusal(stored, true, nobody) ?? "";
    expect(refusal).toContain("`iterion remote runs launch --bot nestor`");
    expect(refusal).toMatch(/bot_id through the API or MCP/);
  });

  it("refuses nothing locally, where the form reads the workspace's own file", () => {
    expect(botLaunchFormRefusal(stored, false, nobody)).toBeNull();
    expect(botLaunchFormRefusal(platform, false, nobody)).toBeNull();
  });
});
