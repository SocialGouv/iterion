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

vi.mock("@/hooks/useDraftBot", () => ({
  useDraftState: () => draft.state,
}));

import {
  EDITOR_OPENED_CONFIRMATION,
  hrefForAssistantReplyTarget,
  navigationTargetForReply,
  useNavigationReply,
} from "@/lib/chatDock/replyNavigation";
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

    await waitFor(() => expect(submit).toHaveBeenCalledWith("Crée le bot."));
    expect(submit).toHaveBeenCalledTimes(1);
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
    act(() => screen.getByText("Continue").click());

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

    expect(submit).toHaveBeenCalledWith("Modifie ce bot.");
    expect(submit).toHaveBeenCalledTimes(1);
  });

  it("does not send a modification request with an incomplete document", async () => {
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
    router.route = "/editor";
    rerender(<NavigationHarness target="bot/bots/demo/main.bot" />);

    expect(
      await screen.findByText(/too large to send completely/i),
    ).toBeTruthy();
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
