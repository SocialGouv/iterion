import { describe, expect, it } from "vitest";

import type { BotEntry } from "@/api/bots";
import type { MarketplaceEntry } from "@/api/marketplace";
import type { PluginView } from "@/api/plugins";
import {
  buildInstalledPluginVersions,
  buildInstalledVersions,
  compareVersions,
  resolveInstalledState,
} from "./installState";

function entry(over: Partial<MarketplaceEntry>): MarketplaceEntry {
  return {
    slug: "acme-thing",
    name: "thing",
    repo_url: "https://example.com/acme/thing",
    installs: 0,
    ...over,
  };
}

function bot(name: string, version?: string): BotEntry {
  return { name, path: `/bots/${name}`, version } as BotEntry;
}

function plugin(name: string, version?: string): PluginView {
  return { name, version, enabled: false, builtin: false, kinds: ["rewriter"] };
}

describe("resolveInstalledState (bot entries)", () => {
  const installed = buildInstalledVersions([bot("thing", "1.2.0"), bot("other", undefined)]);

  it("is absent when the bot isn't in the workspace", () => {
    expect(resolveInstalledState(entry({ name: "missing" }), installed)).toBe("absent");
  });

  it("is installed at the same version", () => {
    expect(resolveInstalledState(entry({ version: "1.2.0" }), installed)).toBe("installed");
  });

  it("is update when the registry version is strictly newer", () => {
    expect(resolveInstalledState(entry({ version: "1.3.0" }), installed)).toBe("update");
  });

  it("stays installed when a version is missing on either side", () => {
    expect(resolveInstalledState(entry({}), installed)).toBe("installed");
    expect(
      resolveInstalledState(entry({ name: "other", version: "9.9.9" }), installed),
    ).toBe("installed");
  });

  it("never matches a bot entry against an installed plugin of the same name", () => {
    const plugins = buildInstalledPluginVersions([plugin("thing", "1.2.0")]);
    expect(resolveInstalledState(entry({}), new Map(), plugins)).toBe("absent");
  });
});

describe("resolveInstalledState (plugin entries)", () => {
  const bots = buildInstalledVersions([bot("thing", "1.2.0")]);
  const plugins = buildInstalledPluginVersions([plugin("rtk", "0.4.0"), plugin("bare")]);

  it("matches plugin entries against installed plugins by name", () => {
    expect(
      resolveInstalledState(entry({ name: "rtk", kind: "plugin", version: "0.4.0" }), bots, plugins),
    ).toBe("installed");
  });

  it("is absent when the plugin isn't installed", () => {
    expect(
      resolveInstalledState(entry({ name: "missing", kind: "plugin" }), bots, plugins),
    ).toBe("absent");
  });

  it("never matches a plugin entry against an installed bot of the same name", () => {
    expect(
      resolveInstalledState(entry({ name: "thing", kind: "plugin" }), bots, plugins),
    ).toBe("absent");
  });

  it("is update when the registry plugin version is strictly newer", () => {
    expect(
      resolveInstalledState(entry({ name: "rtk", kind: "plugin", version: "0.5.0" }), bots, plugins),
    ).toBe("update");
  });

  it("stays installed on an uncomparable version", () => {
    expect(
      resolveInstalledState(entry({ name: "bare", kind: "plugin", version: "1.0.0" }), bots, plugins),
    ).toBe("installed");
  });

  it("defaults the plugins index to empty when omitted", () => {
    expect(resolveInstalledState(entry({ name: "rtk", kind: "plugin" }), bots)).toBe("absent");
  });
});

describe("compareVersions", () => {
  it("compares dotted numerics, not lexicographically", () => {
    expect(compareVersions("1.2.10", "1.2.9")).toBe(1);
    expect(compareVersions("1.2.9", "1.2.10")).toBe(-1);
  });

  it("ignores a leading v and pads missing segments with zero", () => {
    expect(compareVersions("v1.2", "1.2.0")).toBe(0);
    expect(compareVersions("1.2.1", "1.2")).toBe(1);
  });
});
