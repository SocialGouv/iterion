// @vitest-environment jsdom
//
// The operator's OPENING message must be in the transcript.
//
// It is the one turn that is not an event: a conversational bot receives it as
// a launch VAR (the manifest's `chat.seed_var`), so a transcript folded from
// the event stream alone opens on the assistant's reply with the question it
// answers nowhere on screen. Reported from a real session — "il n'affiche pas
// mon message dans le dialogue".
//
// Read back off the run's persisted inputs rather than the launch call, so a
// REATTACHED session (reload, second tab, days later) shows it too — which is
// exactly the case the launch-time value cannot cover.
import { renderHook } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import type { RunSnapshot } from "@/api/runs";
import type { FirstClassBot } from "@/lib/whats-next/firstClassBots";

import { useSessionMessages } from "./useSessionMessages";

const bot = {
  id: "copilot",
  label: "Copi",
  description: "",
  workflowPath: "bots/copilot/main.bot",
  launcherVars: [],
  seedVar: "initial_message",
  nodeMap: {},
} as unknown as FirstClassBot;

function snapshotWith(inputs: Record<string, unknown>): RunSnapshot {
  return { run: { inputs } } as unknown as RunSnapshot;
}

function render(opts: {
  bot?: FirstClassBot;
  snapshot: RunSnapshot | null;
}) {
  return renderHook(() =>
    useSessionMessages({
      bot: opts.bot ?? bot,
      runId: "run-1",
      runStatus: "paused_waiting_human",
      events: [],
      snapshot: opts.snapshot,
      pendingHuman: null,
    }),
  );
}

describe("the operator's opening message", () => {
  it("opens the transcript with what the operator actually asked", () => {
    const { result } = render({
      snapshot: snapshotWith({ initial_message: "Crée un bot hello world" }),
    });
    expect(result.current[0]).toMatchObject({
      kind: "user-message",
      text: "Crée un bot hello world",
    });
  });

  it("survives a reattach, because it comes from the run's own inputs", () => {
    // No launch happened in this browser — only the persisted run remains.
    const { result } = render({
      snapshot: snapshotWith({ initial_message: "reattached ask" }),
    });
    expect(result.current).toHaveLength(1);
    expect(result.current[0]).toMatchObject({ text: "reattached ask" });
  });

  it("adds nothing when the bot declares no seed var", () => {
    const noSeed = { ...bot, seedVar: undefined } as unknown as FirstClassBot;
    const { result } = render({
      bot: noSeed,
      snapshot: snapshotWith({ initial_message: "ignored" }),
    });
    expect(result.current).toHaveLength(0);
  });

  it("adds nothing for a blank seed rather than an empty bubble", () => {
    const { result } = render({ snapshot: snapshotWith({ initial_message: "   " }) });
    expect(result.current).toHaveLength(0);
  });

  it("tolerates a run with no inputs at all", () => {
    const { result } = render({ snapshot: snapshotWith({}) });
    expect(result.current).toHaveLength(0);
    expect(render({ snapshot: null }).result.current).toHaveLength(0);
  });
});
