import { useEffect, useState } from "react";

import { Button, IconButton } from "@/components/ui";

import { desktop, type CLIStatus } from "@/lib/desktopBridge";

const DISMISS_KEY = "iterion-desktop:missing-cli-dismissed";

// Sticky banner shown when claude/codex/git are missing. Dismissable per
// session via sessionStorage so it doesn't nag, but reappears on next
// launch.
export default function MissingCLIBanner() {
  const [missing, setMissing] = useState<CLIStatus[] | null>(null);
  const [dismissed, setDismissed] = useState<boolean>(() =>
    typeof sessionStorage !== "undefined" &&
    sessionStorage.getItem(DISMISS_KEY) === "1",
  );

  useEffect(() => {
    let cancelled = false;
    desktop
      .detectExternalCLIs(false)
      .then((all) => {
        if (!cancelled) setMissing(all.filter((s) => !s.found));
      })
      .catch(() => {
        if (!cancelled) setMissing([]);
      });
    return () => {
      cancelled = true;
    };
  }, []);

  if (dismissed || !missing || missing.length === 0) return null;

  return (
    <div className="bg-warning-soft text-warning-fg flex items-center gap-3 px-4 py-2 text-sm">
      <span>
        {missing.map((m) => m.name).join(", ")}{" "}
        {missing.length === 1 ? "is" : "are"} not installed — workflows that
        depend on {missing.length === 1 ? "it" : "them"} will fail.
      </span>
      <span className="ml-auto flex items-center gap-2">
        {missing.map((m) => (
          <Button
            key={m.name}
            size="sm"
            variant="ghost"
            onClick={() => desktop.openExternal(m.install_url)}
          >
            Install {m.name}
          </Button>
        ))}
        <IconButton
          label="Dismiss"
          variant="ghost"
          size="sm"
          onClick={() => {
            sessionStorage.setItem(DISMISS_KEY, "1");
            setDismissed(true);
          }}
        >
          ✕
        </IconButton>
      </span>
    </div>
  );
}
