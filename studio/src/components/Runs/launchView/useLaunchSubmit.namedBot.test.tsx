// @vitest-environment jsdom
//
// A launch that NAMES its bot posts the bot's id and the digest its read
// issued — and nothing of the text (#3416). The text is on the form all the
// same: it is what the fields were rendered from and what the cost preview
// prices. Posted beside the id it would make the launch unnamed work — the
// server reads an inline source as naming no bot (#1368) — so the payload is
// decided on the road, never on what happens to be at hand. And until the
// read lands the form has no program at all: a slug with no digest would
// launch whatever the tier holds when the request arrives.
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { useState } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const runsApi = vi.hoisted(() => ({ createRun: vi.fn() }));
vi.mock("@/api/runs", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/api/runs")>()),
  createRun: runsApi.createRun,
}));

import type { ServerInfo } from "@/api/types";
import { createEmptyDocument } from "@/lib/defaults";
import type { NamedBotProgram } from "@/lib/launchSource";

import { emptyBudgetFieldValues } from "./budgetPayload";
import type { UseRepoTargetResult } from "./useRepoTarget";
import type { UseRunOverridesResult } from "./useRunOverrides";
import { useLaunchSubmit } from "./useLaunchSubmit";

const noRepoTarget = { setRepoTargetCreateError: () => {} } as unknown as UseRepoTargetResult;
const noOverrides = {
  modelOverrides: {},
  budgetFields: emptyBudgetFieldValues,
} as unknown as UseRunOverridesResult;
const cloudInfo: ServerInfo = {
  mode: "cloud",
  version: "test",
  work_dir: "",
  limits: {
    upload: { max_file_size: 1, max_total_size: 1, max_files_per_run: 1, allowed_mime: [] },
  },
};
const text = "workflow nestor:\n  entry: done\n";

function Form({ namedBot }: { namedBot: NamedBotProgram | null }) {
  const [error, setError] = useState<string | null>(null);
  const { onSubmit, launchRun } = useLaunchSubmit({
    filePath: "",
    doc: createEmptyDocument(),
    // The program text is on the form whether or not the digest is.
    currentSource: text,
    namedBot,
    confirmedDiskPath: null,
    serverInfo: cloudInfo,
    fields: [],
    values: {},
    attachmentFields: [],
    attachments: {},
    selectedPreset: "",
    selectedPresetMeta: undefined,
    repoTarget: noRepoTarget,
    overrides: noOverrides,
    setError,
  });
  return (
    <>
      <button onClick={onSubmit}>Launch</button>
      <button onClick={() => void launchRun()}>Confirm</button>
      <output aria-label="error">{error ?? ""}</output>
    </>
  );
}

const submit = () => fireEvent.click(screen.getByRole("button", { name: "Launch" }));
const confirm = () => fireEvent.click(screen.getByRole("button", { name: "Confirm" }));
const error = () => screen.getByLabelText("error").textContent;

beforeEach(() => {
  runsApi.createRun.mockReset();
  runsApi.createRun.mockResolvedValue({ run_id: "run-1" });
});

afterEach(cleanup);

describe("a launch that names its bot", () => {
  it("posts the id and the digest, and none of the text on the form", () => {
    render(<Form namedBot={{ id: "nestor", sha256: "sha-issued" }} />);

    submit();

    expect(runsApi.createRun).toHaveBeenCalledTimes(1);
    const posted = runsApi.createRun.mock.calls[0]?.[0] as Record<string, unknown>;
    expect(posted).toMatchObject({ bot_id: "nestor", expect_source_sha256: "sha-issued" });
    expect(posted).not.toHaveProperty("source");
    expect(posted).not.toHaveProperty("file_path");
    expect(error()).toBe("");
  });

  it("sends no run before its program was read, whatever text is at hand", () => {
    render(<Form namedBot={null} />);

    submit();

    expect(runsApi.createRun).not.toHaveBeenCalled();
    expect(error()).toMatch(/still being prepared/);
  });

  // The no-sandbox dialog's Confirm reaches launchRun past onSubmit.
  it("sends no run through the no-sandbox dialog either", () => {
    render(<Form namedBot={null} />);

    confirm();

    expect(runsApi.createRun).not.toHaveBeenCalled();
    expect(error()).toMatch(/still being prepared/);
  });
});
