import { beforeEach, describe, expect, it, vi, type Mock } from "vitest";

vi.mock("@/api/bots", () => ({
  listBots: vi.fn(),
  setBotOverlay: vi.fn(),
  updateBot: vi.fn(),
}));

import { listBots, type BotEntryWithSchema } from "@/api/bots";

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

describe("useBotsStore load sequencing", () => {
  beforeEach(() => {
    useBotsStore.setState({ bots: null, loading: false, error: null });
    vi.mocked(listBots).mockReset();
  });

  it("keeps the latest refetch when a stale in-flight fetch resolves after it", async () => {
    // Project A's initial fetch is slow; the project-switch refetch for B
    // is fast. The A response landing LAST must not clobber B's catalog.
    const slow = deferred<BotEntryWithSchema[]>();
    const fast = deferred<BotEntryWithSchema[]>();
    (listBots as Mock)
      .mockReturnValueOnce(slow.promise)
      .mockReturnValueOnce(fast.promise);

    const p1 = useBotsStore.getState().fetch(); // project A (slow)
    const p2 = useBotsStore.getState().refetch(); // project switch → B (fast)

    fast.resolve([entry("beta")]);
    await p2;
    slow.resolve([entry("alpha")]);
    await p1;

    expect(useBotsStore.getState().bots?.map((b) => b.name)).toEqual(["beta"]);
    expect(useBotsStore.getState().loading).toBe(false);
  });

  it("a stale fetch error cannot mask the fresher catalog", async () => {
    const slow = deferred<BotEntryWithSchema[]>();
    const fast = deferred<BotEntryWithSchema[]>();
    (listBots as Mock)
      .mockReturnValueOnce(slow.promise)
      .mockReturnValueOnce(fast.promise);

    const p1 = useBotsStore.getState().fetch();
    const p2 = useBotsStore.getState().refetch();

    fast.resolve([entry("beta")]);
    await p2;
    slow.reject(new Error("old project went away"));
    await p1;

    expect(useBotsStore.getState().error).toBeNull();
    expect(useBotsStore.getState().bots?.map((b) => b.name)).toEqual(["beta"]);
  });
});
