// @vitest-environment jsdom
import { act, cleanup, renderHook, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { RunVeille } from "@/api/runs";
import { useRunVeille } from "./useRunVeille";

const api = vi.hoisted(() => ({ listRunVeille: vi.fn(), stopAssistantRunWatch: vi.fn(), removeWatch: vi.fn() }));
const store = vi.hoisted(() => ({ doorbell: 0 }));
vi.mock("@/api/runs", () => api);
vi.mock("@/store/run", () => ({ useRunStore: () => store.doorbell }));
function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (reason: Error) => void;
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}
function veille(run: string): RunVeille {
  return { watched_issue_ids: ["issue-" + run], run_watches: [{
    id: "watch-" + run, target_run_id: "target-" + run, assistant_run_id: run,
    state: "active", mode: "diagnose", delivered_episodes: 0, cooldown_seconds: 0, created_at: "2026-09-13T00:00:00Z",
  }] };
}
beforeEach(() => { vi.resetAllMocks(); store.doorbell = 0; api.stopAssistantRunWatch.mockResolvedValue(undefined); api.removeWatch.mockResolvedValue(undefined); });
afterEach(cleanup);

describe("useRunVeille ownership", () => {
  it.each(["success", "failure"])("ignores stale %s after changing run", async (completion) => {
    const old = deferred<RunVeille>();
    api.listRunVeille.mockImplementation((id: string) => id === "A" ? old.promise : Promise.resolve(veille("B")));
    const { result, rerender } = renderHook(({ id }) => useRunVeille(id), { initialProps: { id: "A" } });
    rerender({ id: "B" });
    expect(result.current.active).toBe(false);
    await waitFor(() => expect(result.current.runTargets).toEqual(["target-B"]));
    await act(async () => { if (completion === "success") old.resolve(veille("A")); else old.reject(new Error("old failure")); });
    expect(result.current.issueIds).toEqual(["issue-B"]);
    await act(() => result.current.stop());
    expect(api.stopAssistantRunWatch).toHaveBeenCalledWith("watch-B");
    expect(api.removeWatch).toHaveBeenCalledWith("B", "issue-B");
    expect(api.removeWatch).not.toHaveBeenCalledWith("B", "issue-A");
  });

  it("ignores an older refresh on the same run", async () => {
    const first = deferred<RunVeille>();
    api.listRunVeille.mockReturnValueOnce(first.promise).mockResolvedValue(veille("new"));
    const { result, rerender } = renderHook(() => useRunVeille("A"));
    store.doorbell++;
    rerender();
    await waitFor(() => expect(result.current.runTargets).toEqual(["target-new"]));
    await act(async () => { first.resolve(veille("old")); });
    expect(result.current.runTargets).toEqual(["target-new"]);
  });

  it("keeps an in-flight stop bound to its original owner", async () => {
    const stopped = deferred<void>();
    api.listRunVeille.mockImplementation((id: string) => Promise.resolve(veille(id)));
    api.stopAssistantRunWatch.mockReturnValueOnce(stopped.promise);
    const { result, rerender } = renderHook(({ id }) => useRunVeille(id), { initialProps: { id: "A" } });
    await waitFor(() => expect(result.current.active).toBe(true));
    let pending!: Promise<void>;
    act(() => { pending = result.current.stop(); });
    expect(result.current.busy).toBe(true);
    rerender({ id: "B" });
    await waitFor(() => expect(result.current.runTargets).toEqual(["target-B"]));
    expect(result.current.busy).toBe(false);
    await act(async () => { stopped.resolve(); await pending; });
    expect(api.removeWatch).toHaveBeenCalledWith("A", "issue-A");
    expect(result.current.runTargets).toEqual(["target-B"]);
    expect(result.current.error).toBeNull();
  });

  it("drops stop errors after unmount and does not refresh", async () => {
    const stopped = deferred<void>();
    api.listRunVeille.mockResolvedValue(veille("A"));
    api.stopAssistantRunWatch.mockReturnValue(stopped.promise);
    const { result, unmount } = renderHook(() => useRunVeille("A"));
    await waitFor(() => expect(result.current.active).toBe(true));
    let pending!: Promise<void>;
    act(() => { pending = result.current.stop(); });
    unmount();
    await act(async () => { stopped.reject(new Error("old stop")); await pending; });
    expect(api.listRunVeille).toHaveBeenCalledTimes(1);
  });
});
