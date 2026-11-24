// Which run a request URL addresses: only the first `runs` segment of the
// run route families counts, and the static routes of those families address
// no run — whatever a caller named a run.
import { afterEach, describe, expect, it } from "vitest";

import { forgetLostRunsForTest, lostRunAddressedBy, markRunLost, runAddressedBy } from "./lostRuns";

afterEach(() => forgetLostRunsForTest());

describe("the run a URL addresses", () => {
  it("is the first run of the REST and socket families, behind any prefix", () => {
    expect(runAddressedBy("/api/runs/run-x")).toBe("run-x");
    expect(runAddressedBy("/api/runs/run-x/log?from=0")).toBe("run-x");
    expect(runAddressedBy("/x/pane-1/api/runs/run-x/events")).toBe("run-x");
    expect(runAddressedBy("http://other.host/x/p1/api/ws/runs/run-x/shell?cols=1")).toBe("run-x");
    expect(runAddressedBy("/api/runs/run%2Dx")).toBe("run-x");
    expect(runAddressedBy("/api/runs/foo/../run-x")).toBe("run-x");
  });

  it("is not a run-like path inside another run's data", () => {
    markRunLost("", "run-x");
    for (const url of [
      "/api/runs/run-y/artifact-files/runs/run-x/report.md",
      "/api/runs/run-y/workspace-files/.iterion/runs/run-x/run.json",
      "/api/runs/run-y/files/content?path=.iterion/runs/run-x/run.json",
    ]) {
      expect(runAddressedBy(url)).toBe("run-y");
      expect(lostRunAddressedBy(url)).toBeNull();
    }
  });

  it("is no run on a static route, even one named like it", () => {
    for (const name of ["stats", "global-active", "preview-cost", "uploads"]) {
      markRunLost("", name);
      expect(lostRunAddressedBy(`/api/runs/${name}`)).toBeNull();
      // Below the static segment, the router reads it as a run ID again.
      expect(lostRunAddressedBy(`/api/runs/${name}/events`)).toBe(name);
    }
  });

  it("is no run outside the run families", () => {
    markRunLost("", "run-x");
    for (const url of ["/runs/run-x", "/api/admin/runs", "/api/teams/t/config-editor/shares/s/runs", "/api/runs//run-x"]) {
      expect(lostRunAddressedBy(url)).toBeNull();
    }
  });
});
