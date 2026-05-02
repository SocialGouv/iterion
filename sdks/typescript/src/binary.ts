/**
 * Iterion binary discovery.
 *
 * Resolution order:
 *   1. explicit `binPath` argument
 *   2. `ITERION_BIN` environment variable
 *   3. `<sdk-package>/bin/iterion[.exe]` (slot for the optional auto-installer)
 *   4. `iterion[.exe]` on `PATH`
 *
 * Mirrors the platform detection in `docs/install.sh`.
 */

import { access, constants } from "node:fs/promises";
import { delimiter, dirname, isAbsolute, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { IterionBinaryNotFoundError } from "./errors.js";

export interface PlatformTarget {
  os: "linux" | "darwin" | "windows";
  arch: "amd64" | "arm64";
  ext: "" | ".exe";
}

export interface BinaryResolveOptions {
  /** Explicit path to the iterion binary. Takes precedence over everything else. */
  binPath?: string;
  /** Environment to read `ITERION_BIN` and `PATH` / `PATHEXT` from. Defaults to `process.env`. */
  env?: NodeJS.ProcessEnv;
  /** Override platform detection (mainly for tests). */
  platform?: PlatformTarget;
  /** Override the path used for "<sdk-package>/bin" lookup (mainly for tests). */
  packageBinDir?: string;
}

export interface PlatformDetectInput {
  platform: NodeJS.Platform;
  arch: string;
}

/** Detect the host platform/arch in the iterion release artifact format. */
export function detectPlatform(input?: PlatformDetectInput): PlatformTarget {
  const platform = input?.platform ?? process.platform;
  const arch = input?.arch ?? process.arch;

  let os: PlatformTarget["os"];
  switch (platform) {
    case "linux":
      os = "linux";
      break;
    case "darwin":
      os = "darwin";
      break;
    case "win32":
      os = "windows";
      break;
    default:
      throw new IterionBinaryNotFoundError(
        [],
        `unsupported OS: ${platform} (iterion ships linux/darwin/windows)`,
      );
  }

  let cpu: PlatformTarget["arch"];
  switch (arch) {
    case "x64":
      cpu = "amd64";
      break;
    case "arm64":
      cpu = "arm64";
      break;
    default:
      throw new IterionBinaryNotFoundError(
        [],
        `unsupported architecture: ${arch} (iterion ships amd64/arm64)`,
      );
  }

  return { os, arch: cpu, ext: os === "windows" ? ".exe" : "" };
}

async function isExecutable(path: string): Promise<boolean> {
  try {
    // F_OK + X_OK on POSIX. On Windows X_OK is approximated by F_OK
    // (Node's fs.access on win32 ignores the X bit).
    await access(path, constants.F_OK | constants.X_OK);
    return true;
  } catch {
    try {
      await access(path, constants.F_OK);
      return true; // Windows fallback when X_OK fails spuriously.
    } catch {
      return false;
    }
  }
}

/** Find the directory of *this* compiled module so we can locate `<package>/bin`. */
function defaultPackageBinDir(): string {
  // After tsc emits dist/binary.js, __dirname-equivalent = .../dist;
  // package root = .../, bin candidate = .../bin.
  try {
    const here = fileURLToPath(import.meta.url);
    return resolve(dirname(here), "..", "bin");
  } catch {
    return resolve(process.cwd(), "bin");
  }
}

/**
 * Locate the iterion binary, walking the resolution order documented at
 * the top of this file.
 */
export async function resolveBinary(
  opts: BinaryResolveOptions = {},
): Promise<string> {
  const env = opts.env ?? process.env;
  const platform = opts.platform ?? detectPlatform();
  const baseName = `iterion${platform.ext}`;
  const searched: string[] = [];

  // 1. Explicit path
  if (opts.binPath) {
    const candidate = isAbsolute(opts.binPath)
      ? opts.binPath
      : resolve(opts.binPath);
    searched.push(candidate);
    if (await isExecutable(candidate)) return candidate;
  }

  // 2. ITERION_BIN env var
  const fromEnv = env.ITERION_BIN;
  if (fromEnv && fromEnv.trim() !== "") {
    const candidate = isAbsolute(fromEnv) ? fromEnv : resolve(fromEnv);
    searched.push(candidate);
    if (await isExecutable(candidate)) return candidate;
  }

  // 3. Package-local bin (postinstall slot)
  const pkgBinDir = opts.packageBinDir ?? defaultPackageBinDir();
  const packageCandidate = join(pkgBinDir, baseName);
  searched.push(packageCandidate);
  if (await isExecutable(packageCandidate)) return packageCandidate;

  // 4. PATH lookup
  const pathEntries = (env.PATH ?? "").split(delimiter).filter(Boolean);
  const pathExts =
    platform.os === "windows"
      ? (env.PATHEXT ?? ".COM;.EXE;.BAT;.CMD")
          .split(";")
          .filter(Boolean)
          .map((s) => s.toLowerCase())
      : [""];
  for (const dir of pathEntries) {
    for (const ext of pathExts) {
      const candidate = join(dir, `iterion${ext}`);
      searched.push(candidate);
      if (await isExecutable(candidate)) return candidate;
    }
  }

  throw new IterionBinaryNotFoundError(searched);
}
