// Which sidebar row the current location belongs to. Its own module because
// NavLinks must export components only (react-refresh), and because the
// mapping is the kind of thing a test should be able to ask directly.

import { ADMIN_BASE } from "@/views/admin/adminRoutes";

export type Section =
  | "home"
  | "whatsNext"
  | "editor"
  | "runs"
  | "bots"
  | "board"
  | "pipelines"
  | "dispatcher"
  | "triggers"
  | "marketplace"
  | "plugins"
  | "secrets"
  | "skills"
  | "configEditor"
  | "org"
  | "team"
  | "integrations"
  | "admin";

// deriveSection maps the current path to the highlighted nav entry by
// looking at the first path segment only — so `/runs/:id` highlights
// "runs" without `/boardroom` accidentally highlighting "board".
const SEGMENT_TO_SECTION: Record<string, Section> = {
  "whats-next": "whatsNext",
  editor: "editor",
  runs: "runs",
  // /insights is reached from the Runs list toolbar; highlight Runs
  // in the side nav so the operator's place stays legible even
  // though it's no longer a top-level entry.
  insights: "runs",
  bots: "bots",
  board: "board",
  pipelines: "pipelines",
  dispatcher: "dispatcher",
  triggers: "triggers",
  marketplace: "marketplace",
  plugins: "plugins",
  "config-editor": "configEditor",
  orgs: "org",
  teams: "team",
  integrations: "integrations",
  // The console's segment comes from the console's own base, so moving it
  // takes this highlight along instead of silently losing it.
  [ADMIN_BASE.slice(1)]: "admin",
};

export function deriveSection(pathname: string): Section | undefined {
  if (pathname === "/" || pathname === "") return "home";
  const segment = pathname.split("/")[1] ?? "";
  return SEGMENT_TO_SECTION[segment];
}
