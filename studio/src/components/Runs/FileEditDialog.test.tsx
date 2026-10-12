// @vitest-environment jsdom
//
// #1755: the run file-edit dialog's buffer lives in `store/editBuffers.ts`,
// keyed by run and path. What is under test: the close THIS dialog triggers
// asks before taking dirty text (it had no gate at all), an unmount it does
// not control — leaving the run, the run view going away — takes nothing,
// and the buffer is adopted back when the same file is opened again.
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

const runs = vi.hoisted(() => ({
  getRunFileContent: vi.fn(),
  saveRunFileContent: vi.fn(),
}));
vi.mock("@/api/runs", () => runs);

vi.mock("@tanstack/react-query", () => ({
  useQueryClient: () => ({ invalidateQueries: vi.fn() }),
}));

vi.mock("@/lib/monaco", () => ({
  default: ({ value, onChange }: { value?: string; onChange?: (v?: string) => void }) => (
    <textarea
      aria-label="file"
      value={value ?? ""}
      onChange={(e) => onChange?.(e.target.value)}
    />
  ),
}));

import FileEditDialog from "./FileEditDialog";
import { runFileBufferKey, useEditBuffersStore } from "@/store/editBuffers";
import { anyUnsavedBuffer } from "@/lib/unsavedBuffers";

afterEach(() => {
  cleanup();
  useEditBuffersStore.setState({ bundle: {}, runFiles: {} });
  vi.clearAllMocks();
});

const FILE = { path: ".gitignore", content: "node_modules/\n", binary: false, exists: true };

function openDialog(path: string | null = ".gitignore", onClose: () => void = () => {}) {
  runs.getRunFileContent.mockResolvedValue(FILE);
  return render(
    <FileEditDialog runId="run-1" path={path} onClose={onClose} />,
  );
}

const bufferValue = () =>
  (screen.getByLabelText("file") as HTMLTextAreaElement).value;

describe("FileEditDialog writes", () => {
  it("saves what is typed, and the save marks the buffer clean", async () => {
    runs.saveRunFileContent.mockResolvedValue(undefined);
    openDialog();
    const buffer = await screen.findByLabelText("file");
    await waitFor(() => expect(bufferValue()).toBe("node_modules/\n"));
    fireEvent.change(buffer, { target: { value: "node_modules/\ndist/\n" } });
    expect(screen.getByRole("button", { name: "Save" })).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() =>
      expect(runs.saveRunFileContent).toHaveBeenCalledWith("run-1", ".gitignore", "node_modules/\ndist/\n"),
    );
    await waitFor(() => expect(screen.getByRole("button", { name: "Saved" })).toBeTruthy());
    // Clean: closing it now asks nothing.
    expect(anyUnsavedBuffer()).toBe(false);
  });
});

describe("FileEditDialog closing", () => {
  it("asks before Cancel takes typed text, and keeps it when the author declines", async () => {
    const onClose = vi.fn();
    openDialog(".gitignore", onClose);
    const buffer = await screen.findByLabelText("file");
    await waitFor(() => expect(bufferValue()).toBe("node_modules/\n"));
    fireEvent.change(buffer, { target: { value: "mine\n" } });

    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    // Two dialogs are up; the question's own Cancel is scoped inside it.
    const question = screen.getByRole("dialog", { name: "Discard this text?" });
    fireEvent.click(
      Array.from(question.querySelectorAll("button")).find((b) => b.textContent === "Cancel")!,
    );

    expect(onClose).not.toHaveBeenCalled();
    expect(bufferValue()).toBe("mine\n");
  });

  it("takes the text once the author confirms, and closes", async () => {
    const onClose = vi.fn();
    openDialog(".gitignore", onClose);
    const buffer = await screen.findByLabelText("file");
    await waitFor(() => expect(bufferValue()).toBe("node_modules/\n"));
    fireEvent.change(buffer, { target: { value: "mine\n" } });

    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    fireEvent.click(await screen.findByRole("button", { name: "Discard" }));

    await waitFor(() => expect(onClose).toHaveBeenCalledTimes(1));
    expect(useEditBuffersStore.getState().runFiles[runFileBufferKey("run-1", ".gitignore")]).toBeUndefined();
    expect(anyUnsavedBuffer()).toBe(false);
  });

  it("asks nothing when the buffer holds what was loaded", async () => {
    const onClose = vi.fn();
    openDialog(".gitignore", onClose);
    await screen.findByLabelText("file");
    await waitFor(() => expect(bufferValue()).toBe("node_modules/\n"));

    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(onClose).toHaveBeenCalledTimes(1));
    expect(screen.queryByRole("dialog", { name: "Discard this text?" })).toBeNull();
  });
});

describe("FileEditDialog's buffer survives an unmount it does not control", () => {
  it("keeps typed text when the run view unmounts, and adopts it when the file is opened again", async () => {
    const view = openDialog();
    const buffer = await screen.findByLabelText("file");
    await waitFor(() => expect(bufferValue()).toBe("node_modules/\n"));
    fireEvent.change(buffer, { target: { value: "dist/\n" } });

    // Leaving the run takes the whole dialog with it — no gate can run.
    view.unmount();
    expect(useEditBuffersStore.getState().runFiles[runFileBufferKey("run-1", ".gitignore")]).toEqual({
      value: "dist/\n",
      original: "node_modules/\n",
    });
    expect(anyUnsavedBuffer()).toBe(true);

    // Back on the run, opening the same file shows the author their text,
    // dirty against what they started from — although the file was re-read.
    openDialog();
    await waitFor(() => expect(bufferValue()).toBe("dist/\n"));
    expect(screen.getByRole("button", { name: "Save" })).toBeTruthy();
    expect(runs.getRunFileContent).toHaveBeenCalledTimes(2);
  });

  it("releases a clean buffer when the path moves on", async () => {
    const view = render(
      <FileEditDialog runId="run-1" path=".gitignore" onClose={() => {}} />,
    );
    await screen.findByLabelText("file");
    await waitFor(() => expect(bufferValue()).toBe("node_modules/\n"));

    // The parent resets the path without a gate — switching runs does — and
    // a clean buffer holds nothing.
    view.rerender(<FileEditDialog runId="run-1" path={null} onClose={() => {}} />);
    await waitFor(() =>
      expect(useEditBuffersStore.getState().runFiles[runFileBufferKey("run-1", ".gitignore")]).toBeUndefined(),
    );
  });

  it("keeps a DIRTY buffer when the path moves on, and the unload warning answers for it", async () => {
    const view = render(
      <FileEditDialog runId="run-1" path=".gitignore" onClose={() => {}} />,
    );
    const buffer = await screen.findByLabelText("file");
    await waitFor(() => expect(bufferValue()).toBe("node_modules/\n"));
    fireEvent.change(buffer, { target: { value: "mine\n" } });

    view.rerender(<FileEditDialog runId="run-1" path={null} onClose={() => {}} />);
    expect(useEditBuffersStore.getState().runFiles[runFileBufferKey("run-1", ".gitignore")]).toEqual({
      value: "mine\n",
      original: "node_modules/\n",
    });
    expect(anyUnsavedBuffer()).toBe(true);
  });
});
