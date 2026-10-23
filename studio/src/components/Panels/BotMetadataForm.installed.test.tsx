// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { BotEntryWithSchema } from "@/api/bots";

import IdentityHeader from "@/views/Bots/BotHome/IdentityHeader";

import BotMetadataForm from "./BotMetadataForm";

// Mock only the two network calls the form can make; keep every other real
// export so transitive imports still resolve.
const updateBot = vi.fn();
const setBotOverlay = vi.fn();
vi.mock("@/api/bots", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/api/bots")>();
  return {
    ...actual,
    updateBot: (...args: unknown[]) => updateBot(...args),
    setBotOverlay: (...args: unknown[]) => setBotOverlay(...args),
  };
});

afterEach(() => {
  cleanup();
  updateBot.mockReset();
  setBotOverlay.mockReset();
});

const installed: BotEntryWithSchema = {
  name: "pkg-bot",
  description: "a package",
  path: "/work/.botz/pkg-bot",
  rel_path: ".botz/pkg-bot",
  is_bundle: true,
  installed: true,
  enabled: true,
  manifest_enabled: true,
};

// An installed package's manifest is the package's: an update replaces it, so
// the Bot tab never writes it (the server refuses the edit). Its visibility in
// this workspace is the overlay's — the toggle the update leaves alone.
describe("BotMetadataForm on an installed package", () => {
  it("disables the bot through the workspace overlay, never the manifest", async () => {
    setBotOverlay.mockResolvedValue({ ...installed, enabled: false });
    render(<BotMetadataForm bot={installed} />);
    expect(screen.queryByLabelText(/Persona name/)).toBeNull();
    fireEvent.click(screen.getByLabelText(/Active in this workspace/));
    await waitFor(() => expect(setBotOverlay).toHaveBeenCalledWith("pkg-bot", false));
    expect(updateBot).not.toHaveBeenCalled();
  });

  it("keeps the manifest form for an authored bundle", () => {
    render(<BotMetadataForm bot={{ ...installed, installed: undefined, path: "/work/bots/own-bot", name: "own-bot" }} />);
    expect(screen.getByLabelText(/Persona name/)).toBeTruthy();
    expect(screen.queryByLabelText(/Active in this workspace/)).toBeNull();
  });

  it("shows its icon in the bot header, never a picker that edits the manifest", () => {
    render(<IdentityHeader entry={installed} />);
    expect(screen.queryByTitle("Pick an emoji icon for this bot")).toBeNull();
    cleanup();
    render(<IdentityHeader entry={{ ...installed, installed: undefined, name: "own-bot" }} />);
    expect(screen.getByTitle("Pick an emoji icon for this bot")).toBeTruthy();
  });
});
