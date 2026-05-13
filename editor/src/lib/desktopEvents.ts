// Event names emitted by cmd/iterion-desktop and consumed by the SPA via
// window.runtime.EventsOn. Keep these in lockstep with
// cmd/iterion-desktop/events.go.

export const DesktopEvent = {
  ProjectSwitched: "project:switched",

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
} as const;

export type DesktopEventName = (typeof DesktopEvent)[keyof typeof DesktopEvent];
