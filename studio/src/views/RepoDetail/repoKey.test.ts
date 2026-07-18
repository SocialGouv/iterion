import { describe, expect, it } from "vitest";

import { decodeRepoKey, encodeRepoKey, repoDetailPath } from "./repoKey";

describe("repoKey codec", () => {
  it("round-trips a key with slashes and the :: separator", () => {
    const key = "conn-123::SocialGouv/iterion";
    expect(decodeRepoKey(encodeRepoKey(key))).toBe(key);
  });

  it("encodes slashes and colons so the key stays one path segment", () => {
    const enc = encodeRepoKey("c1::owner/repo");
    expect(enc).not.toContain("/");
    expect(enc).not.toContain(":");
    expect(enc).toBe("c1%3A%3Aowner%2Frepo");
  });

  it("round-trips nested group paths (gitlab subgroups)", () => {
    const key = "c9::group/sub-group/repo.name";
    expect(decodeRepoKey(encodeRepoKey(key))).toBe(key);
  });

  it("round-trips unicode and spaces", () => {
    const key = "c1::équipe/dépôt name";
    expect(decodeRepoKey(encodeRepoKey(key))).toBe(key);
  });

  it("returns a malformed param as-is instead of throwing", () => {
    expect(decodeRepoKey("bad%2")).toBe("bad%2");
  });

  it("builds the detail path from a repo row", () => {
    expect(
      repoDetailPath({ connection_id: "c1", repo_full_name: "owner/repo" }),
    ).toBe("/repos/c1%3A%3Aowner%2Frepo");
  });
});
