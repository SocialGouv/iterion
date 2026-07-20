import { useRuns } from "@/hooks/useRuns";
import { useActiveRepo } from "@/hooks/useActiveRepo";
import CostCapBanner from "@/components/shared/CostCapBanner";
import WhatsNextCard from "./WhatsNextCard";
import HubGrid from "./HubGrid";
import RecentFilesPanel from "./RecentFilesPanel";
import CloudReposPanel from "./CloudReposPanel";
import GettingStartedCard from "./GettingStartedCard";
import RunsPanel from "./RunsPanel";

export default function HomeView() {
  // Repo-first scope: in cloud the Home runs list follows the active
  // repo (like /runs does); the hook is inert outside cloud mode.
  const { activeRepo, overview, repos, enabled: repoScope, choose } =
    useActiveRepo();
  const scopeRepoName =
    repoScope && !overview ? activeRepo?.repo_full_name ?? "" : "";
  const { runs, loading, error } = useRuns({ limit: 50, repo: scopeRepoName });

  // Nexie session signal for the hero card, derived from the runs the
  // page already fetches — no extra request.
  const nexieLive = runs.find(
    (r) =>
      (r.workflow_name === "whats_next" || r.workflow_name === "whats-next") &&
      (r.status === "queued" ||
        r.status === "running" ||
        r.status === "paused_waiting_human"),
  );

  return (
    <div className="h-full overflow-auto p-4 sm:p-6">
      {/* Daily spend-cap banner sits above the page content so it's the
          first thing the operator sees when runs are paused on budget. */}
      <CostCapBanner />
      <div className="max-w-6xl mx-auto space-y-4">
        {/* The cloud golden path: connect → enable → launch. Renders
            only while a step is incomplete, above everything else so a
            fresh operator sees their next move first. */}
        {repoScope && <GettingStartedCard repos={repos} runs={runs} />}
        {/* WhatsNextCard is the curated entry point — full-width above
            the grid so it reads as "start here" rather than as one
            option among many. */}
        <WhatsNextCard liveStatus={nexieLive?.status ?? null} />
        {/* The hub: every surface the operator can reach, gated by the same
            server_info flags + role as the sidebar. Makes the root a place
            you can get anywhere from, and represents the veille correctly. */}
        <HubGrid />
        <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
          {/* Left slot: local mode gets the file-oriented picker; cloud
              gets the team's repositories (there is no server-side file
              tree to browse). RunsPanel folds cross-store running runs
              into one box either way. */}
          {repoScope ? (
            <CloudReposPanel
              repos={repos}
              activeRepo={activeRepo}
              choose={choose}
            />
          ) : (
            <RecentFilesPanel />
          )}
          <RunsPanel
            runs={runs}
            loading={loading}
            error={error}
            scope={scopeRepoName || null}
          />
        </div>
      </div>
    </div>
  );
}
