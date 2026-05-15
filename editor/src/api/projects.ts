// Project registry HTTP client. Mirrors pkg/server/projects.go.
//
// The same on-disk file is shared with the desktop (Wails) app. The
// frontend uses this client in browser/server mode; in desktop mode
// `useProjects` delegates to the Wails bridge (useDesktop.ts) so both
// surfaces stay consistent.

import { request } from "./client";

export interface Project {
  id: string;
  name: string;
  dir: string;
  store_dir?: string;
  last_opened: string; // ISO 8601 UTC
  color?: string;
}

export function listProjects(): Promise<Project[]> {
  return request<Project[]>("/projects");
}

export function getCurrentProject(): Promise<Project | null> {
  return request<Project | null>("/projects/current");
}

export function switchProject(id: string): Promise<Project> {
  return request<Project>("/projects/switch", {
    method: "POST",
    body: JSON.stringify({ id }),
  });
}

export function addProject(dir: string): Promise<Project> {
  return request<Project>("/projects", {
    method: "POST",
    body: JSON.stringify({ dir }),
  });
}

export function removeProject(id: string): Promise<void> {
  return request<void>(`/projects/${encodeURIComponent(id)}`, {
    method: "DELETE",
  });
}

export interface FilesystemEntry {
  name: string;
  abs_dir: string;
}

export interface FilesystemListing {
  cwd: string;
  root: string;
  entries: FilesystemEntry[];
}

export function listFilesystem(
  path: string,
  signal?: AbortSignal,
): Promise<FilesystemListing> {
  const qs = `?path=${encodeURIComponent(path)}`;
  return request<FilesystemListing>(`/filesystem/list${qs}`, { signal });
}
