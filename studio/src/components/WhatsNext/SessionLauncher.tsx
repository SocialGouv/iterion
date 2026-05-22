import { useEffect, useState } from "react";

import type { FirstClassBot } from "@/lib/whats-next/firstClassBots";
import { Button, Input } from "@/components/ui";
import { useServerInfoStore } from "@/store/serverInfo";

interface Props {
  bot: FirstClassBot;
  // Called when the user clicks "Start" with the form vars. Wired in
  // Étape 2 — for Étape 1, the WhatsNextView swaps to mock mode locally.
  onLaunch?: (vars: Record<string, string>) => void;
  busy?: boolean;
  errorMessage?: string | null;
}

export default function SessionLauncher({
  bot,
  onLaunch,
  busy = false,
  errorMessage = null,
}: Props) {
  const workDir = useServerInfoStore((s) => s.info?.work_dir ?? "");

  // Initialise each launcherVar from its defaultFrom rule (today only
  // `work_dir`). User can override before submitting.
  const [vars, setVars] = useState<Record<string, string>>(() => {
    const out: Record<string, string> = {};
    for (const v of bot.launcherVars) {
      out[v.name] = v.defaultFrom === "work_dir" ? workDir : "";
    }
    return out;
  });

  // Keep the work_dir defaults in sync if server info loads after
  // first render (initial fetch may be in-flight when the route
  // mounts).
  useEffect(() => {
    setVars((prev) => {
      const next = { ...prev };
      for (const v of bot.launcherVars) {
        if (v.defaultFrom === "work_dir" && !prev[v.name]) {
          next[v.name] = workDir;
        }
      }
      return next;
    });
  }, [bot.launcherVars, workDir]);

  const canLaunch =
    !busy &&
    bot.launcherVars.every((v) => (vars[v.name] ?? "").trim() !== "");

  const launch = () => {
    if (!onLaunch || !canLaunch) return;
    onLaunch(vars);
  };

  return (
    <div className="max-w-2xl mx-auto py-8 px-4">
      <div className="rounded-lg border border-border-default bg-surface-1 p-6 space-y-4">
        <div>
          <h2 className="text-lg font-semibold text-fg-default">{bot.label}</h2>
          <p className="mt-1 text-[13px] text-fg-muted">{bot.description}</p>
        </div>

        <div className="space-y-3">
          {bot.launcherVars.map((v) => (
            <div key={v.name} className="space-y-1">
              <label className="text-[11px] uppercase tracking-wide text-fg-subtle">
                {v.label}
              </label>
              <Input
                value={vars[v.name] ?? ""}
                onChange={(e) =>
                  setVars((prev) => ({ ...prev, [v.name]: e.target.value }))
                }
                placeholder={
                  v.defaultFrom === "work_dir" ? "Workspace directory" : ""
                }
                disabled={busy}
              />
              {v.defaultFrom === "work_dir" && (
                <p className="text-[10px] text-fg-subtle">
                  Absolute path. Defaults to the studio&apos;s working directory.
                </p>
              )}
            </div>
          ))}
        </div>

        {errorMessage && (
          <p className="text-[12px] text-danger-fg" role="alert">
            {errorMessage}
          </p>
        )}

        <div className="flex items-center justify-end gap-2 pt-2 border-t border-border-subtle">
          <Button
            variant="primary"
            size="md"
            disabled={!canLaunch}
            onClick={launch}
          >
            {busy ? "Starting…" : "Start"}
          </Button>
        </div>
      </div>
    </div>
  );
}
