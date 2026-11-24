import type { ServerInfo } from "@/api/types";

type WorkspaceInfo = Pick<ServerInfo, "mode" | "work_dir">;

function normalizePath(value: string, preserveSpelling = false): string | null {
  const normalized = preserveSpelling ? value : value.trim().replace(/\\/g, "/");
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

export function absolutePath(value: string, base?: string, preserveSpelling = false): string | null {
  const normalized = normalizePath(value, preserveSpelling);
  if (!normalized) return null;
  if (normalized.startsWith("/")) return normalized;
  const normalizedBase = base ? normalizePath(base, preserveSpelling) : null;
  if (!normalizedBase || !normalizedBase.startsWith("/")) return null;
  return normalizePath(`${normalizedBase}/${normalized}`, preserveSpelling);
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

/**
 * A launch names its program one of three ways: the text itself inline; —
 * where the server can read the operator's filesystem — a workflow path; or a
 * bot the server resolves, by its id, pinned to the digest of the program
 * this form rendered (`namedBot`, the read of
 * GET /api/v1/bots/{name}/launch-source). With none of them, the request
 * carries an empty file_path and no source, and the server refuses it by name
 * ("file_path, source or bot_id is required") naming fields the form never
 * showed.
 *
 * A named bot is a program only WITH its digest: the id alone would launch
 * whatever the tier holds when the request lands, not what this form shows.
 *
 * A cloud file_path is not a program: the server pod has no operator
 * filesystem, so it infers a catalog bot id FROM the path and runs that
 * tier's own bytes — not the ones this form rendered its fields from — or
 * refuses by name when no bot answers ("cloud mode: source or a catalog
 * bot_id is required", pkg/server/runs_launch.go). Mode-aware like
 * canLaunchFromConfirmedDisk above, and permissive when the mode is still
 * unknown: the server stays authoritative.
 *
 * The launch form can be up before it has a program: the inline branch's text
 * is the RENDER of the editor's document (useLaunchDoc), one round trip
 * behind it, and a named bot's digest is one read behind its slug. Lives next
 * to sourceForLaunch so the gate and the payload read the same rule.
 */
export function launchCarriesProgram({
  filePath,
  source,
  serverInfo,
  namedBot,
}: Pick<LaunchSourceInput, "filePath" | "source" | "serverInfo"> & {
  /** Absent on a launch that names no bot; `null` on one that names a bot
   *  whose program has not been read yet — which carries no program, whatever
   *  text or path stands beside it. */
  namedBot?: NamedBotProgram | null;
}): boolean {
  if (namedBot !== undefined) return !!namedBot && !!namedBot.id && !!namedBot.sha256;
  if (source) return true;
  return !!filePath && serverInfo?.mode !== "cloud";
}

/** A bot the server resolves, pinned to the program a form rendered: the
 *  canonical id and the digest the server issued with that program. */
export interface NamedBotProgram {
  id: string;
  sha256: string;
}
