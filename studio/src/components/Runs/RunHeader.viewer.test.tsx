// @vitest-environment jsdom
//
// The action affordances of the run header derive from the caller's
// EFFECTIVE standing for this run (ADR-143), not from guessing off the
// run status: a viewer — a team viewer, an org-admin-lineage reader, an
// org-shared reader — reads but never acts, so the header proposes no
// command the run ladder will 403. No viewer named (auth-less local
// server): today's affordances stand unchanged.
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

// The run store is a selector hook; the tests answer every selection
// from one mutable object they reshape per case.
const store: Record<string, unknown> = {};

vi.mock("wouter", () => ({
  useLocation: () => ["/", vi.fn()],
}));
vi.mock("@/api/runs", () => ({
  cancelRun: vi.fn(),
  deleteRun: vi.fn(),
  getRun: vi.fn(),
  loadEvents: vi.fn(),
  pauseRun: vi.fn(),
  renameRun: vi.fn(),
}));
vi.mock("@/hooks/useConfirm", () => ({
  useConfirm: () => ({ confirm: vi.fn(async () => true), dialog: <div /> }),
}));
vi.mock("@/store/run", () => ({
  useRunStore: (select: (s: Record<string, unknown>) => unknown) => select(store),
}));
vi.mock("@/store/serverInfo", () => ({
  useServerInfoStore: (select: (s: Record<string, unknown>) => unknown) => select(store),
}));
// The dialogs and the side rows are heavy or fetch on mount and are
// irrelevant: this is about which action affordances the header offers.
vi.mock("./ForkDialog", () => ({ default: () => <div /> }));
vi.mock("./RewindDialog", () => ({ default: () => <div /> }));
vi.mock("./ResumeDialog", () => ({ default: () => <div /> }));
vi.mock("./RunShellPanel", () => ({ default: () => <div /> }));
vi.mock("./runHeader/AssistantWatchBadge", () => ({ default: () => <div /> }));
vi.mock("./runHeader/BackendsUsedRow", () => ({ default: () => <div /> }));
vi.mock("./runHeader/BotChip", () => ({ default: () => <div /> }));
vi.mock("./runHeader/DeploymentRow", () => ({ default: () => <div /> }));
vi.mock("./runHeader/ErrorHintRow", () => ({ default: () => <div /> }));
vi.mock("./runHeader/FallbacksUsedRow", () => ({ default: () => <div /> }));
vi.mock("./runHeader/FinalizationRow", () => ({ default: () => <div /> }));
vi.mock("./runHeader/ForkedFromRow", () => ({ default: () => <div /> }));
vi.mock("./runHeader/RunChildrenPanel", () => ({ default: () => <div /> }));
vi.mock("./runHeader/NotesRow", () => ({ default: () => <div /> }));
vi.mock("./runHeader/ResultLinksRow", () => ({ default: () => <div /> }));
vi.mock("./runHeader/DeliverablesRow", () => ({ default: () => <div /> }));
vi.mock("./runHeader/RunTagsRow", () => ({ default: () => <div /> }));
vi.mock("./runHeader/RunInformationAccordion", () => ({ default: () => <div /> }));
vi.mock("./runHeader/SourceTicketRow", () => ({ default: () => <div /> }));
vi.mock("@/components/shared/WSStatusDot", () => ({ default: () => <div /> }));

import type { RunHeader as RunHeaderType } from "@/api/runs";

import RunHeader from "./RunHeader";

// A failed-resumable run lights the action affordances whose absence the
// ADR-143 gate is about: resume, rename (cancel does not light on this
// status for anyone — that conditional is not under test here).
const run = {
  id: "run-1",
  status: "failed_resumable",
  name: "Demo run",
  workflow_name: "wf",
  created_at: new Date().toISOString(),
} as unknown as RunHeaderType;

function mount(viewer?: { role: string; via?: string } | null) {
  return render(<RunHeader run={run} active={false} wsState="open" viewer={viewer} />);
}

describe("RunHeader action affordances follow the caller's standing", () => {
  beforeEach(() => {
    for (const k of Object.keys(store)) delete store[k];
    store.requestWsReconnect = vi.fn();
    store.applySnapshot = vi.fn();
    store.resultLinks = [];
  });
  afterEach(cleanup);

  it("a viewer is offered no action the ladder would 403", () => {
    mount({ role: "viewer", via: "org-shared" });
    expect(screen.queryByRole("button", { name: /resume/i })).toBeNull();
    expect(screen.queryByLabelText("Rename run")).toBeNull();
    // Reads stay: export is client-side assembly of GET-only data.
    expect(screen.queryByRole("button", { name: /export/i })).not.toBeNull();
  });

  it("no viewer named (auth-less local server) keeps today's affordances", () => {
    mount(undefined);
    expect(screen.queryByRole("button", { name: /resume/i })).not.toBeNull();
    expect(screen.queryByLabelText("Rename run")).not.toBeNull();
  });

  it("a member keeps today's affordances", () => {
    mount({ role: "member" });
    expect(screen.queryByRole("button", { name: /resume/i })).not.toBeNull();
    expect(screen.queryByLabelText("Rename run")).not.toBeNull();
  });
});
