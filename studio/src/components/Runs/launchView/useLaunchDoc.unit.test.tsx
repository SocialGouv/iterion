// @vitest-environment jsdom
//
// /runs/new?file=X is the road the toolbar's Run navigates to, and the one
// the command palette, a repo's bot list, the bind wizard and a bot's home
// send the operator down with no editor involved. A bot in several files is
// launched from its files AS THEY ARE STORED, and this road reads them: the
// form must not be offered for files the launch will refuse once it is
// filled in (#1532).
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

vi.mock("@/api/client", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/api/client")>()),
  openFile: vi.fn(),
  unparse: vi.fn(),
}));

import * as client from "@/api/client";
import type { UnitInfo } from "@/api/types";
import { createEmptyDocument } from "@/lib/defaults";
import LaunchDocStoreProvider from "./LaunchDocStoreProvider";
import { useLaunchDoc } from "./useLaunchDoc";

const FRAGMENT_ERROR = "lib/nodes.bot:5:9: error [E002]: expected :, got String";

function answerWith(unit: UnitInfo) {
  vi.mocked(client.openFile).mockResolvedValue({
    source: 'import "lib/nodes.bot"\n',
    document: createEmptyDocument(),
    // What the open answers once auto-validation has not run yet; the
    // refusal may not depend on it — the gate's own fact is the unit's.
    diagnostics: [],
    path: "bots/x/main.bot",
    bindable: true,
    unit,
  });
}

const errors: string[] = [];

function Launch() {
  const { doc } = useLaunchDoc("bots/x/main.bot", (m) => {
    errors.push(m);
  });
  return <output aria-label="form">{doc ? "offered" : "none"}</output>;
}
const form = () => screen.getByLabelText("form").textContent;

beforeEach(() => {
  errors.length = 0;
  vi.mocked(client.openFile).mockReset();
});
afterEach(cleanup);

describe("the launch form of a bot in several files, by path", () => {
  it("is refused while a file of the unit does not parse, naming the file", async () => {
    answerWith({
      root: "bots/x",
      main: "main.bot",
      revision: "r1",
      files: [{ rel: "main.bot" }, { rel: "lib/nodes.bot", parse_errors: 2, parse_error: FRAGMENT_ERROR }],
    });
    render(
      <LaunchDocStoreProvider>
        <Launch />
      </LaunchDocStoreProvider>,
    );
    await waitFor(() => expect(errors).toHaveLength(1));
    expect(errors[0]).toContain("lib/nodes.bot");
    expect(errors[0]).toContain(FRAGMENT_ERROR);
    expect(form()).toBe("none");
  });

  it("is refused while the files do not load as one bot", async () => {
    const loadError = 'main.bot:1:1: error [E046]: import "lib/missing.bot": no such fragment (lib/missing.bot)';
    answerWith({
      root: "bots/x",
      main: "main.bot",
      revision: "r1",
      files: [{ rel: "main.bot" }],
      load_errors: 1,
      load_error: loadError,
    });
    render(
      <LaunchDocStoreProvider>
        <Launch />
      </LaunchDocStoreProvider>,
    );
    await waitFor(() => expect(errors).toHaveLength(1));
    expect(errors[0]).toContain(loadError);
    expect(form()).toBe("none");
  });

  it("is offered when every file parses and they load as one bot", async () => {
    answerWith({
      root: "bots/x",
      main: "main.bot",
      revision: "r1",
      files: [{ rel: "main.bot" }, { rel: "lib/nodes.bot" }],
    });
    render(
      <LaunchDocStoreProvider>
        <Launch />
      </LaunchDocStoreProvider>,
    );
    await waitFor(() => expect(form()).toBe("offered"));
    expect(errors).toEqual([]);
  });
});
