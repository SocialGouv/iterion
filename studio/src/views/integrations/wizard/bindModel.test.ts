import { describe, expect, it } from "vitest";

import type { ForgeEnablePreview } from "@/api/forgeConnections";

import {
  bindBotPath,
  buildBindPreviewModel,
  firstIncompleteBindStep,
  prevBindStep,
  resolveBindStep,
  sanitizeReturnTo,
  unionBotIds,
} from "./bindModel";

describe("resolveBindStep", () => {
  it("starts at the repo picker with no prefills", () => {
    expect(resolveBindStep({})).toBe("repo");
  });

  it("skips the repo step when ?repo= is present", () => {
    expect(resolveBindStep({ repo: "conn::o/r" })).toBe("bot");
  });

  it("skips the bot step when ?bot= is present (bot-page entry)", () => {
    expect(resolveBindStep({ bot: "revi" })).toBe("repo");
  });

  it("lands on review when both prefills are present", () => {
    expect(resolveBindStep({ repo: "conn::o/r", bot: "revi" })).toBe("review");
  });

  it("honours an explicit valid ?step=", () => {
    expect(
      resolveBindStep({ step: "bot", repo: "conn::o/r", bot: "revi" }),
    ).toBe("bot");
  });

  it("ignores a garbage ?step=", () => {
    expect(resolveBindStep({ step: "nope", repo: "conn::o/r" })).toBe("bot");
  });

  it("degrades a forward step whose prerequisites are missing", () => {
    expect(resolveBindStep({ step: "review", repo: "conn::o/r" })).toBe("bot");
    expect(resolveBindStep({ step: "review" })).toBe("repo");
    expect(resolveBindStep({ step: "done", bot: "revi" })).toBe("repo");
    expect(resolveBindStep({ step: "bot" })).toBe("repo");
  });

  it("treats whitespace-only params as absent", () => {
    expect(resolveBindStep({ repo: "  ", bot: "" })).toBe("repo");
  });
});

describe("firstIncompleteBindStep", () => {
  it("walks repo → bot → review", () => {
    expect(firstIncompleteBindStep(false, false)).toBe("repo");
    expect(firstIncompleteBindStep(true, false)).toBe("bot");
    expect(firstIncompleteBindStep(false, true)).toBe("repo");
    expect(firstIncompleteBindStep(true, true)).toBe("review");
  });
});

describe("prevBindStep", () => {
  const none = { repo: false, bot: false };

  it("goes back through every non-prefilled step", () => {
    expect(prevBindStep("review", none)).toBe("bot");
    expect(prevBindStep("bot", none)).toBe("repo");
    expect(prevBindStep("repo", none)).toBeNull();
  });

  it("skips a bot arrival prefill (bot-page entry goes review → repo)", () => {
    expect(prevBindStep("review", { repo: false, bot: true })).toBe("repo");
  });

  it("skips a repo arrival prefill (repo-page entry goes review → bot)", () => {
    expect(prevBindStep("review", { repo: true, bot: false })).toBe("bot");
    expect(prevBindStep("bot", { repo: true, bot: false })).toBeNull();
  });

  it("has nothing to go back to when both were prefilled", () => {
    expect(prevBindStep("review", { repo: true, bot: true })).toBeNull();
  });
});

describe("sanitizeReturnTo", () => {
  it("accepts in-app paths", () => {
    expect(sanitizeReturnTo("/bots/revi")).toBe("/bots/revi");
    expect(sanitizeReturnTo("/repos/x%3A%3Ao%2Fr")).toBe("/repos/x%3A%3Ao%2Fr");
  });

  it("rejects absolute and protocol-relative URLs", () => {
    expect(sanitizeReturnTo("https://evil.example")).toBeNull();
    expect(sanitizeReturnTo("//evil.example")).toBeNull();
  });

  it("rejects empty / missing values", () => {
    expect(sanitizeReturnTo("")).toBeNull();
    expect(sanitizeReturnTo(null)).toBeNull();
    expect(sanitizeReturnTo(undefined)).toBeNull();
  });
});

describe("unionBotIds", () => {
  it("appends a new bot, keeps order", () => {
    expect(unionBotIds(["a", "b"], "c")).toEqual(["a", "b", "c"]);
  });
  it("dedupes an already-bound bot", () => {
    expect(unionBotIds(["a", "b"], "b")).toEqual(["a", "b"]);
  });
  it("ignores an empty next", () => {
    expect(unionBotIds(["a"], "")).toEqual(["a"]);
  });
});

