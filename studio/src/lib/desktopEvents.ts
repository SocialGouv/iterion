// Event names emitted by cmd/iterion-desktop and consumed by the SPA via
// window.runtime.EventsOn. Keep these in lockstep with
// cmd/iterion-desktop/events.go.

export const DesktopEvent = {
  ProjectSwitched: "project:switched",
  // Fires on every config mutation (add / remove / switch). Useful
  // for refreshing list views without depending on the current
  // project pointer also flipping.
  ProjectsChanged: "projects:changed",

  MenuSettings: "menu:settings",
  MenuSwitchProject: "menu:switch-project",
  MenuNewProject: "menu:new-project",
  MenuAbout: "menu:about",
  MenuUndo: "menu:undo",
  MenuRedo: "menu:redo",

  UpdateAvailable: "update:available",
  UpdateApplied: "update:applied",
  UpdateProgress: "update:progress",
  UpdateNone: "update:none",
  UpdateError: "update:error",

  // Run-health alert (stall / budget / failure) forwarded from the
  // embedded server's alert Manager. The SPA surfaces it as a native OS
  // notification; the in-page toast + dot ride the run WS separately.
  RunAlert: "run:alert",

  // The active cloud connection's refresh token was rejected (silent
  // refresh failed / server revoked). Payload: the connection id. The
  // shell prompts the operator to log back in.
  CloudAuthExpired: "cloud:auth-expired",
} as const;

export type DesktopEventName = (typeof DesktopEvent)[keyof typeof DesktopEvent];
