import { beforeEach, describe, expect, it, vi, type Mock } from "vitest";

vi.mock("@/api/bots", () => ({
  listBotsWithDiagnostics: vi.fn(),
  setBotOverlay: vi.fn(),
  updateBot: vi.fn(),
}));

import { listBotsWithDiagnostics, type BotEntryWithSchema } from "@/api/bots";

import { useBotsStore } from "./bots";

function deferred<T>() {
  let resolve!: (v: T) => void;
  let reject!: (e: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

const entry = (name: string): BotEntryWithSchema =>
  ({ name, path: `/x/${name}` }) as BotEntryWithSchema;

const catalog = (...names: string[]) => ({
  bots: names.map(entry),
  discoveryErrors: [],
});

describe("useBotsStore load sequencing", () => {
  beforeEach(() => {
    useBotsStore.setState({ bots: null, loading: false, error: null, discoveryErrors: [] });
    vi.mocked(listBotsWithDiagnostics).mockReset();
  });

  it("keeps the latest refetch when a stale in-flight fetch resolves after it", async () => {
    // Project A's initial fetch is slow; the project-switch refetch for B
    // is fast. The A response landing LAST must not clobber B's catalog.
    const slow = deferred<ReturnType<typeof catalog>>();
    const fast = deferred<ReturnType<typeof catalog>>();
    (listBotsWithDiagnostics as Mock)
      .mockReturnValueOnce(slow.promise)
      .mockReturnValueOnce(fast.promise);

    const p1 = useBotsStore.getState().fetch(); // project A (slow)
    const p2 = useBotsStore.getState().refetch(); // project switch → B (fast)

    fast.resolve(catalog("beta"));
    await p2;
    slow.resolve(catalog("alpha"));
    await p1;

    expect(useBotsStore.getState().bots?.map((b) => b.name)).toEqual(["beta"]);
    expect(useBotsStore.getState().loading).toBe(false);
  });

  it("a stale fetch error cannot mask the fresher catalog", async () => {
    const slow = deferred<ReturnType<typeof catalog>>();
    const fast = deferred<ReturnType<typeof catalog>>();
    (listBotsWithDiagnostics as Mock)
      .mockReturnValueOnce(slow.promise)
      .mockReturnValueOnce(fast.promise);

    const p1 = useBotsStore.getState().fetch();
    const p2 = useBotsStore.getState().refetch();

    fast.resolve(catalog("beta"));
    await p2;
    slow.reject(new Error("old project went away"));
    await p1;

    expect(useBotsStore.getState().error).toBeNull();
    expect(useBotsStore.getState().bots?.map((b) => b.name)).toEqual(["beta"]);
  });

  it("starts a new request for each refetch so the latest project wins", async () => {
    const oldProject = deferred<ReturnType<typeof catalog>>();
    const newProject = deferred<ReturnType<typeof catalog>>();
    (listBotsWithDiagnostics as Mock)
      .mockReturnValueOnce(oldProject.promise)
      .mockReturnValueOnce(newProject.promise);

    const oldLoad = useBotsStore.getState().refetch();
    const newLoad = useBotsStore.getState().refetch();
    expect(listBotsWithDiagnostics).toHaveBeenCalledTimes(2);

    newProject.resolve(catalog("beta"));
    await newLoad;
    oldProject.resolve(catalog("alpha"));
    await oldLoad;

    expect(useBotsStore.getState().bots?.map((b) => b.name)).toEqual(["beta"]);
  });

  it("keeps the discovery errors of a skipped malformed bundle", async () => {
    vi.mocked(listBotsWithDiagnostics).mockResolvedValue({
      bots: [entry("good")],
      discoveryErrors: [{ path: "bots/broken", error: "bundle: parse manifest: chat: ..." }],
    });

    await useBotsStore.getState().refetch();

    expect(useBotsStore.getState().discoveryErrors).toHaveLength(1);
    expect(useBotsStore.getState().discoveryErrors[0]?.path).toBe("bots/broken");
  });
});