describe("bindBotPath", () => {
  it("builds the prefilled URL", () => {
    expect(bindBotPath({ bot: "revi", returnTo: "/bots/revi" })).toBe(
      "/integrations/bind?bot=revi&returnTo=%2Fbots%2Frevi",
    );
  });
  it("encodes the repo key's :: and /", () => {
    expect(bindBotPath({ repoKey: "conn::o/r" })).toBe(
      "/integrations/bind?repo=conn%3A%3Ao%2Fr",
    );
  });
  it("is bare with no prefills", () => {
    expect(bindBotPath({})).toBe("/integrations/bind");
  });
});

/* ------------------------- preview view-model ------------------------ */

function fixture(overrides: Partial<ForgeEnablePreview> = {}): ForgeEnablePreview {
  return {
    events_normalized: ["merge_request", "note"],
    forge_native_events: ["pull_request", "issue_comment"],
    scopes: { repo: "read/write code and PRs" },
    secrets: [{ bot_id: "revi", secret: "REVI_TOKEN" }],
    commands: [{ command: "revi", bot_id: "revi" }],
    identity: { handle: "iterion-bot", provider: "github", base_url: "https://github.com" },
    conflicts: [],
    ...overrides,
  };
}

describe("buildBindPreviewModel", () => {
  it("returns null without a preview", () => {
    expect(buildBindPreviewModel(null, [])).toBeNull();
    expect(buildBindPreviewModel(undefined, [])).toBeNull();
  });

  it("happy path: native events, sorted scopes, identity, no conflicts", () => {
    const m = buildBindPreviewModel(fixture(), ["REVI_TOKEN"]);
    expect(m).not.toBeNull();
    expect(m!.events).toEqual(["pull_request", "issue_comment"]);
    expect(m!.scopes).toEqual([{ scope: "repo", reason: "read/write code and PRs" }]);
    expect(m!.commands).toEqual([{ command: "revi", botId: "revi" }]);
    expect(m!.secrets).toEqual([
      { botId: "revi", secret: "REVI_TOKEN", missing: false },
    ]);
    expect(m!.hasMissingSecrets).toBe(false);
    expect(m!.identity).toEqual({
      handle: "iterion-bot",
      provider: "github",
      baseUrl: "https://github.com",
    });
    expect(m!.hasConflicts).toBe(false);
  });

  it("falls back to normalized events when native ones are absent", () => {
    const m = buildBindPreviewModel(fixture({ forge_native_events: [] }), []);
    expect(m!.events).toEqual(["merge_request", "note"]);
  });

  it("flags a secret the team doesn't have", () => {
    const m = buildBindPreviewModel(fixture(), ["OTHER"]);
    expect(m!.secrets[0]?.missing).toBe(true);
    expect(m!.hasMissingSecrets).toBe(true);
  });

  it("never flags missing secrets when the secret list is unknown", () => {
    const m = buildBindPreviewModel(fixture(), null);
    expect(m!.secrets[0]?.missing).toBe(false);
    expect(m!.hasMissingSecrets).toBe(false);
  });

  it("surfaces conflicts", () => {
    const m = buildBindPreviewModel(
      fixture({ conflicts: ["billy has no forge: block and no invocation"] }),
      [],
    );
    expect(m!.hasConflicts).toBe(true);
    expect(m!.conflicts).toEqual(["billy has no forge: block and no invocation"]);
  });

  it("degrades gracefully on absent fields (older servers)", () => {
    const sparse = {
      events_normalized: undefined,
      forge_native_events: undefined,
      scopes: undefined,
      secrets: undefined,
      commands: undefined,
      identity: undefined,
      conflicts: undefined,
    } as unknown as ForgeEnablePreview;
    const m = buildBindPreviewModel(sparse, []);
    expect(m).not.toBeNull();
    expect(m!.events).toEqual([]);
    expect(m!.scopes).toEqual([]);
    expect(m!.commands).toEqual([]);
    expect(m!.secrets).toEqual([]);
    expect(m!.identity).toBeNull();
    expect(m!.conflicts).toEqual([]);
    expect(m!.hasConflicts).toBe(false);
  });
});
