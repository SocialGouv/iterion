// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { OrgView } from "@/api/orgs";
import { expectNoViolationsStrict } from "@/__tests__/a11y/axeHelpers";

import { OrgsTable } from "./OrgsTable";

afterEach(cleanup);

// Two orgs sharing a DISPLAY NAME: the hostile case for the row's own
// control, and a real one — the name is free text while the slug is not.
const orgs: OrgView[] = [
  { id: "o1", name: "Acme", slug: "acme-eu", status: "active", memory_quota_bytes: 1 << 30 },
  { id: "o2", name: "Acme", slug: "acme-us", status: "suspended", memory_quota_bytes: 2 << 30 },
];

function mount(onOpen = vi.fn()) {
  const view = render(<OrgsTable orgs={orgs} loaded onOpen={onOpen} />);
  return { ...view, onOpen };
}

// The table's rows held a ghost Open button while spreading
// clickableRowProps (role="button" + tabindex) — a button inside a button
// (#2766): invalid ARIA, and two tab stops for one action on every row.
describe("OrgsTable activation paths", () => {
  it("leaves the rows roleless — the Open button is the keyboard path", () => {
    const { onOpen } = mount();
    const rows = screen.getAllByRole("row").slice(1); // drop the header row
    expect(rows).toHaveLength(2);
    for (const row of rows) {
      expect(row.getAttribute("role")).toBeNull();
      expect(row.hasAttribute("tabindex")).toBe(false);
      fireEvent.keyDown(row, { key: "Enter" });
      fireEvent.keyDown(row, { key: " " });
    }
    expect(onOpen).not.toHaveBeenCalled();
  });

  it("names each row's Open after the org it opens, slug included", () => {
    const { onOpen } = mount();
    const opens = screen.getAllByRole("button", { name: /^Open / });
    expect(opens).toHaveLength(2);
    // Two orgs named "Acme": the name alone would offer a screen reader
    // two identical controls doing different things.
    expect(new Set(opens.map((b) => b.getAttribute("aria-label"))).size).toBe(2);
    // The visible word survives in the name, for SC 2.5.3.
    for (const b of opens) expect(b.textContent).toBe("Open");
    fireEvent.click(screen.getByRole("button", { name: "Open Acme acme-us" }));
    expect(onOpen).toHaveBeenCalledTimes(1);
    expect(onOpen).toHaveBeenCalledWith(orgs[1]);
  });

  it("keeps the row click as the pointer convenience, firing once", () => {
    const { onOpen } = mount();
    fireEvent.click(screen.getAllByRole("row")[1]!);
    expect(onOpen).toHaveBeenCalledTimes(1);
    expect(onOpen).toHaveBeenCalledWith(orgs[0]);
  });

  it("Admin · OrgsTable — axe smoke, incompletes included", async () => {
    const { container } = mount();
    await expectNoViolationsStrict(container, "OrgsTable");
  });
});
