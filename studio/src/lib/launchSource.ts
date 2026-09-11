import type { ServerInfo } from "@/api/types";

type WorkspaceInfo = Pick<ServerInfo, "mode" | "work_dir">;

function normalizePath(value: string): string | null {
  const normalized = value.trim().replace(/\\/g, "/");
  if (!normalized) return null;

  const absolute = normalized.startsWith("/");
  const parts: string[] = [];
  for (const part of normalized.split("/")) {
    if (!part || part === ".") continue;
    if (part === "..") {
      if (parts.length > 0 && parts[parts.length - 1] !== "..") {
        parts.pop();
      } else if (!absolute) {
        parts.push(part);
      }
      continue;
    }
    parts.push(part);
  }

  const body = parts.join("/");
  if (absolute) return `/${body}`;
  return body || ".";
}

function absolutePath(value: string, base?: string): string | null {
  const normalized = normalizePath(value);
  if (!normalized) return null;
  if (normalized.startsWith("/")) return normalized;
  const normalizedBase = base ? normalizePath(base) : null;
  if (!normalizedBase || !normalizedBase.startsWith("/")) return null;
  return normalizePath(`${normalizedBase}/${normalized}`);
}

function isStrictDescendant(target: string | null, root: string | null): boolean {
  if (!target || !root || target === root) return false;
  if (root === "/") return target.startsWith("/");
  return target.startsWith(`${root}/`);
}

/**
 * Client-side preflight for assistant-supplied resume paths. The server is
 * still authoritative; this check keeps an unsafe path out of the action
 * request and makes the host boundary explicit in the UI.
 */
export function isPathWithinWorkDir(
  filePath: string,
  workDir: string | null | undefined,
): boolean {
  const root = absolutePath(workDir ?? "");
  const target = absolutePath(filePath, workDir ?? undefined);
  return isStrictDescendant(target, root);
}

export interface LaunchSourceInput {
  filePath: string;
  source: string | null | undefined;
  confirmedDiskPath?: string | null;
  serverInfo?: WorkspaceInfo | null;
}

/**
 * Omit inline source only after /files/open has confirmed the same local
 * on-disk file that the launch form is submitting. Every uncertain case
 * remains inline, which is safe for unsaved and cloud workflows.
 */
export function canLaunchFromConfirmedDisk({
  filePath,
  confirmedDiskPath,
  serverInfo,
}: Omit<LaunchSourceInput, "source">): boolean {
  if (!filePath || !confirmedDiskPath || serverInfo?.mode !== "local") {
    return false;
  }
  const root = absolutePath(serverInfo.work_dir ?? "");
  const requested = absolutePath(filePath, serverInfo.work_dir ?? undefined);
  const confirmed = absolutePath(confirmedDiskPath, serverInfo.work_dir ?? undefined);
  return (
    isStrictDescendant(requested, root) &&
    isStrictDescendant(confirmed, root) &&
    requested === confirmed
  );
}

export function sourceForLaunch({
  source,
  ...input
}: LaunchSourceInput): string | undefined {
  if (canLaunchFromConfirmedDisk(input)) return undefined;
  return source || undefined;
}
