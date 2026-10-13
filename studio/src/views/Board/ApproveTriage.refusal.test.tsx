// @vitest-environment jsdom
import { beforeEach, afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

vi.mock("@/api/native", async () => {
  const actual = await vi.importActual<typeof import("@/api/native")>("@/api/native");
  return { ...actual, patchIssue: vi.fn() };
});

import { apiErrorFrom } from "@/api/client";
import { patchIssue, type NativeIssue } from "@/api/native";
import { useUIStore } from "@/store/ui";
import { ApproveTriageBanner } from "./ApproveTriage";

const patchMock = vi.mocked(patchIssue);

const parked: NativeIssue = {
  id: "native:abc12345",
  title: "opened by a drive-by author",
  state: "todo",
  created_at: "2026-10-01T10:00:00Z",
  updated_at: "2026-10-01T10:00:00Z",
  labels: ["bug", "needs:approval"],
  external: {
    provider: "github",
    connection_id: "c1",
    repo: "acme/widgets",
    number: 7,
    author: "drive-by",
  },
};

const errorToasts = () =>
  useUIStore
    .getState()
    .toasts.filter((t) => t.type === "error")
    .map((t) => t.message);

const approveButton = () => screen.getByRole("button", { name: /Approve & triage/i });

beforeEach(() => {
  useUIStore.setState({ toasts: [] });
  patchMock.mockReset();
});

afterEach(cleanup);

// Approving is the only gesture that arms a bot on a parked card. A refused
// label swap leaves the card parked, and nothing retries it: the reason has to
// reach the operator who clicked, and it has to be THIS click's reason —
// `useAsyncAction.run` hands it over through its onError channel, because the
// `error` field it also sets belongs to the next render.
describe("ApproveTriageBanner — what a refused approve tells the operator", () => {
  it("reports the refusal of the approve that just failed", async () => {
    patchMock.mockRejectedValueOnce(
      apiErrorFrom(409, { message: "patch issue: labels: card moved since" }),
    );
    render(<ApproveTriageBanner iss={parked} />);
    fireEvent.click(approveButton());

    await waitFor(() => expect(patchMock).toHaveBeenCalledTimes(1));
    await waitFor(() =>
      expect(
        errorToasts(),
        "a refused approve that reports nothing leaves the card parked under a button that looked like it worked",
      ).toEqual([expect.stringContaining("card moved since")]),
    );

    // And the SECOND refusal must speak of itself, not of the first.
    patchMock.mockRejectedValueOnce(
      apiErrorFrom(403, { message: "patch issue: forbidden on this team" }),
    );
    fireEvent.click(approveButton());
    await waitFor(() => expect(patchMock).toHaveBeenCalledTimes(2));
    await waitFor(() =>
      expect(errorToasts().join("\n"), "the second refusal's own reason").toContain(
        "forbidden on this team",
      ),
    );
  });

  it("swaps the labels and notifies its embedder when the approve lands", async () => {
    patchMock.mockResolvedValueOnce({ ...parked, labels: ["bug", "triage:auto"] });
    const onApproved = vi.fn();
    render(<ApproveTriageBanner iss={parked} onApproved={onApproved} />);
    fireEvent.click(approveButton());

    await waitFor(() => expect(onApproved).toHaveBeenCalledWith(["bug", "triage:auto"]));
    expect(patchMock).toHaveBeenCalledWith(parked.id, { labels: ["bug", "triage:auto"] });
    expect(errorToasts()).toEqual([]);
  });
});
