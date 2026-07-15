// @vitest-environment jsdom
import { renderHook } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import type { NativeBoard, NativeIssue } from "@/api/native";

import { useBoardColumns } from "./useBoardColumns";

const board = {
  states: [{ name: "backlog" }, { name: "done" }],
} as unknown as NativeBoard;

function issue(id: string, bot?: string): NativeIssue {
  return { id, title: id, state: "backlog", bot } as unknown as NativeIssue;
}

function run(issues: NativeIssue[], botFilter: string) {
  return renderHook(() =>
    useBoardColumns({
      board,
      issues,
      searchQuery: "",
      labelFilter: new Set<string>(),
      assigneeFilter: "",
      botFilter,
      sortMode: "priority",
    }),
  );
}

describe("useBoardColumns bot filter", () => {
  const issues = [
    issue("a", "feature-dev"),
    issue("b", "docs-refresh"),
    issue("c"),
  ];

  it("derives the distinct bot list from the issues", () => {
    const { result } = run(issues, "");
    expect(result.current.allBots).toEqual(["docs-refresh", "feature-dev"]);
  });

  it("narrows filteredIssues to cards matching the selected bot", () => {
    const { result } = run(issues, "feature-dev");
    expect(result.current.filteredIssues.map((i) => i.id)).toEqual(["a"]);
  });

  it("returns all issues when no bot filter is active", () => {
    const { result } = run(issues, "");
    expect(result.current.filteredIssues.map((i) => i.id)).toEqual([
      "a",
      "b",
      "c",
    ]);
  });
});
