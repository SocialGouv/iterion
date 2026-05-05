package main

// Event names emitted via wruntime.EventsEmit and consumed by the SPA.
// Keep in lockstep with editor/src/lib/desktopEvents.ts.
const (
	eventProjectSwitched = "project:switched"

	eventMenuSettings      = "menu:settings"
	eventMenuSwitchProject = "menu:switch-project"
	eventMenuOpenProject   = "menu:open-project"
	eventMenuNewProject    = "menu:new-project"
	eventMenuAbout         = "menu:about"

	eventUpdateAvailable = "update:available"
	eventUpdateApplied   = "update:applied"
	eventUpdateProgress  = "update:progress"
	eventUpdateNone      = "update:none"
	eventUpdateError     = "update:error"
)
