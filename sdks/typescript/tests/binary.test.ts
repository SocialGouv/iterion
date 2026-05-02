import { mkdir, mkdtemp, rm, writeFile, chmod } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, beforeEach, describe, expect, it } from "vitest";

import {
  detectPlatform,
  resolveBinary,
  IterionBinaryNotFoundError,
} from "../src/index.js";

describe("detectPlatform", () => {
  it("maps linux/x64 → linux/amd64", () => {
    expect(detectPlatform({ platform: "linux", arch: "x64" })).toEqual({
      os: "linux",
      arch: "amd64",
      ext: "",
    });
  });

  it("maps darwin/arm64 → darwin/arm64", () => {
    expect(detectPlatform({ platform: "darwin", arch: "arm64" })).toEqual({
      os: "darwin",
      arch: "arm64",
      ext: "",
    });
  });

  it("maps win32/x64 → windows/amd64 with .exe", () => {
    expect(detectPlatform({ platform: "win32", arch: "x64" })).toEqual({
      os: "windows",
      arch: "amd64",
      ext: ".exe",
    });
  });

  it("throws on unsupported OS", () => {
    expect(() =>
      detectPlatform({ platform: "freebsd" as NodeJS.Platform, arch: "x64" }),
    ).toThrow(IterionBinaryNotFoundError);
  });

  it("throws on unsupported arch", () => {
    expect(() =>
      detectPlatform({ platform: "linux", arch: "ia32" }),
    ).toThrow(IterionBinaryNotFoundError);
  });
});

describe("resolveBinary", () => {
  let dir: string;

  beforeEach(async () => {
    dir = await mkdtemp(join(tmpdir(), "iterion-sdk-bin-"));
  });

  afterEach(async () => {
    await rm(dir, { recursive: true, force: true });
  });

  async function makeExecutable(path: string): Promise<void> {
    await writeFile(path, "#!/bin/sh\necho hi\n");
    await chmod(path, 0o755);
  }

  it("uses explicit binPath when given", async () => {
    const explicit = join(dir, "explicit");
    await makeExecutable(explicit);
    const got = await resolveBinary({
      binPath: explicit,
      env: { PATH: "" },
      platform: { os: "linux", arch: "amd64", ext: "" },
      packageBinDir: join(dir, "nope"),
    });
    expect(got).toBe(explicit);
  });

  it("falls back to ITERION_BIN env var", async () => {
    const fromEnv = join(dir, "from-env");
    await makeExecutable(fromEnv);
    const got = await resolveBinary({
      env: { ITERION_BIN: fromEnv, PATH: "" },
      platform: { os: "linux", arch: "amd64", ext: "" },
      packageBinDir: join(dir, "nope"),
    });
    expect(got).toBe(fromEnv);
  });

  it("then tries package-local <package>/bin/iterion", async () => {
    const pkgBin = join(dir, "pkgbin");
    await mkdir(pkgBin, { recursive: true });
    const candidate = join(pkgBin, "iterion");
    await writeFile(candidate, "");
    await chmod(candidate, 0o755);
    const got = await resolveBinary({
      env: { PATH: "" },
      platform: { os: "linux", arch: "amd64", ext: "" },
      packageBinDir: pkgBin,
    });
    expect(got).toBe(candidate);
  });

  it("falls back to PATH lookup", async () => {
    const pathDir = join(dir, "pathdir");
    await mkdir(pathDir, { recursive: true });
    const candidate = join(pathDir, "iterion");
    await writeFile(candidate, "");
    await chmod(candidate, 0o755);
    const got = await resolveBinary({
      env: { PATH: pathDir },
      platform: { os: "linux", arch: "amd64", ext: "" },
      packageBinDir: join(dir, "nope"),
    });
    expect(got).toBe(candidate);
  });

  it("throws IterionBinaryNotFoundError with searched paths", async () => {
    await expect(
      resolveBinary({
        env: { PATH: "" },
        platform: { os: "linux", arch: "amd64", ext: "" },
        packageBinDir: join(dir, "nope"),
      }),
    ).rejects.toBeInstanceOf(IterionBinaryNotFoundError);
  });

  it("respects explicit binPath precedence over ITERION_BIN", async () => {
    const explicit = join(dir, "explicit");
    const fromEnv = join(dir, "from-env");
    await makeExecutable(explicit);
    await makeExecutable(fromEnv);
    const got = await resolveBinary({
      binPath: explicit,
      env: { ITERION_BIN: fromEnv, PATH: "" },
      platform: { os: "linux", arch: "amd64", ext: "" },
      packageBinDir: join(dir, "nope"),
    });
    expect(got).toBe(explicit);
  });
});
