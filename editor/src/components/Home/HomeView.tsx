import { Link } from "wouter";
import { ListBulletIcon, Pencil2Icon } from "@radix-ui/react-icons";

import ProjectLabel from "@/components/shared/ProjectLabel";
import { useRuns } from "@/hooks/useRuns";
import RunningRunsBanner from "./RunningRunsBanner";
import RecentFilesPanel from "./RecentFilesPanel";
import RecentRunsPanel from "./RecentRunsPanel";

// One polled list of runs feeds the banner + recent panel — fans out
// to children as props so we don't run duplicate /api/runs timers.
export default function HomeView() {
  const { runs, loading, error } = useRuns({ limit: 50 });

  return (
    <div className="h-full flex flex-col overflow-hidden bg-surface-0 text-fg-default">
      <header className="border-b border-border-default px-4 py-3 flex items-center gap-3 bg-surface-1">
        <h1 className="text-sm font-bold">Iterion</h1>
        <ProjectLabel variant="header" />
        <div className="ml-auto flex items-center gap-2">
          <Link
            href="/editor"
            className="inline-flex items-center gap-1.5 text-xs px-2.5 py-1.5 rounded bg-surface-2 hover:bg-surface-3 border border-border-default"
          >
            <Pencil2Icon className="w-3.5 h-3.5" />
            Open editor
          </Link>
          <Link
            href="/runs"
            className="inline-flex items-center gap-1.5 text-xs px-2.5 py-1.5 rounded bg-surface-2 hover:bg-surface-3 border border-border-default"
          >
            <ListBulletIcon className="w-3.5 h-3.5" />
            Runs
          </Link>
        </div>
      </header>

      <RunningRunsBanner runs={runs} />

      <main className="flex-1 overflow-auto p-4 sm:p-6">
        <div className="max-w-6xl mx-auto grid grid-cols-1 md:grid-cols-2 gap-4">
          <RecentFilesPanel />
          <RecentRunsPanel runs={runs} loading={loading} error={error} />
        </div>
      </main>
    </div>
  );
}
