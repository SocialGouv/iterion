// @vitest-environment jsdom
//
// A reply that requires the editor must navigate, wait for the authoritative
// live document, and only then answer the paused assistant turn.

import { act, cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const router = vi.hoisted(() => ({
  route: "/pipelines",
  setLocation: vi.fn(),
}));
const editor = vi.hoisted(() => ({
  capture: vi.fn(),
}));
const files = vi.hoisted(() => ({
  open: vi.fn(),
}));
const draft = vi.hoisted(() => ({
  state: { source: null as string | null, designing: true },
}));
const submit = vi.fn().mockResolvedValue(undefined);

vi.mock("wouter", () => ({
  useLocation: () => [router.route, router.setLocation],
  Link: ({
    children,
    href,
  }: {
    children: React.ReactNode;
    href?: string;
  }) => (
    <a href={href}>
      {children}
    </a>
  ),
}));

vi.mock("@/lib/chatDock/editorSession", () => ({
  captureActiveEditorDocument: editor.capture,
}));

vi.mock("@/api/client", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/api/client")>()),
  openFile: files.open,
}));

vi.mock("@/hooks/useDraftBot", () => ({
  useDraftState: () => draft.state,
}));

import {
  EDITOR_OPENED_CONFIRMATION,
  hrefForAssistantReplyTarget,
  navigationTargetForReply,
  useNavigationReply,
} from "@/lib/chatDock/replyNavigation";
import { ApiError } from "@/api/client";
import { DraftBotOffer } from "./draftBotOffer";

function NavigationHarness({
  target = "view/editor",
  message = EDITOR_OPENED_CONFIRMATION,
}: {
  target?: string;
  message?: string;
}) {
  const navigation = useNavigationReply(submit);
  return (
    <>
      <button type="button" onClick={() => navigation.submit(message, target)}>
        Continue
      </button>
      {navigation.busy && <span>Loading destination</span>}
      {navigation.error && <span>{navigation.error}</span>}
    </>
  );
}

