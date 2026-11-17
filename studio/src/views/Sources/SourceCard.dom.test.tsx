// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { SourceCard } from "./SourceCard";
import type { Source } from "@/api/sources";

const SOURCE: Source = {
  kind: "connector",
  id: "probe",
  display_name: "Probe",
  description: "a probe source",
  category: "source_control",
  maturity: "experimental",
  tier: "team",
  connections: 2,
  subscriptions: 1,
};

afterEach(cleanup);

describe("SourceCard", () => {
  it("renders the facts that decide a choice and pluralizes honestly", () => {
    render(<SourceCard source={SOURCE} onOpen={() => {}} />);
    expect(screen.getByText("Probe")).toBeTruthy();
    expect(screen.getByText((_, el) => el?.textContent === "2 connexions · 1 abonnement")).toBeTruthy();
    expect(screen.getByText("Expérimental")).toBeTruthy();
    expect(screen.getByText("Forge & dépôts")).toBeTruthy();
  });

  it("opens the drawer on card and button", () => {
    const onOpen = vi.fn();
    render(<SourceCard source={SOURCE} onOpen={onOpen} />);
    fireEvent.click(screen.getByRole("button", { name: "Gérer Probe" }));
    expect(onOpen).toHaveBeenCalled();
  });

  it("reads Brancher when nothing is connected yet", () => {
    render(
      <SourceCard
        source={{ ...SOURCE, connections: 0, subscriptions: 0 }}
        onOpen={() => {}}
      />,
    );
    expect(screen.getByRole("button", { name: "Brancher Probe" })).toBeTruthy();
  });
});
