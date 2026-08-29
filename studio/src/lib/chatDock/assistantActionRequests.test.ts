import { beforeEach, describe, expect, it, vi } from "vitest";

const api = vi.hoisted(() => ({
  transitionIssue: vi.fn(),
}));

vi.mock("@/api/native", () => api);
vi.mock("@/api/bots", () => ({}));
vi.mock("@/api/dispatcher", () => ({}));
vi.mock("@/api/pipelineBoards", () => ({}));
vi.mock("@/api/plugins", () => ({}));
vi.mock("@/api/runs", () => ({}));

import type { AssistantActionRequest } from "./assistantActions";
import {
  executeAssistantAction,
  validateAssistantActionRequest,
} from "./assistantActionRequests";

beforeEach(() => vi.resetAllMocks());

function request(
  id: AssistantActionRequest["id"],
  args: Record<string, unknown>,
): AssistantActionRequest {
  return { key: "run:node:1:0", id, intent: "explicit", args };
}

describe("assistant action request boundary", () => {
  it("rebuilds API payloads from allowed fields only", () => {
    const validated = validateAssistantActionRequest(
      request("board.issue.update", {
        issue_id: "issue-1",
        title: "New title",
        labels: ["bug"],
        method: "DELETE",
        secret: "must not cross",
      }),
    );
    expect(validated.args).toEqual({
      issue_id: "issue-1",
      patch: { title: "New title", labels: ["bug"] },
    });
  });

  it("rejects incomplete and editor-session-bypassing requests", () => {
    expect(() =>
      validateAssistantActionRequest(request("run.cancel", {})),
    ).toThrow(/run_id/);
    expect(() =>
      validateAssistantActionRequest(request("editor.save", {})),
    ).toThrow(/live editor-session/i);
  });

  it("executes the host-selected function with validated arguments", async () => {
    api.transitionIssue.mockResolvedValue({ id: "issue-1", state: "done" });
    const validated = validateAssistantActionRequest(
      request("board.issue.transition", {
        issue_id: "issue-1",
        to: "done",
        arbitrary_request_options: { credentials: "omit" },
      }),
    );
    await expect(executeAssistantAction(validated)).resolves.toMatchObject({
      message: expect.stringContaining("issue-1"),
    });
    expect(api.transitionIssue).toHaveBeenCalledWith("issue-1", "done");
  });
});
