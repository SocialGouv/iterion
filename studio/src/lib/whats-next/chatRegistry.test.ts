import { describe, expect, it } from "vitest";

import type { BotEntry } from "@/api/bots";

import { chatBotFromEntry, chatRegistryFrom, isChatBot } from "./chatRegistry";

function entry(over: Partial<BotEntry> = {}): BotEntry {
  return {
    name: "copilot",
    display_name: "Copi",
    description: "Knows iterion itself.",
    path: "/abs/bots/copilot",
    rel_path: "bots/copilot",
    chat: {
      seed_var: "initial_message",
      nodes: {
        seed: { kind: "silent" },
        copi: { kind: "banner", label: "Copi is thinking" },
        chat: { kind: "human", text_field: "message" },
      },
    },
    ...over,
  } as BotEntry;
}

describe("isChatBot", () => {
  it("recognises a bot that declares a usable chat surface", () => {
    expect(isChatBot(entry())).toBe(true);
  });

  it("is false for an ordinary bot", () => {
    expect(isChatBot(entry({ chat: undefined }))).toBe(false);
  });

  it("is false for a chat block with no human turn", () => {
    // The Go loader rejects this, so it can only arrive from a server on a
    // different build. The failure it prevents is silent: a chat window that
    // accepts messages and can never be answered.
    expect(
      isChatBot(entry({ chat: { nodes: { a: { kind: "banner" } } } })),
    ).toBe(false);
  });
});

describe("chatBotFromEntry", () => {
  it("maps the manifest shape onto the surface the studio already renders", () => {
    const bot = chatBotFromEntry(entry())!;
    expect(bot.id).toBe("copilot");
    expect(bot.label).toBe("Copi");
    expect(bot.seedVar).toBe("initial_message");
    expect(bot.nodeMap.chat).toEqual({ kind: "human", textField: "message" });
    expect(bot.nodeMap.copi).toEqual({ kind: "banner", label: "Copi is thinking" });
  });

  it("resolves the workflow path a launch can actually use", () => {
    // rel_path is workspace-relative (what the run API resolves); the
    // absolute path is the fallback for a server that computed no root.
    expect(chatBotFromEntry(entry())!.workflowPath).toBe("bots/copilot/main.bot");
    expect(
      chatBotFromEntry(entry({ rel_path: undefined }))!.workflowPath,
    ).toBe("/abs/bots/copilot/main.bot");
    // A loose .bot file is already the workflow.
    expect(
      chatBotFromEntry(entry({ rel_path: "bots/x/main.bot" }))!.workflowPath,
    ).toBe("bots/x/main.bot");
  });

  it("builds the canned-opener form, keeping the free-text escape hatch", () => {
    const bot = chatBotFromEntry(
      entry({
        chat: {
          seed_var: "initial_message",
          nodes: { chat: { kind: "human", text_field: "message" } },
          launcher: {
            prompt: "What do you want to ask?",
            presets: [{ value: "Explique C083", label: "A diagnostic" }],
          },
        },
      }),
    )!;
    const q = bot.launcherForm!.questions[0];
    expect(q.label).toBe("What do you want to ask?");
    expect(q.options).toEqual([
      { value: "Explique C083", label: "A diagnostic" },
    ]);
    // Defaulting this to false would turn a conversation into a menu.
    expect(q.allow_other).toBe(true);
  });

  it("ignores a launcher-var pre-fill source it does not implement", () => {
    // A bundle authored against a newer studio must still render — an empty
    // field, not a crash or a dropped bot.
    const bot = chatBotFromEntry(
      entry({
        chat: {
          nodes: { chat: { kind: "human", text_field: "message" } },
          launcher_vars: [
            { name: "workspace_dir", default_from: "work_dir" },
            { name: "future", default_from: "something_new" },
          ],
        },
      }),
    )!;
    expect(bot.launcherVars[0]).toEqual({
      name: "workspace_dir",
      label: "workspace_dir",
      defaultFrom: "work_dir",
    });
    expect(bot.launcherVars[1]).toEqual({ name: "future", label: "future" });
  });
});

describe("chatRegistryFrom", () => {
  it("keeps only conversational bots, keyed by id", () => {
    const reg = chatRegistryFrom([
      entry(),
      entry({ name: "review-pr", chat: undefined }),
    ]);
    expect(Object.keys(reg)).toEqual(["copilot"]);
  });

  it("honours the catalog visibility the operator already set", () => {
    // One visibility decision, not two: a bot hidden in the Catalog manager
    // must not reappear as a chat correspondent.
    expect(chatRegistryFrom([entry({ enabled: false })])).toEqual({});
  });
});
