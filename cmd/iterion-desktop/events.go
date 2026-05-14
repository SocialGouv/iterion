package main

// Event names emitted via wruntime.EventsEmit and consumed by the SPA.
// Keep in lockstep with editor/src/lib/desktopEvents.ts.
const (
	eventProjectSwitched = "project:switched"
	// eventProjectsChanged fires whenever the persisted project list
	// mutates (add / remove / switch) — regardless of whether the
	// current project changed. The frontend listens on this to refresh
	// any list view (project switcher, settings panel) without having
	// to depend on the current-project pointer also flipping.
	eventProjectsChanged = "projects:changed"

	eventMenuSettings      = "menu:settings"
	eventMenuSwitchProject = "menu:switch-project"
	eventMenuNewProject    = "menu:new-project"
	eventMenuAbout         = "menu:about"
	eventMenuUndo          = "menu:undo"
	eventMenuRedo          = "menu:redo"

	eventUpdateAvailable = "update:available"
	eventUpdateApplied   = "update:applied"
	eventUpdateProgress  = "update:progress"
	eventUpdateNone      = "update:none"
	eventUpdateError     = "update:error"
)
