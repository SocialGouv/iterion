package main

// Event names emitted via wruntime.EventsEmit and consumed by the SPA.
// Keep in lockstep with editor/src/lib/desktopEvents.ts.
const (
	eventProjectSwitched = "project:switched"

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
