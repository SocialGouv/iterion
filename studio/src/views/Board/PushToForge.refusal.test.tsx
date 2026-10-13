// @vitest-environment jsdom
import { beforeEach, afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

vi.mock("@/auth/AuthContext", () => ({ useAuth: () => ({ activeTeamID: "t1" }) }));

vi.mock("@/api/native", async () => {
  const actual = await vi.importActual<typeof import("@/api/native")>("@/api/native");
  return { ...actual, pushIssueToForge: vi.fn() };
});

vi.mock("@/api/forgeConnections", async () => {
  const actual =
    await vi.importActual<typeof import("@/api/forgeConnections")>("@/api/forgeConnections");
  return {
    ...actual,
    listForgeConnections: vi.fn(async () => [
      {
        id: "c1",
        provider: "github",
        kind: "pat",
        status: "active",
        account_login: "iterion-bot",
        created_at: "2026-10-01T00:00:00Z",
      },
    ]),
  };
});

import { apiErrorFrom } from "@/api/client";
import { pushIssueToForge, type NativeIssue } from "@/api/native";
import { useServerInfoStore } from "@/store/serverInfo";
import { useUIStore } from "@/store/ui";
import { PushToForgeButton } from "./PushToForge";

const pushMock = vi.mocked(pushIssueToForge);

const linked: NativeIssue = {
  id: "native:abc12345",
  title: "needs a decision",
  state: "in_progress",
  created_at: "2026-10-01T10:00:00Z",
  updated_at: "2026-10-01T10:00:00Z",
  external: {
    provider: "github",
    connection_id: "c1",
    repo: "acme/widgets",
    number: 7,
    url: "https://github.com/acme/widgets/issues/7",
  },
};
const unlinked: NativeIssue = { ...linked, external: undefined };

const renderButton = (iss: NativeIssue) =>
  render(
    <QueryClientProvider
      client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}
    >
      <PushToForgeButton iss={iss} />
    </QueryClientProvider>,
  );

const errorToasts = () =>
  useUIStore
    .getState()
    .toasts.filter((t) => t.type === "error")
    .map((t) => t.message);

beforeEach(() => {
  useServerInfoStore.setState({ info: { mode: "cloud" } as never });
  useUIStore.setState({ toasts: [] });
  pushMock.mockReset();
});

afterEach(cleanup);

// A push the server refuses is the only statement anything makes about a card
// the forge does not hold: nothing retries it and nothing else mentions it.
// The reason has to reach the operator who clicked, and it has to be THIS
// click's reason — `useAsyncAction.run` hands it over through its onError
// channel, because the `error` field it also sets belongs to the next render.
describe("PushToForgeButton — what a refused push tells the operator", () => {
  it("reports the refusal of the push that just failed", async () => {
    pushMock.mockRejectedValueOnce(
      apiErrorFrom(502, { message: "push to forge: create issue: 403 forbidden" }),
    );
    renderButton(linked);
    fireEvent.click(screen.getByRole("button", { name: /Push to forge/i }));

    await waitFor(() => expect(pushMock).toHaveBeenCalledTimes(1));
    await waitFor(() =>
      expect(
        errorToasts(),
        "a refused push that reports nothing leaves the operator believing the card is on the forge",
      ).toEqual([expect.stringContaining("403 forbidden")]),
    );

    // And the SECOND refusal must speak of itself, not of the first.
    pushMock.mockRejectedValueOnce(apiErrorFrom(409, { message: "push to forge: already linked" }));
    fireEvent.click(screen.getByRole("button", { name: /Push to forge/i }));
    await waitFor(() => expect(pushMock).toHaveBeenCalledTimes(2));
    await waitFor(() =>
      expect(errorToasts().join("\n"), "the second refusal's own reason").toContain(
        "already linked",
      ),
    );
  });

  it("shows the dialog's own banner when the create-and-link is refused", async () => {
    pushMock.mockRejectedValueOnce(
      apiErrorFrom(404, { message: "push to forge: repo acme/widgets: not found" }),
    );
    renderButton(unlinked);
    fireEvent.click(screen.getByRole("button", { name: /Push to forge/i }));

    fireEvent.change(await screen.findByPlaceholderText("owner/repo"), {
      target: { value: "acme/widgets" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Push" }));

    await waitFor(() => expect(pushMock).toHaveBeenCalledTimes(1));
    await screen.findByText(/repo acme\/widgets: not found/);
    // The dialog stays open on a refusal — closing it would drop the only
    // place that reason is shown.
    expect(screen.getByPlaceholderText("owner/repo")).toBeTruthy();
  });

  it("toasts the forge URL of a push that succeeded", async () => {
    pushMock.mockResolvedValueOnce({
      url: "https://github.com/acme/widgets/issues/7",
      number: 7,
      provider: "github",
    });
    renderButton(linked);
    fireEvent.click(screen.getByRole("button", { name: /Push to forge/i }));

    await waitFor(() => expect(useUIStore.getState().toasts).toHaveLength(1));
    const [toast] = useUIStore.getState().toasts;
    expect(toast?.type).toBe("success");
    expect(errorToasts()).toEqual([]);
  });
});
