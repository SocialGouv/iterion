import { useState } from "react";

import { Button } from "@/components/ui";

import { desktop } from "@/lib/desktopBridge";

interface Props {
  onNext: () => void;
}

interface OnboardingProjectDesktop {
  pickProjectDirectory: () => Promise<string>;
  scaffoldProject: (dir: string) => Promise<void>;
  addProjectSilently: (dir: string) => Promise<unknown>;
}

export async function selectOnboardingProject(
  bridge: OnboardingProjectDesktop,
  scaffold: boolean,
): Promise<boolean> {
  const dir = await bridge.pickProjectDirectory();
  if (!dir) return false;
  if (scaffold) await bridge.scaffoldProject(dir);
  await bridge.addProjectSilently(dir);
  return true;
}

export default function ProjectPicker({ onNext }: Props) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const run = async (scaffold: boolean) => {
    setError(null);
    setBusy(true);
    try {
      const selected = await selectOnboardingProject(desktop, scaffold);
      if (selected) onNext();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="max-w-xl flex flex-col gap-4">
      <h2 className="text-lg font-semibold">Where is your iterion project?</h2>
      <p className="text-fg-subtle text-sm">
        Iterion projects are folders containing one or more <code>.iter</code>{" "}
        workflow files. Pick an existing one or create a new project (this
        runs <code>iterion init</code> in the chosen folder).
      </p>
      <div className="flex gap-3">
        <Button onClick={() => run(false)} loading={busy} variant="primary">
          Pick existing folder…
        </Button>
        <Button onClick={() => run(true)} loading={busy}>
          Create new project…
        </Button>
      </div>
      {error && (
        <p className="text-danger text-sm" role="alert">
          {error}
        </p>
      )}
    </div>
  );
}
