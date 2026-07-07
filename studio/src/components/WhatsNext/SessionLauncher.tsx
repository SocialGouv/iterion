import { useEffect, useState } from "react";

import type { FirstClassBot } from "@/lib/whats-next/firstClassBots";
import type { FormAnswer } from "@/lib/whats-next/questionForm";
import { Button, Input } from "@/components/ui";
import { WizardForm } from "@/components/ui/WizardForm";
import { useServerInfoStore } from "@/store/serverInfo";

interface Props {
  bot: FirstClassBot;
  // Called when the user submits the launcher (form or bare Start)
  // with the launch var map. When the bot declares a `seedVar`, the
  // launcher form's answer text is already folded into the vars under
  // that name (the bot reads it as the first operator message).
  onLaunch?: (params: { vars: Record<string, string> }) => void;
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

  const varsReady = bot.launcherVars.every(
    (v) => (vars[v.name] ?? "").trim() !== "",
  );
  const launch = (formAnswer?: FormAnswer) => {
    if (!onLaunch || busy || !varsReady) return;
    // Seed-var wiring: the launcher form is a single question whose
    // answer (a canned preset or the operator's own text) becomes the
    // bot's first message via vars[seedVar].
    const next = { ...vars };
    if (bot.seedVar && formAnswer) {
      const first = Object.values(formAnswer)[0];
      const text = Array.isArray(first) ? first.join(", ") : first ?? "";
      if (text.trim() !== "") next[bot.seedVar] = text.trim();
    }
    onLaunch({ vars: next });
  };

  return (
    <div className="max-w-2xl mx-auto py-8 px-4">
      <div className="rounded-[var(--radius-lg)] border border-border-default bg-surface-1 shadow-[var(--shadow-lg)] p-6 space-y-4">
        <div>
          <h2 className="text-display font-semibold tracking-tight text-fg-default">{bot.label}</h2>
          <p className="mt-1 text-label text-fg-muted">{bot.description}</p>
        </div>

        {bot.launcherVars.length > 0 && (
          <div className="space-y-3">
            {bot.launcherVars.map((v) => (
              <div key={v.name} className="space-y-1">
                <label className="text-micro uppercase tracking-wide text-fg-subtle">
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
                  <p className="text-caption text-fg-subtle">
                    Absolute path. Defaults to the studio&apos;s working directory.
                  </p>
                )}
              </div>
            ))}
          </div>
        )}

        {errorMessage && (
          <p className="text-body text-danger-fg" role="alert">
            {errorMessage}
          </p>
        )}

        {bot.launcherForm ? (
          <WizardForm
            spec={bot.launcherForm}
            busy={busy || !varsReady}
            onSubmit={(answer) => launch(answer)}
          />
        ) : (
          <div className="flex items-center justify-end gap-2 pt-2 border-t border-border-subtle">
            <Button
              variant="primary"
              size="md"
              disabled={busy || !varsReady}
              onClick={() => launch()}
            >
              {busy ? "Starting…" : "Start"}
            </Button>
          </div>
        )}

      </div>
    </div>
  );
}