beforeEach(() => {
  router.route = "/pipelines";
  router.setLocation.mockReset();
  editor.capture.mockReset();
  files.open.mockReset();
  files.open.mockResolvedValue({
    source: "workflow demo:\n",
    document: {},
    diagnostics: [],
    path: "bots/demo/main.bot",
  });
  submit.mockReset();
  submit.mockResolvedValue(undefined);
  draft.state = { source: null, designing: true };
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

describe("navigate then send", () => {
  it("fuses a legacy edit reply with the retired editor venue", () => {
    expect(
      navigationTargetForReply(
        {
          label: "Modifier le bot",
          message: "Modifier le bot",
          navigateTo: null,
          legacy: true,
        },
        true,
        {
          kind: "bot",
          ref: "bot/bots/demo/main.bot",
          label: "Demo",
        },
      ),
    ).toBe("bot/bots/demo/main.bot");
  });

  it("does not navigate an immediate typed reply", () => {
    expect(
      navigationTargetForReply(
        {
          label: "Explique davantage",
          message: "Explique davantage",
          navigateTo: null,
          legacy: false,
        },
        true,
        null,
      ),
    ).toBeNull();
  });

  it("does not send before the destination route is active", () => {
    render(<NavigationHarness />);
    act(() => screen.getByText("Continue").click());

    expect(router.setLocation).toHaveBeenCalledWith("/editor");
    expect(submit).not.toHaveBeenCalled();
    expect(screen.getByText("Loading destination")).toBeTruthy();
  });

  it("sends the selected reply once after generic editor navigation", async () => {
    const { rerender } = render(<NavigationHarness message="Crée le bot." />);
    act(() => screen.getByText("Continue").click());

    router.route = "/editor";
    rerender(<NavigationHarness message="Crée le bot." />);

    await waitFor(() =>
      expect(submit).toHaveBeenCalledWith("Crée le bot.", "view/editor"),
    );
    expect(submit).toHaveBeenCalledTimes(1);
  });

  it("preflights an existing bot before starting navigation", async () => {
    let resolveOpen: ((value: unknown) => void) | undefined;
    files.open.mockImplementationOnce(
      () => new Promise((resolve) => {
        resolveOpen = resolve;
      }),
    );
    render(<NavigationHarness target="bot/bots/demo/main.bot" />);

    act(() => screen.getByText("Continue").click());

    expect(screen.getByText("Loading destination")).toBeTruthy();
    expect(files.open).toHaveBeenCalledWith("bots/demo/main.bot");
    expect(router.setLocation).not.toHaveBeenCalled();

    await act(async () => {
      resolveOpen?.({
        source: "workflow demo:\n",
        document: {},
        diagnostics: [],
        path: "bots/demo/main.bot",
      });
      await Promise.resolve();
    });

    expect(router.setLocation).toHaveBeenCalledWith(
      "/editor?file=bots%2Fdemo%2Fmain.bot",
    );
  });

  it("waits for the exact complete editor document before sending", async () => {
    vi.useFakeTimers();
    editor.capture.mockResolvedValue(null);
    const { rerender } = render(
      <NavigationHarness
        target="bot/bots/demo/main.bot"
        message="Modifie ce bot."
      />,
    );
    await act(async () => {
      screen.getByText("Continue").click();
      await Promise.resolve();
    });

    expect(router.setLocation).toHaveBeenCalledWith(
      "/editor?file=bots%2Fdemo%2Fmain.bot",
    );
    router.route = "/editor";
    rerender(
      <NavigationHarness
        target="bot/bots/demo/main.bot"
        message="Modifie ce bot."
      />,
    );
    await act(async () => {
      await Promise.resolve();
    });
    expect(submit).not.toHaveBeenCalled();

    editor.capture.mockResolvedValue({
      sessionId: "session-1",
      revision: 3,
      file: "bots/demo/main.bot",
      complete: true,
      sourceLength: 42,
      source: "workflow demo:\n",
    });
    await act(async () => {
      vi.advanceTimersByTime(50);
      await Promise.resolve();
    });

    expect(submit).toHaveBeenCalledWith(
      "Modifie ce bot.",
      "bot/bots/demo/main.bot",
    );
    expect(submit).toHaveBeenCalledTimes(1);
  });

  it("sends the withheld-document marker rather than dropping the request", async () => {
    editor.capture.mockResolvedValue({
      sessionId: "session-1",
      revision: 3,
      file: "bots/demo/main.bot",
      complete: false,
      sourceLength: 200_000,
    });
    const { rerender } = render(
      <NavigationHarness target="bot/bots/demo/main.bot" />,
    );
    act(() => screen.getByText("Continue").click());
    await waitFor(() =>
      expect(router.setLocation).toHaveBeenCalledWith(
        "/editor?file=bots%2Fdemo%2Fmain.bot",
      ),
    );
    router.route = "/editor";
    rerender(<NavigationHarness target="bot/bots/demo/main.bot" />);

    // A document over MAX_ACTIVE_EDITOR_SOURCE is WITHHELD, not a failed
    // send: the capture layer already drops `source` and leaves
    // `complete: false` + `sourceLength` for the bot to read. Dropping the
    // request instead gave the operator a dead click on the one path where
    // they had already stated their intent — while the ordinary composer
    // send, carrying the very same oversized document, went through.
    //
    // The bot can then answer usefully: name the file, say it is too large to
    // read inline, ask which part. What must never happen is a PREFIX — half
    // a workflow looks editable and cannot be validated honestly.
    await waitFor(() =>
      expect(submit).toHaveBeenCalledWith(
        expect.any(String),
        "bot/bots/demo/main.bot",
      ),
    );
    expect(
      screen.queryByText(/too large to send completely/i),
    ).toBeNull();
  });

  it("does not navigate to a bot missing from the current workspace", async () => {
    files.open.mockRejectedValueOnce(
      new ApiError(404, "API error 404: file not found"),
    );
    render(<NavigationHarness target="bot/bots/missing/main.bot" />);

    act(() => screen.getByText("Continue").click());

    expect(
      await screen.findByText(/does not exist in the current workspace/i),
    ).toBeTruthy();
    expect(router.setLocation).not.toHaveBeenCalled();
    expect(submit).not.toHaveBeenCalled();
  });

  it("does not navigate to a bot path rejected by the workspace boundary", async () => {
    files.open.mockRejectedValueOnce(
      new ApiError(400, "API error 400: invalid path"),
    );
    render(<NavigationHarness target="bot/bots/rejected/main.bot" />);

    act(() => screen.getByText("Continue").click());

    expect(
      await screen.findByText(/not a valid file in the current workspace/i),
    ).toBeTruthy();
    expect(router.setLocation).not.toHaveBeenCalled();
    expect(submit).not.toHaveBeenCalled();
  });

  it("refuses a model-authored URL or unknown reference", () => {
    render(<NavigationHarness target="https://evil.example/editor" />);
    act(() => screen.getByText("Continue").click());

    expect(screen.getByText(/destination is not available/i)).toBeTruthy();
    expect(router.setLocation).not.toHaveBeenCalled();
    expect(submit).not.toHaveBeenCalled();
  });

  it("refuses a typed reference that escapes the workspace", () => {
    expect(hrefForAssistantReplyTarget("bot/../../etc/passwd")).toBeNull();
  });

  it("refuses a source helper because only a workflow can attach authoring context", () => {
    expect(
      hrefForAssistantReplyTarget(
        "bot/bots/demo/scripts/tools/pipeline_helper.py",
      ),
    ).toBeNull();
  });
});

describe("draft offer", () => {
  it("does not render the old standalone venue button", () => {
    render(<DraftBotOffer runId="run-1" revision={1} />);
    expect(screen.queryByText(/^Open the editor$/i)).toBeNull();
  });

  it("still offers an already-produced draft", () => {
    draft.state = { source: "workflow demo:\n", designing: true };
    render(<DraftBotOffer runId="run-1" revision={1} />);
    expect(screen.getByText(/open this draft in the editor/i)).toBeTruthy();
  });
});
