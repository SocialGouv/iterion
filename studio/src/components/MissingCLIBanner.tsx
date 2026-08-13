import { useEffect, useState } from "react";

import { Button } from "@/components/ui";
import { InlineBanner } from "@/components/ui/InlineBanner";

import { desktop, type CLIStatus } from "@/lib/desktopBridge";

const DISMISS_KEY = "iterion-desktop:missing-cli-dismissed";

type DetectState =
  | { kind: "loading" }
  | { kind: "ok"; missing: CLIStatus[] }
  | { kind: "error" };

// Sticky banner shown when claude/codex/git are missing — or when the
// desktop bridge can't enumerate them at all. Dismissable per session via
// sessionStorage so it doesn't nag, but reappears on next launch.
export default function MissingCLIBanner() {
  const [state, setState] = useState<DetectState>({ kind: "loading" });
  const [dismissed, setDismissed] = useState<boolean>(() =>
    typeof sessionStorage !== "undefined" &&
    sessionStorage.getItem(DISMISS_KEY) === "1",
  );

  useEffect(() => {
    let cancelled = false;
    desktop
      .detectExternalCLIs(false)
      .then((all) => {
        // Codex is an explicit-only backend and is absent from the default
        // auto-detection order. A missing codex binary therefore should not
        // nag users who did not select it; Welcome > CliCheck still surfaces
        // it for operators who opt in.
        if (!cancelled) {
          setState({
            kind: "ok",
            missing: all.filter((s) => !s.found && s.name !== "codex"),
          });
        }
      })
      .catch(() => {
        // Bridge/IPC failure: do NOT collapse to "all installed" — that hid
        // a real desktop-bridge outage behind a green-looking app. Surface
        // it as an explicit unknown-status warning instead.
        if (!cancelled) setState({ kind: "error" });
      });
    return () => {
      cancelled = true;
    };
  }, []);

  const dismiss = () => {
    sessionStorage.setItem(DISMISS_KEY, "1");
    setDismissed(true);
  };

  if (dismissed || state.kind === "loading") return null;

  if (state.kind === "error") {
    return (
      <InlineBanner tone="warning" dismissable onDismiss={dismiss}>
        CLI detection unavailable — install status unknown. The desktop bridge
        could not enumerate claude/git/codex; workflows may fail without
        warning.
      </InlineBanner>
    );
  }

  if (state.missing.length === 0) return null;
  const { missing } = state;

  return (
    <InlineBanner
      tone="warning"
      dismissable
      onDismiss={dismiss}
      action={
        <span className="flex items-center gap-2">
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
        </span>
      }
    >
      {missing.map((m) => m.name).join(", ")}{" "}
      {missing.length === 1 ? "is" : "are"} not installed — workflows that depend
      on {missing.length === 1 ? "it" : "them"} will fail.
    </InlineBanner>
  );
}
