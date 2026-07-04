package main

// Event names emitted via wruntime.EventsEmit and consumed by the SPA.
// Keep in lockstep with studio/src/lib/desktopEvents.ts.
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

	// eventRunAlert carries a run-health alert (stall / budget / failure)
	// from the embedded studio server's alert Manager to the SPA, which
	// surfaces it as a native OS notification. The payload is the flat map
	// produced by alert.Alert.AsEventData(). The in-page toast + dot are
	// driven separately off the run WS `alert` event; this channel adds
	// the out-of-window native notification desktop sessions expect.
	eventRunAlert = "run:alert"

	// eventCloudAuthExpired fires when the active cloud connection's refresh
	// token is rejected (silent refresh failed / server revoked it). The SPA
	// shell surfaces a re-login prompt. Payload: the connection id (string).
	eventCloudAuthExpired = "cloud:auth-expired"
)
