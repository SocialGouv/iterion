import { describe, expect, it } from "vitest";
import { forgeLabel } from "./forge";

describe("forgeLabel", () => {
  it("says MR only for gitlab", () => {
    expect(forgeLabel("gitlab").noun).toBe("MR");
    expect(forgeLabel("GitLab").long).toBe("merge request");
  });

  it("defaults to PR for github, forgejo and unknown providers", () => {
    expect(forgeLabel("github").noun).toBe("PR");
    expect(forgeLabel("forgejo").noun).toBe("PR");
    expect(forgeLabel("gitea").long).toBe("pull request");
    expect(forgeLabel("").noun).toBe("PR");
    expect(forgeLabel(undefined).noun).toBe("PR");
    expect(forgeLabel(null).noun).toBe("PR");
  });
});
