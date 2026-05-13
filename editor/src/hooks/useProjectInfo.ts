import { useDesktop } from "@/hooks/useDesktop";
import { useServerInfoStore } from "@/store/serverInfo";

export interface ProjectInfo {
  name: string | null;
  dir: string | null;
  source: "desktop" | "server" | null;
}

/**
 * useProjectInfo resolves the "currently selected folder/project" for
 * display in the editor chrome (Toolbar + RunHeader) and the document
 * title. Two sources, in order of precedence:
 *
 *   1. Desktop (Wails): `currentProject` from useDesktop — the user
 *      switched projects via the desktop ProjectSwitcher.
 *   2. Server: `/api/server/info` exposes `work_dir` + `project_name`
 *      in local mode (browser `iterion editor --dir`).
 *
 * Cloud mode returns `{name: null, dir: null}` — no folder concept.
 */
export function useProjectInfo(): ProjectInfo {
  const { isDesktop, currentProject } = useDesktop();
  const serverInfo = useServerInfoStore((s) => s.info);

  if (isDesktop && currentProject) {
    return {
      name: currentProject.name,
      dir: currentProject.dir,
      source: "desktop",
    };
  }
  if (serverInfo?.project_name) {
    return {
      name: serverInfo.project_name,
      dir: serverInfo.work_dir ?? null,
      source: "server",
    };
  }
  return { name: null, dir: null, source: null };
}
