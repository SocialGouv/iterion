// @vitest-environment jsdom
//
// A reading of a source reports at most 1000 of each code and says, on the
// last one it kept, how many more it counted: the panel shows the readings'
// counts, not the list's length — the count the unit answer carries as
// load_errors.
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { DocumentStoreProvider, getOrCreateDocumentStore } from "@/store/document";
import { SelectionStoreProvider, getOrCreateSelectionStore } from "@/store/selection";
import { countOfRaw, groupDiagnostics, countOf } from "@/lib/diagnostics";
import DiagnosticBadge from "./DiagnosticBadge";
import DiagnosticsPanel from "./DiagnosticsPanel";

const NOTE = "a reading reports 1000 of each code and 2000 of each severity, every code's first kept";

/** A unit answer's list: 1500 errors of a code in the main, 1500 and 1200
 *  of two codes in a fragment, 1100 duplicate declarations of the loader —
 *  5300 errors, 4000 reported, the last of each code saying how many more. */
function unitAnswer(): { diagnostics: string[]; loadErrors: number } {
  const out: string[] = [];
  const flood = (file: string, code: string, n: number) => {
    for (let i = 1; i <= Math.min(n, 1000); i++) {
      let msg = `${file}:${i}:3: error [${code}]: unknown property "bogus"`;
      if (i === 1000 && n > 1000) msg += ` — and ${n - 1000} more ${code} errors not reported: ${NOTE}`;
      out.push(msg);
    }
  };
  flood("main.bot", "E011", 1500);
  flood("lib/f.bot", "E011", 1500);
  flood("lib/f.bot", "E012", 1200);
  flood("lib/f.bot", "E010", 1100);
  return { diagnostics: out, loadErrors: 5300 };
}

afterEach(cleanup);

describe("the counts the studio shows are the readings'", () => {
  it("counts the ones a reading did not report", () => {
    const { diagnostics, loadErrors } = unitAnswer();
    expect(diagnostics).toHaveLength(4000);
    expect(countOfRaw(diagnostics)).toBe(loadErrors);
    expect(countOf(groupDiagnostics(diagnostics, [], null).all, "error")).toBe(loadErrors);
  });

  it("shows 5300 errors in the panel for a reading of 5300 reported 4000", () => {
    const { diagnostics } = unitAnswer();
    const docStore = getOrCreateDocumentStore("count-test");
    docStore.getState().setDiagnostics(diagnostics);
    render(
      <DocumentStoreProvider store={docStore}>
        <SelectionStoreProvider store={getOrCreateSelectionStore("count-test")}>
          <DiagnosticsPanel />
        </SelectionStoreProvider>
      </DocumentStoreProvider>,
    );
    expect(screen.getByText("5300 errors")).toBeTruthy();
    expect(screen.queryByText("4000 errors")).toBeNull();
  });

  it("says a node's badge counts at least what it lists", () => {
    const listed = groupDiagnostics([`main.bot:1:3: error [E011]: x — and 7 more E011 errors not reported: ${NOTE}`, "main.bot:2:3: error [E011]: y"], [], null).all;
    render(<DiagnosticBadge diagnostics={listed} />);
    expect(screen.getByRole("button", { name: "at least 2 errors" }).textContent).toContain("2+");
  });
});
