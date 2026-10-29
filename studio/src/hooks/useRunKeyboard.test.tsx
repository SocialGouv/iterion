// @vitest-environment jsdom
//
// #1812 — run tabs stay mounted while hidden (RunsTabsView), and every
// mounted RunView wires the run console's keys at window level. Inside a
// subtree marked hidden the listener must not be there at all: j/k, the
// arrows, Escape and the g-chords would move a console nobody is looking
// at — the #1788 class, on the run console. The dialogs' half is the kit's
// (hiddenSubtree.test.tsx pins each portal against the same flag).
import { cleanup, fireEvent, render } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { ExecutionState } from "@/api/runs";
import { HiddenSubtreeContext } from "@/components/ui/hiddenSubtree";
import { useRunKeyboard } from "./useRunKeyboard";

const executions = [
  { ir_node_id: "a", status: "finished", first_seq: 1 },
  { ir_node_id: "b", status: "finished", first_seq: 2 },
] as unknown as ExecutionState[];

interface ProbeProps {
  onSelectNode: (id: string | null) => void;
  onJumpToFailed: () => void;
}

function Probe({ onSelectNode, onJumpToFailed }: ProbeProps) {
  useRunKeyboard({
    selectedNodeId: null,
    executions,
    iterationByNode: new Map(),
    onSelectNode,
    onSelectIteration: () => {},
    onJumpToFailed,
  });
  return null;
}

// Two run tabs as RunsTabsView keeps them: both mounted, one marked hidden.
function tabs(firstHidden: boolean, first: ProbeProps, second: ProbeProps) {
  return (
    <>
      <HiddenSubtreeContext.Provider value={firstHidden}>
        <Probe {...first} />
      </HiddenSubtreeContext.Provider>
      <HiddenSubtreeContext.Provider value={!firstHidden}>
        <Probe {...second} />
      </HiddenSubtreeContext.Provider>
    </>
  );
}

const probe = () => ({ onSelectNode: vi.fn(), onJumpToFailed: vi.fn() });

afterEach(cleanup);

describe("useRunKeyboard with two run tabs mounted (#1812)", () => {
  it("a key reaches the tab on screen only", () => {
    const hidden = probe();
    const shown = probe();
    render(tabs(true, hidden, shown));
    fireEvent.keyDown(document.body, { key: "j" });
    expect(hidden.onSelectNode).not.toHaveBeenCalled();
    expect(shown.onSelectNode).toHaveBeenCalledWith("a");
  });

  it("a hidden tab holds no g-chord either", () => {
    const hidden = probe();
    const shown = probe();
    render(tabs(true, hidden, shown));
    // "g" then "f" — jump to the first failed node: only the console on
    // screen takes the chord.
    fireEvent.keyDown(document.body, { key: "g" });
    fireEvent.keyDown(document.body, { key: "f" });
    expect(hidden.onJumpToFailed).not.toHaveBeenCalled();
    expect(shown.onJumpToFailed).toHaveBeenCalledTimes(1);
  });

  it("the listener follows the tab: released on hide, back on show", () => {
    const first = probe();
    const second = probe();
    const view = render(tabs(false, first, second));
    fireEvent.keyDown(document.body, { key: "j" });
    expect(first.onSelectNode).toHaveBeenCalledTimes(1);
    expect(second.onSelectNode).not.toHaveBeenCalled();

    // The user switches tabs: the now-hidden one answers no longer, the
    // one on screen answers.
    view.rerender(tabs(true, first, second));
    fireEvent.keyDown(document.body, { key: "j" });
    expect(first.onSelectNode).toHaveBeenCalledTimes(1);
    expect(second.onSelectNode).toHaveBeenCalledWith("a");
  });
});
