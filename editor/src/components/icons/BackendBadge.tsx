// Visual badge for the execution backend (claw / claude_code / codex).
// Uses brand SVGs for delegates that have one (claude_code → Claude,
// codex → Codex) and a crab emoji for claw, iterion's in-process
// backend. Empty/undefined backend renders as claw because that's the
// runtime default — see pkg/backend/model/executor.go:resolveBackendName,
// which falls back to BackendClaw when no backend is set.

import Claude from "@lobehub/icons/es/Claude";
import Codex from "@lobehub/icons/es/Codex";

import { backendEmoji } from "./backendEmoji";

interface Props {
  // Raw backend value from the .iter source. Empty/undefined → "claw".
  backend?: string;
  // resolved is the backend name the runtime would actually pick at run
  // time when `backend` is empty. When supplied AND backend is empty,
  // the badge renders as "auto → <resolved>" to surface the live
  // detect-based selection from /api/backends/detect.
  resolved?: string;
  size?: number;
  showLabel?: boolean;
  className?: string;
}

// Resolve the displayed backend name. The runtime treats an empty value
// as the workflow default (which itself defaults to "claw"), so the
// editor surfaces the same fact rather than the legacy "direct" label.
export function effectiveBackend(backend?: string, resolved?: string): string {
  if (backend && backend !== "") return backend;
  if (resolved && resolved !== "") return resolved;
  return "claw";
}

export function BackendBadge({
  backend,
  resolved,
  size = 10,
  showLabel = true,
  className,
}: Props) {
  const effective = effectiveBackend(backend, resolved);
  const isImplicit = !backend || backend === "";
  const usingDetect = isImplicit && resolved && resolved !== "";

  let icon;
  if (effective === "claude_code") {
    icon = <Claude.Color size={size} className="shrink-0" />;
  } else if (effective === "codex") {
    icon = <Codex.Color size={size} className="shrink-0" />;
  } else {
    icon = (
      <span
        aria-hidden
        className="shrink-0 leading-none"
        style={{ fontSize: `${size}px` }}
      >
        {backendEmoji(effective)}
      </span>
    );
  }

  let title: string;
  if (!isImplicit) {
    title = `backend: ${effective}`;
  } else if (usingDetect) {
    title = `backend: auto → ${effective} (detected from your environment)`;
  } else {
    title = "backend: claw (runtime default for unset backend)";
  }

  return (
    <span
      className={`inline-flex items-center gap-0.5 ${className ?? ""}`}
      title={title}
    >
      {icon}
      {showLabel && (
        <span className="truncate">
          {effective}
          {isImplicit && (
            <span className="ml-0.5 text-fg-subtle/70 text-[8px]">
              {usingDetect ? "·auto" : "·default"}
            </span>
          )}
        </span>
      )}
    </span>
  );
}
