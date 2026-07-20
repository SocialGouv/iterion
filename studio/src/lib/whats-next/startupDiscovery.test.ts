import { describe, it, expect } from "vitest";

import type { RunSummary } from "@/api/runs";

import { candidateWorkflows, pickLiveRunId } from "./startupDiscovery";

function run(id: string, status: RunSummary["status"]): RunSummary {
  return { id, workflow_name: "whats_next", status } as RunSummary;
}

describe("candidateWorkflows", () => {
  it("probes the underscore spelling first, then the raw bot id", () => {
    expect(candidateWorkflows("whats-next")).toEqual([
      "whats_next",
      "whats-next",
    ]);
  });

  it("dedupes when the bot id has no hyphens", () => {
    expect(candidateWorkflows("nexie")).toEqual(["nexie"]);
  });

  it("replaces every hyphen, not just the first", () => {
    expect(candidateWorkflows("a-b-c")).toEqual(["a_b_c", "a-b-c"]);
  });
});

describe("pickLiveRunId", () => {
  it("returns null on an empty match list", () => {
    expect(pickLiveRunId([])).toBeNull();
  });

  it("picks the first non-terminal run in list order", () => {
    expect(
      pickLiveRunId([
        run("r1", "finished"),
        run("r2", "running"),
        run("r3", "paused_waiting_human"),
      ]),
    ).toBe("r2");
  });

  it("treats queued and paused_waiting_human as live", () => {
    expect(pickLiveRunId([run("q", "queued")])).toBe("q");
    expect(pickLiveRunId([run("p", "paused_waiting_human")])).toBe("p");
  });

  it("ignores terminal and operator-paused runs", () => {
    expect(
      pickLiveRunId([
        run("a", "finished"),
        run("b", "failed"),
        run("c", "failed_resumable"),
        run("d", "cancelled"),
        run("e", "paused_operator"),
      ]),
    ).toBeNull();
  });
});
