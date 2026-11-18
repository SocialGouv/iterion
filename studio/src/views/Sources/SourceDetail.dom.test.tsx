// @vitest-environment jsdom
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { SourceDetail } from "./SourceDetail";
import type { Source } from "@/api/sources";

const MIXED: Source = {
  kind: "connector",
  id: "mix",
  display_name: "Mix",
  delivery: "mixed",
  connections: 0,
  subscriptions: 0,
  triggers: [
    { id: "mix.issue.updated", display_name: "Issue updated", delivery: "push" },
    {
      id: "mix.issue.polled",
      display_name: "Issue changed (polled)",
      delivery: "poll",
      interval: "5m",
      query: [{ id: "jql", type: "string", required: true }],
    },
  ],
};

afterEach(cleanup);

describe("SourceDetail", () => {
  // What polling promises is said on the card, before the plug.
  it("says each trigger's delivery on a mixed card, and a polled one's contract and query", () => {
    render(<SourceDetail source={MIXED} onClose={() => {}} />);
    const pushed = screen.getByText("Issue updated").closest("li");
    const polled = screen.getByText("Issue changed (polled)").closest("li");
    expect(pushed?.textContent).toContain("webhook");
    expect(pushed?.textContent).not.toContain("Interrogé");
    expect(polled?.textContent).toContain("interrogé");
    expect(polled?.textContent).toContain(
      "Interrogé toutes les 5 min : voit les versions qu'il observe à chaque lecture",
    );
    expect(polled?.textContent).toContain("Requête : jql *");
  });
});
