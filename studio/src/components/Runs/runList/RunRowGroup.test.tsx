// @vitest-environment jsdom
import { cleanup, render } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { RunRowGroup } from "./RunRowGroup";

afterEach(cleanup);

// RGAA 5.7 (#2509): each group owns its <tbody> — the rowgroup header
// (scope="rowgroup") then spans exactly its own rows, and the group
// boundary is structural, not just visual.
describe("RunRowGroup owns its tbody", () => {
  it("wraps the rowgroup header and the group's rows in one tbody", () => {
    const { container } = render(
      <table>
        <RunRowGroup label="In progress" count={2} showHeader columnSpan={8}>
          <tr>
            <td>row one</td>
          </tr>
          <tr>
            <td>row two</td>
          </tr>
        </RunRowGroup>
      </table>,
    );
    const tbody = container.querySelector("tbody")!;
    expect(tbody).not.toBeNull();
    expect(tbody.querySelector('th[scope="rowgroup"]')).not.toBeNull();
    expect(tbody.querySelectorAll("tr").length).toBe(3);
  });

  it("still wraps the rows when the header is suppressed", () => {
    const { container } = render(
      <table>
        <RunRowGroup label="none" count={1} showHeader={false} columnSpan={8}>
          <tr>
            <td>row one</td>
          </tr>
        </RunRowGroup>
      </table>,
    );
    const tbody = container.querySelector("tbody")!;
    expect(tbody).not.toBeNull();
    expect(tbody.querySelector('th[scope="rowgroup"]')).toBeNull();
    expect(tbody.querySelectorAll("tr").length).toBe(1);
  });
});
