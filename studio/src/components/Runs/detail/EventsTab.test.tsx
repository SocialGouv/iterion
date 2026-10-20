// @vitest-environment jsdom
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { EventsTab } from "./EventsTab";

afterEach(cleanup);

describe("EventsTab", () => {
  it("names its search field — a placeholder alone is not a label (RGAA 11.1)", () => {
    render(<EventsTab events={[]} />);
    expect(screen.getByRole("textbox", { name: "Search events" })).toBeTruthy();
  });
});
