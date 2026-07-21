import { errorMessage } from "@/lib/errorHints";
import { useState } from "react";

import { Button } from "@/components/ui";

import { desktop } from "@/lib/desktopBridge";

interface Props {
  onNext: () => void;
}

interface OnboardingProjectDesktop {
  pickProjectDirectory: () => Promise<string>;
  addProjectSilently: (dir: string) => Promise<unknown>;
}

export async function selectOnboardingProject(
  bridge: OnboardingProjectDesktop,
): Promise<boolean> {
  const dir = await bridge.pickProjectDirectory();
  if (!dir) return false;
  await bridge.addProjectSilently(dir);
  return true;
}

export default function ProjectPicker({ onNext }: Props) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const run = async () => {
    setError(null);
    setBusy(true);
    try {
      const selected = await selectOnboardingProject(desktop);
      if (selected) onNext();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="max-w-xl flex flex-col gap-4">
      <h2 className="text-lg font-semibold">
        Which folder should iterion work in?
      </h2>
      <p className="text-fg-subtle text-sm">
        Pick the repository or folder you want bots to work on — an empty one
        is fine. You&rsquo;ll choose what to do with it next: run a bot from
        the catalog, or create your own from the Bots view.
      </p>
      <div className="flex gap-3">
        <Button onClick={run} loading={busy} variant="primary">
          Choose folder…
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
