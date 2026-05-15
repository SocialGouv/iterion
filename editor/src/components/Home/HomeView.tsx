import ProjectLabel from "@/components/shared/ProjectLabel";
import NavLinks from "@/components/shared/NavLinks";
import { useRuns } from "@/hooks/useRuns";
import RecentFilesPanel from "./RecentFilesPanel";
import RunsPanel from "./RunsPanel";

export default function HomeView() {
  const { runs, loading, error } = useRuns({ limit: 50 });

  return (
    <div className="h-full flex flex-col overflow-hidden bg-surface-0 text-fg-default">
      <header className="border-b border-border-default px-4 py-2.5 flex items-center gap-3 bg-surface-1">
        <span className="text-sm font-bold tracking-wide">ITERION</span>
        <NavLinks active="home" />
        <ProjectLabel />
      </header>

      <main className="flex-1 overflow-auto p-4 sm:p-6">
        <div className="max-w-6xl mx-auto grid grid-cols-1 md:grid-cols-2 gap-4">
          {/* Both panels span one column each. Cross-store running
              runs are folded into RunsPanel's "In other locations"
              section so the operator sees everything in-flight in a
              single box. */}
          <RecentFilesPanel />
          <RunsPanel runs={runs} loading={loading} error={error} />
        </div>
      </main>
    </div>
  );
}
