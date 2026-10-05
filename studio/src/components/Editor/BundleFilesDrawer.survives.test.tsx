// @vitest-environment jsdom
//
// #1755: the typed buffer lives in `store/editBuffers.ts`, not in this
// component, so the ancestor renders that used to destroy it — the toolbar
// dropping the drawer with the `botsource://` path, a viewport notice, the
// tab host swapping the view, a route change — take nothing. What is under
// test is exactly that: an unmount MID-EDIT keeps the text (and the app-wide
// unload answer for it), and the next mount adopts it back.
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

const botSources = vi.hoisted(() => ({
  getBotSource: vi.fn(),
  putBotSourceFile: vi.fn(),
  deleteBotSourceFile: vi.fn(),
}));
vi.mock("@/api/botSources", () => botSources);

vi.mock("@/lib/monaco", () => ({
  default: ({ value, onChange }: { value?: string; onChange?: (v?: string) => void }) => (
    <textarea
      aria-label="file"
      value={value ?? ""}
      onChange={(e) => onChange?.(e.target.value)}
    />
  ),
}));

import BundleFilesDrawer from "./BundleFilesDrawer";
import { bundleBufferKey, useEditBuffersStore } from "@/store/editBuffers";
import { anyUnsavedBuffer } from "@/lib/unsavedBuffers";

afterEach(() => {
  cleanup();
  useEditBuffersStore.setState({ bundle: {}, runFiles: {} });
  vi.clearAllMocks();
});

const BUNDLE = {
  id: "b1",
  slug: "demo",
  version: 7,
  files: {
    "main.bot": "workflow w:\n  entry: a\n",
    "skills/notes.md": "# notes\n",
  },
};

function openDrawer() {
  botSources.getBotSource.mockResolvedValue(BUNDLE);
  return render(
    <BundleFilesDrawer teamID="team-1" slug="demo" open onOpenChange={() => {}} />,
  );
}

describe("BundleFilesDrawer's buffer survives the drawer's unmount", () => {
  it("keeps text typed mid-edit, answers the unload warning for it, and adopts it back on the next mount", async () => {
    const view = openDrawer();
    fireEvent.click(await screen.findByRole("button", { name: "skills/notes.md" }));
    fireEvent.change(screen.getByLabelText("file"), { target: { value: "# mine\n" } });

    // The ancestor takes the drawer away — no gate can run, none needs to.
    view.unmount();

    // The text is in the store, keyed by the bundle, still dirty — the
    // browser-unload warning answers for it even though no surface is
    // mounted.
    const held = useEditBuffersStore.getState().bundle[bundleBufferKey("team-1", "demo")];
    expect(held).toMatchObject({ rel: "skills/notes.md", value: "# mine\n" });
    expect(held?.value).not.toBe(held?.original);
    expect(anyUnsavedBuffer()).toBe(true);

    // The next mount — the ancestor rendering the drawer again — shows the
    // author their text, not the stored file.
    openDrawer();
    const buffer = (await screen.findByLabelText("file")) as HTMLTextAreaElement;
    await waitFor(() => expect(buffer.value).toBe("# mine\n"));

    // And it is still WORK the gates must ask about: leaving asks before
    // taking it.
    fireEvent.click(screen.getByRole("button", { name: "Back" }));
    expect(await screen.findByText("Discard this text?")).toBeTruthy();
  });

  it("releases a clean buffer on unmount, so a reopened drawer starts from the list", async () => {
    const view = openDrawer();
    fireEvent.click(await screen.findByRole("button", { name: "skills/notes.md" }));
    expect(screen.getByLabelText("file")).toBeTruthy();

    view.unmount();

    expect(useEditBuffersStore.getState().bundle[bundleBufferKey("team-1", "demo")]).toBeUndefined();
    expect(anyUnsavedBuffer()).toBe(false);

    openDrawer();
    await screen.findByRole("button", { name: "skills/notes.md" });
    expect(screen.queryByLabelText("file")).toBeNull();
  });

  it("keeps two bundles' buffers apart, adopting each on its own drawer", async () => {
    botSources.getBotSource.mockResolvedValue(BUNDLE);
    const view = render(
      <BundleFilesDrawer teamID="team-1" slug="demo" open onOpenChange={() => {}} />,
    );
    fireEvent.click(await screen.findByRole("button", { name: "skills/notes.md" }));
    fireEvent.change(screen.getByLabelText("file"), { target: { value: "meant for demo\n" } });
    view.unmount();

    const other = { ...BUNDLE, slug: "other", files: { "main.bot": "workflow o:\n" } };
    botSources.getBotSource.mockResolvedValue(other);
    render(<BundleFilesDrawer teamID="team-1" slug="other" open onOpenChange={() => {}} />);
    // `other` holds nothing: its list, not demo's editor.
    await screen.findByTitle("Open the workflow in the Canvas editor");
    expect(screen.queryByLabelText("file")).toBeNull();

    botSources.getBotSource.mockResolvedValue(BUNDLE);
    render(<BundleFilesDrawer teamID="team-1" slug="demo" open onOpenChange={() => {}} />);
    await waitFor(() =>
      expect((screen.getByLabelText("file") as HTMLTextAreaElement).value).toBe(
        "meant for demo\n",
      ),
    );
  });
});
