// @vitest-environment jsdom
//
// Clicking "Open the editor" must ANSWER the paused turn, not merely navigate.
//
// Reported from a real session — "j'ai cliqué et rien ne s'est lancé". The
// assistant had asked to move, the operator agreed, and the run stayed parked
// at its human node while they waited on a canvas nothing was going to fill.
// The click is a consent event and has to reach the run as one.
//
// The second rule is timing: the composer stamps the page context at SEND
// time, so the answer waits for the route to settle. Answering from the page
// they just left would tell the bot they are still there — and it would orient
// them to the editor a second time.
import { act, cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const submitPending = vi.fn().mockResolvedValue(undefined);
let route = "/board";
let draftState: { source: string | null; designing: boolean } = {
  source: null,
  designing: true,
};

vi.mock("wouter", () => ({
  useLocation: () => [route, vi.fn()],
  Link: ({
    children,
    onClick,
  }: {
    children: React.ReactNode;
    onClick?: () => void;
    href?: string;
  }) => (
    <button type="button" onClick={onClick}>
      {children}
    </button>
  ),
}));

vi.mock("@/hooks/useDraftBot", () => ({
  useDraftState: () => draftState,
}));

import {
  DraftBotOffer,
  EDITOR_OPENED_CONFIRMATION,
  useEditorConsent,
} from "./draftBotOffer";

// Exercises the REAL hook — an earlier draft of this test re-implemented the
// effect in the harness and would have passed with the bug still in place.
function Harness() {
  const consent = useEditorConsent(submitPending);
  return <DraftBotOffer runId="run-1" revision={1} onOpenEditor={consent.accept} />;
}

beforeEach(() => {
  submitPending.mockClear();
  route = "/board";
  draftState = { source: null, designing: true };
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

describe("accepting the move to the editor", () => {
  it("offers the venue while the turn is designing with nothing to show", () => {
    render(<Harness />);
    expect(screen.getByText(/open the editor/i)).toBeTruthy();
  });

  it("does not answer the turn before the route reaches the editor", () => {
    render(<Harness />);
    act(() => {
      screen.getByText(/open the editor/i).click();
    });
    expect(submitPending).not.toHaveBeenCalled();
  });

  it("answers the turn once the operator is on the editor", () => {
    const { rerender } = render(<Harness />);
    act(() => {
      screen.getByText(/open the editor/i).click();
    });
    route = "/editor";
    act(() => {
      rerender(<Harness />);
    });
    expect(submitPending).toHaveBeenCalledWith(EDITOR_OPENED_CONFIRMATION);
  });

  it("answers exactly once, not on every later render", () => {
    const { rerender } = render(<Harness />);
    act(() => {
      screen.getByText(/open the editor/i).click();
    });
    route = "/editor";
    act(() => rerender(<Harness />));
    act(() => rerender(<Harness />));
    expect(submitPending).toHaveBeenCalledTimes(1);
  });

  it("retires consent when navigation goes somewhere other than the editor", () => {
    const { rerender } = render(<Harness />);
    act(() => {
      screen.getByText(/open the editor/i).click();
    });
    route = "/runs/run-2";
    act(() => rerender(<Harness />));
    route = "/editor";
    act(() => rerender(<Harness />));
    expect(submitPending).not.toHaveBeenCalled();
  });

  it("expires consent when the editor link opens outside this tab", () => {
    vi.useFakeTimers();
    const { rerender } = render(<Harness />);
    act(() => {
      screen.getByText(/open the editor/i).click();
    });
    act(() => {
      vi.advanceTimersByTime(30_000);
    });
    route = "/editor";
    act(() => rerender(<Harness />));
    expect(submitPending).not.toHaveBeenCalled();
  });

  it("never speaks for the operator on its own — no click, no answer", () => {
    route = "/editor";
    render(<Harness />);
    expect(submitPending).not.toHaveBeenCalled();
    expect(screen.queryByText(/open the editor/i)).toBeNull();
  });

  it("asks for no consent when the draft is already in hand", () => {
    // A finished draft needs no permission to be looked at; that button only
    // navigates, so it must not answer the turn.
    draftState = { source: "workflow demo:\n", designing: true };
    render(<Harness />);
    act(() => {
      screen.getByText(/open this draft/i).click();
    });
    expect(submitPending).not.toHaveBeenCalled();
  });
});
